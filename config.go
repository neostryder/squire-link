package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type route struct {
	Target string `json:"target"`
	// Fallbacks are more addresses for the same kind of server, tried in order
	// after Target when it is busy, not ready or not answering.
	Fallbacks []string `json:"fallbacks,omitempty"`
	Key       string   `json:"key,omitempty"`
}

type config struct {
	Routes  map[string]route `json:"routes"`
	Origins []string         `json:"origins"`
	Channel channelConfig    `json:"channel"`
}

// channelConfig says where viewers' orders come from and which ones are taken.
type channelConfig struct {
	Prefix          string   `json:"prefix"`
	CooldownSeconds int      `json:"cooldownSeconds"`
	Allow           []string `json:"allow"`
	Block           []string `json:"block"`
	Twitch          struct {
		Enabled bool   `json:"enabled"`
		Channel string `json:"channel"`
	} `json:"twitch"`
	Discord struct {
		Enabled   bool   `json:"enabled"`
		ChannelID string `json:"channelId"`
	} `json:"discord"`
}

// discordKey is the keychain entry that holds the Discord bot token.
const discordKey = "discord"

var (
	namePattern          = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	twitchChannelPattern = regexp.MustCompile(`^[A-Za-z0-9_]{1,25}$`)
	snowflakePattern     = regexp.MustCompile(`^[0-9]{5,25}$`)
)

func defaultChannelConfig() channelConfig {
	return channelConfig{Prefix: "!squire", CooldownSeconds: 60, Allow: []string{}, Block: []string{}}
}

func defaultConfig() config {
	return config{
		Routes: map[string]route{
			"jev":  {Target: "https://api.typesafe.ai/v1/systemone", Key: "jev"},
			"laya": {Target: "http://localhost:8010/v1/systemone"},
		},
		Origins: []string{"https://angband.rpgm.world", "https://*.itch.zone", "http://localhost:*", "http://127.0.0.1:*"},
		Channel: defaultChannelConfig(),
	}
}

func configPath(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("user config directory: %w", err)
	}
	return filepath.Join(dir, "squire-link", "config.json"), nil
}

func loadConfig(path string) (config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return config{}, fmt.Errorf("create config directory: %w", err)
		}
		data, err = json.MarshalIndent(defaultConfig(), "", "  ")
		if err != nil {
			return config{}, err
		}
		data = append(data, '\n')
		file, createErr := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if createErr == nil {
			_, err = file.Write(data)
			closeErr := file.Close()
			if err != nil {
				return config{}, fmt.Errorf("write config: %w", err)
			}
			if closeErr != nil {
				return config{}, fmt.Errorf("close config: %w", closeErr)
			}
		} else if !errors.Is(createErr, os.ErrExist) {
			return config{}, fmt.Errorf("create config: %w", createErr)
		}
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return config{}, fmt.Errorf("read config %s: %w", path, err)
	}
	// A config file written before the channel section existed reads with the
	// channel defaults, which leave both platforms off.
	cfg := config{Channel: defaultChannelConfig()}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return config{}, fmt.Errorf("parse config %s: %w", path, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return config{}, fmt.Errorf("parse config %s: unexpected trailing data", path)
	}
	if err := cfg.validate(); err != nil {
		return config{}, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return cfg, nil
}

func (cfg config) validate() error {
	for name, r := range cfg.Routes {
		if !namePattern.MatchString(name) || name == "link-token" {
			return fmt.Errorf("invalid route name %q", name)
		}
		if r.Key != "" && (!namePattern.MatchString(r.Key) || r.Key == "link-token") {
			return fmt.Errorf("invalid key name for route %q", name)
		}
		for _, target := range append([]string{r.Target}, r.Fallbacks...) {
			u, err := url.Parse(target)
			if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" {
				return fmt.Errorf("invalid target for route %q: use an http or https URL without credentials, query, or fragment", name)
			}
		}
	}
	for _, origin := range cfg.Origins {
		if _, err := parseOriginPattern(origin); err != nil {
			return fmt.Errorf("invalid origin %q: %w", origin, err)
		}
	}
	return cfg.Channel.validate()
}

func (c channelConfig) validate() error {
	if strings.TrimSpace(c.Prefix) == "" || strings.ContainsAny(c.Prefix, " \t\r\n") {
		return errors.New("channel prefix must be a word such as !squire, with no spaces")
	}
	if c.CooldownSeconds < 0 || c.CooldownSeconds > 86400 {
		return errors.New("channel cooldownSeconds must be between 0 and 86400")
	}
	if c.Twitch.Enabled && !twitchChannelPattern.MatchString(strings.TrimPrefix(c.Twitch.Channel, "#")) {
		return errors.New("channel twitch.channel must be a Twitch channel name, such as the streamer's login name")
	}
	if c.Discord.Enabled && !snowflakePattern.MatchString(c.Discord.ChannelID) {
		return errors.New("channel discord.channelId must be the channel's numeric ID")
	}
	return nil
}

func routeNames(routes map[string]route) []string {
	names := make([]string, 0, len(routes))
	for name := range routes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

type originPattern struct {
	scheme, host, port string
	wildHost, wildPort bool
}

func parseOriginPattern(raw string) (originPattern, error) {
	p := originPattern{}
	parseable := raw
	if strings.HasSuffix(raw, ":*") {
		p.wildPort = true
		parseable = strings.TrimSuffix(raw, ":*")
	}
	u, err := url.Parse(parseable)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return p, errors.New("expected an http or https origin")
	}
	p.scheme = u.Scheme
	host := u.Hostname()
	p.port = u.Port()
	if strings.HasPrefix(host, "*.") {
		p.wildHost = true
		host = strings.TrimPrefix(host, "*.")
	}
	if host == "" || strings.Contains(host, "*") || strings.Contains(p.port, "*") || strings.Contains(raw, "#") || strings.Contains(raw, "?") {
		return p, errors.New("only a *. host prefix and a :* port suffix are supported")
	}
	if !p.wildPort && strings.Contains(raw, "*") && !p.wildHost {
		return p, errors.New("unsupported wildcard")
	}
	if p.port != "" {
		port, err := strconv.Atoi(p.port)
		if err != nil || port < 1 || port > 65535 || !regexp.MustCompile(`^[0-9]+$`).MatchString(p.port) {
			return p, errors.New("port must be numeric")
		}
	}
	p.host = strings.ToLower(host)
	return p, nil
}

func originAllowed(origin string, patterns []string) bool {
	u, err := url.Parse(origin)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	for _, raw := range patterns {
		p, err := parseOriginPattern(raw)
		if err != nil || p.scheme != u.Scheme {
			continue
		}
		host := strings.ToLower(u.Hostname())
		if p.wildHost {
			if !strings.HasSuffix(host, "."+p.host) || len(host) <= len(p.host)+1 {
				continue
			}
		} else if host != p.host {
			continue
		}
		if p.wildPort || p.port == u.Port() {
			return true
		}
	}
	return false
}
