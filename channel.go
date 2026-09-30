package main

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	// maxOrderRunes is the longest order text kept, in characters.
	maxOrderRunes = 300
	// maxQueued bounds the queue while nothing polls it, so a busy chat with
	// no game running does not grow it without end. The oldest order goes first.
	maxQueued          = 100
	maxCooldownEntries = 4096
	orderPageSize      = 50
)

type chatOrder struct {
	Text     string    `json:"text"`
	Platform string    `json:"platform"`
	User     string    `json:"user"`
	At       time.Time `json:"at"`
}

// orderQueue holds the viewers' orders until Squire collects them.
type orderQueue struct {
	prefix   string
	cooldown time.Duration
	allow    map[string]bool
	block    map[string]bool
	now      func() time.Time
	logger   *log.Logger

	mu      sync.Mutex
	items   []chatOrder
	last    map[string]time.Time
	dropped int
}

// startChannel starts a reader for each platform the config turns on.
func startChannel(ctx context.Context, c channelConfig, keys secretStore, queue *orderQueue, logger *log.Logger) {
	if c.Twitch.Enabled {
		reader := newTwitchReader(c.Twitch.Channel, func(user, text string) { queue.take("twitch", user, text) }, logger)
		go reader.run(ctx)
	}
	if c.Discord.Enabled {
		token, err := keys.Get("squire-link", discordKey)
		if err != nil || token == "" {
			logger.Printf("discord: no bot token is stored, so Discord is not read. Store one with: squire-link key set discord")
			return
		}
		reader := newDiscordReader(c.Discord.ChannelID, token, func(user, text string) { queue.take("discord", user, text) }, logger)
		go reader.run(ctx)
	}
}

func newOrderQueue(c channelConfig, now func() time.Time, logger *log.Logger) *orderQueue {
	q := &orderQueue{
		prefix:   strings.ToLower(strings.TrimSpace(c.Prefix)),
		cooldown: time.Duration(c.CooldownSeconds) * time.Second,
		allow:    nameSet(c.Allow),
		block:    nameSet(c.Block),
		now:      now,
		logger:   logger,
		last:     map[string]time.Time{},
	}
	return q
}

func nameSet(names []string) map[string]bool {
	set := map[string]bool{}
	for _, name := range names {
		if name = strings.ToLower(strings.TrimSpace(name)); name != "" {
			set[name] = true
		}
	}
	return set
}

// orderText returns the order a chat message carries, or false when the
// message does not start with the prefix as a word of its own.
func orderText(prefix, message string) (string, bool) {
	message = strings.TrimSpace(message)
	if len(message) < len(prefix) || !strings.EqualFold(message[:len(prefix)], prefix) {
		return "", false
	}
	rest := message[len(prefix):]
	if rest != "" {
		first, _ := utf8.DecodeRuneInString(rest)
		if !unicode.IsSpace(first) {
			return "", false
		}
	}
	text := strings.Join(strings.FieldsFunc(rest, unicode.IsSpace), " ")
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, text)
	if utf8.RuneCountInString(text) > maxOrderRunes {
		text = strings.TrimSpace(string([]rune(text)[:maxOrderRunes]))
	}
	return text, text != ""
}

// take queues a chat message as an order when it passes the prefix, the
// allow and block lists and the user's cooldown. It reports whether it did.
func (q *orderQueue) take(platform, user, message string) bool {
	text, ok := orderText(q.prefix, message)
	if !ok {
		return false
	}
	name := strings.ToLower(strings.TrimSpace(user))
	if name == "" || q.block[name] || (len(q.allow) > 0 && !q.allow[name]) {
		return false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	now := q.now()
	who := platform + ":" + name
	if at, seen := q.last[who]; seen && now.Sub(at) < q.cooldown {
		return false
	}
	if q.cooldown > 0 {
		for key, at := range q.last {
			if now.Sub(at) >= q.cooldown {
				delete(q.last, key)
			}
		}
		if len(q.last) >= maxCooldownEntries {
			oldest := ""
			for key, at := range q.last {
				if oldest == "" || at.Before(q.last[oldest]) {
					oldest = key
				}
			}
			delete(q.last, oldest)
		}
		q.last[who] = now
	}
	q.items = append(q.items, chatOrder{Text: text, Platform: platform, User: strings.TrimSpace(user), At: now.UTC()})
	if len(q.items) > maxQueued {
		q.items[0] = chatOrder{}
		q.items = q.items[1:]
		q.dropped++
		if q.logger != nil {
			q.logger.Printf("time=%s orders dropped=%d", now.UTC().Format(time.RFC3339), q.dropped)
		}
	}
	if q.logger != nil {
		q.logger.Printf("time=%s order platform=%s user=%s chars=%d", now.UTC().Format(time.RFC3339), platform, name, utf8.RuneCountInString(text))
	}
	return true
}

// drain returns one page of orders, oldest first, and leaves the rest queued.
func (q *orderQueue) drain(limit int) ([]chatOrder, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := min(limit, len(q.items))
	out := make([]chatOrder, n)
	copy(out, q.items[:n])
	clear(q.items[:n])
	q.items = q.items[n:]
	return out, len(q.items) > 0
}
