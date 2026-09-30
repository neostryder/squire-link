package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net"
	"strings"
	"time"
)

const (
	twitchAddress = "irc.chat.twitch.tv:6697"
	// Twitch sends a PING about every five minutes, so a longer silence means
	// the connection is gone.
	twitchIdle     = 6 * time.Minute
	readerRetryMin = 5 * time.Second
	readerRetryMax = 2 * time.Minute
)

// twitchReader reads one Twitch channel's chat anonymously over IRC. The
// justinfan login needs no token and can only read.
type twitchReader struct {
	channel string
	dial    func(ctx context.Context) (net.Conn, error)
	take    func(user, text string)
	logger  *log.Logger
	retry   time.Duration
}

func newTwitchReader(channel string, take func(user, text string), logger *log.Logger) *twitchReader {
	return &twitchReader{
		channel: strings.ToLower(strings.TrimPrefix(channel, "#")),
		dial: func(ctx context.Context) (net.Conn, error) {
			dialer := &tls.Dialer{NetDialer: &net.Dialer{Timeout: 15 * time.Second}, Config: &tls.Config{ServerName: "irc.chat.twitch.tv", MinVersion: tls.VersionTLS12}}
			return dialer.DialContext(ctx, "tcp", twitchAddress)
		},
		take:   take,
		logger: logger,
		retry:  readerRetryMin,
	}
}

func (t *twitchReader) run(ctx context.Context) {
	wait := t.retry
	for {
		joined, err := t.session(ctx)
		if ctx.Err() != nil {
			return
		}
		if joined {
			wait = t.retry
		}
		t.logger.Printf("twitch: %v; trying again in %s", err, wait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, readerRetryMax)
	}
}

// session reads chat until the connection ends. It reports whether the
// channel was joined, so a working connection that later drops is retried
// at once rather than after a long wait.
func (t *twitchReader) session(ctx context.Context) (bool, error) {
	conn, err := t.dial(ctx)
	if err != nil {
		return false, fmt.Errorf("cannot connect: %w", err)
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
		case <-done:
		}
		conn.Close()
	}()
	if _, err := fmt.Fprintf(conn, "NICK justinfan%d\r\nJOIN #%s\r\n", 10000+rand.IntN(89999), t.channel); err != nil {
		return false, fmt.Errorf("cannot log in: %w", err)
	}
	joined := false
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 4096), 64<<10)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(twitchIdle))
		if !scanner.Scan() {
			err := scanner.Err()
			if err == nil {
				err = io.EOF
			}
			return joined, fmt.Errorf("connection closed: %w", err)
		}
		line := strings.TrimRight(scanner.Text(), "\r")
		if strings.HasPrefix(line, "@") {
			_, line, _ = strings.Cut(line, " ")
		}
		prefix := ""
		if strings.HasPrefix(line, ":") {
			prefix, line, _ = strings.Cut(line[1:], " ")
		}
		command, params, _ := strings.Cut(line, " ")
		switch command {
		case "PING":
			if _, err := fmt.Fprintf(conn, "PONG %s\r\n", params); err != nil {
				return joined, fmt.Errorf("cannot answer ping: %w", err)
			}
		case "JOIN":
			if !joined {
				joined = true
				t.logger.Printf("twitch: reading chat in #%s", t.channel)
			}
		case "PRIVMSG":
			_, text, ok := strings.Cut(params, " :")
			nick, _, _ := strings.Cut(prefix, "!")
			if ok && nick != "" {
				t.take(nick, text)
			}
		case "RECONNECT":
			return joined, errors.New("the chat server asked to reconnect")
		case "NOTICE":
			_, text, _ := strings.Cut(params, " :")
			t.logger.Printf("twitch: notice: %s", text)
		}
	}
}
