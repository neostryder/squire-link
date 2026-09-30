package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/zalando/go-keyring"
	"golang.org/x/term"
)

var version = "dev"

func main() {
	if err := runCLI(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, osKeyring{}); err != nil {
		fmt.Fprintln(os.Stderr, "squire-link:", err)
		os.Exit(1)
	}
}

func usage(out io.Writer) {
	fmt.Fprintln(out, "Usage: squire-link [run] [--mode local|serve] [--listen 127.0.0.1:8765] [--config PATH]")
	fmt.Fprintln(out, "       squire-link routes [--config PATH]")
	fmt.Fprintln(out, "       squire-link key set NAME | delete NAME | list [--config PATH]")
	fmt.Fprintln(out, "       squire-link token | version | --version | --help")
}

func flags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

func runCLI(args []string, stdin io.Reader, stdout, stderr io.Writer, keys secretStore) error {
	if len(args) > 0 && args[0] == "--version" {
		if len(args) != 1 {
			return errors.New("--version takes no arguments")
		}
		fmt.Fprintln(stdout, version)
		return nil
	}
	if len(args) > 0 && (args[0] == "--help" || args[0] == "-h" || args[0] == "help") {
		usage(stdout)
		return nil
	}
	command := "run"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command, args = args[0], args[1:]
	}
	switch command {
	case "run":
		fs := flags("run")
		mode := fs.String("mode", "local", "local or serve")
		listen := fs.String("listen", "127.0.0.1:8765", "loopback listen address")
		configFlag := fs.String("config", "", "config file")
		if err := fs.Parse(args); errors.Is(err, flag.ErrHelp) {
			usage(stdout)
			return nil
		} else if err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return fmt.Errorf("unexpected run arguments: %v", fs.Args())
		}
		if *mode != "local" && *mode != "serve" {
			return fmt.Errorf("mode must be local or serve")
		}
		if err := checkLoopback(*listen); err != nil {
			return err
		}
		path, err := configPath(*configFlag)
		if err != nil {
			return err
		}
		cfg, err := loadConfig(path)
		if err != nil {
			return err
		}
		listener, err := net.Listen("tcp", *listen)
		if err != nil {
			return fmt.Errorf("listen on %s: %w", *listen, err)
		}
		defer listener.Close()
		fmt.Fprintf(stdout, "Config: %s\nMode: %s\nListening: %s\n", path, *mode, listener.Addr())
		logger := log.New(stderr, "", 0)
		handler := newRelay(cfg, path, *mode, keys, logger)
		server := &http.Server{
			Handler:           handler,
			ReadHeaderTimeout: 5 * time.Second,
		}
		readers, stopReaders := context.WithCancel(context.Background())
		defer stopReaders()
		startChannel(readers, cfg.Channel, keys, handler.orders, logger)
		stop := make(chan os.Signal, 1)
		signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
		defer signal.Stop(stop)
		go func() {
			<-stop
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = server.Shutdown(ctx)
		}()
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve: %w", err)
		}
		return nil
	case "version":
		if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
			fmt.Fprintln(stdout, "Usage: squire-link version")
			return nil
		}
		if len(args) != 0 {
			return errors.New("version takes no arguments")
		}
		fmt.Fprintln(stdout, version)
		return nil
	case "token":
		if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
			fmt.Fprintln(stdout, "Usage: squire-link token")
			return nil
		}
		if len(args) != 0 {
			return errors.New("token takes no arguments")
		}
		secret, err := keys.Get("squire-link", "link-token")
		if errors.Is(err, keyring.ErrNotFound) {
			random := make([]byte, 32)
			if _, err := rand.Read(random); err != nil {
				return fmt.Errorf("generate token: %w", err)
			}
			secret = base64.RawURLEncoding.EncodeToString(random)
			if err := keys.Set("squire-link", "link-token", secret); err != nil {
				return fmt.Errorf("store token in OS keychain: %w", err)
			}
		} else if err != nil {
			return fmt.Errorf("read token from OS keychain: %w", err)
		}
		fmt.Fprintln(stdout, secret)
		return nil
	case "routes":
		fs := flags("routes")
		configFlag := fs.String("config", "", "config file")
		if err := fs.Parse(args); errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(stdout, "Usage: squire-link routes [--config PATH]")
			return nil
		} else if err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New("routes takes no positional arguments")
		}
		path, err := configPath(*configFlag)
		if err != nil {
			return err
		}
		cfg, err := loadConfig(path)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Config: %s\nDefault mode: local\n", path)
		for _, name := range routeNames(cfg.Routes) {
			r := cfg.Routes[name]
			keyState := "not required"
			if r.Key != "" {
				_, err := keys.Get("squire-link", r.Key)
				if errors.Is(err, keyring.ErrNotFound) {
					keyState = "not set"
				} else if err != nil {
					return fmt.Errorf("check key %q: %w", r.Key, err)
				} else {
					keyState = "set"
				}
			}
			fmt.Fprintf(stdout, "%s  %s  key: %s\n", name, r.Target, keyState)
		}
		return nil
	case "key":
		return keyCommand(args, stdin, stdout, stderr, keys)
	default:
		return fmt.Errorf("unknown command %q (use --help)", command)
	}
}

func keyCommand(args []string, stdin io.Reader, stdout, stderr io.Writer, keys secretStore) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintln(stdout, "Usage: squire-link key set NAME | delete NAME | list [--config PATH]")
		return nil
	}
	action, rest := args[0], args[1:]
	if len(rest) == 1 && (rest[0] == "--help" || rest[0] == "-h") {
		fmt.Fprintf(stdout, "Usage: squire-link key %s %s\n", action, map[string]string{"set": "NAME", "delete": "NAME", "list": "[--config PATH]"}[action])
		return nil
	}
	switch action {
	case "set", "delete":
		if len(rest) != 1 || !namePattern.MatchString(rest[0]) || rest[0] == "link-token" {
			return errors.New("key name must contain only letters, digits, _ or - and cannot be link-token")
		}
		if action == "delete" {
			if err := keys.Delete("squire-link", rest[0]); err != nil {
				return fmt.Errorf("delete key %q: %w", rest[0], err)
			}
			fmt.Fprintf(stdout, "Deleted key %s\n", rest[0])
			return nil
		}
		secret, err := readSecret(stdin, stderr)
		if err != nil {
			return err
		}
		if secret == "" {
			return errors.New("key cannot be empty")
		}
		if err := keys.Set("squire-link", rest[0], secret); err != nil {
			return fmt.Errorf("store key %q: %w", rest[0], err)
		}
		fmt.Fprintf(stdout, "Stored key %s (%d bytes)\n", rest[0], len(secret))
		return nil
	case "list":
		fs := flags("key list")
		configFlag := fs.String("config", "", "config file")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New("key list takes no positional arguments")
		}
		path, err := configPath(*configFlag)
		if err != nil {
			return err
		}
		cfg, err := loadConfig(path)
		if err != nil {
			return err
		}
		seen := make(map[string]bool)
		keyNames := []string{}
		for _, name := range routeNames(cfg.Routes) {
			keyNames = append(keyNames, cfg.Routes[name].Key)
		}
		if cfg.Channel.Discord.Enabled {
			keyNames = append(keyNames, discordKey)
		}
		for _, keyName := range keyNames {
			if keyName == "" || seen[keyName] {
				continue
			}
			seen[keyName] = true
			_, err := keys.Get("squire-link", keyName)
			if errors.Is(err, keyring.ErrNotFound) {
				fmt.Fprintf(stdout, "%s: not set\n", keyName)
			} else if err != nil {
				return fmt.Errorf("check key %q: %w", keyName, err)
			} else {
				fmt.Fprintf(stdout, "%s: set\n", keyName)
			}
		}
		return nil
	default:
		return fmt.Errorf("unknown key command %q", action)
	}
}

func readSecret(input io.Reader, stderr io.Writer) (string, error) {
	if file, ok := input.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
		fmt.Fprint(stderr, "Key: ")
		secret, err := term.ReadPassword(int(file.Fd()))
		fmt.Fprintln(stderr)
		if err != nil {
			return "", fmt.Errorf("read key: %w", err)
		}
		return string(secret), nil
	}
	line, err := bufio.NewReader(input).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read key: %w", err)
	}
	return strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r"), nil
}

func checkLoopback(address string) error {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("listen address must be a loopback IP and port: %w", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return errors.New("listen port must be between 1 and 65535")
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("listen address must use a loopback IP such as 127.0.0.1 or [::1]")
	}
	return nil
}
