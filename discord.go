package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"time"
)

const (
	discordAPI   = "https://discord.com/api/v10"
	discordEvery = 5 * time.Second
)

// discordReader polls one Discord channel's messages with a bot token. Polling
// needs only the standard library; the gateway would need a WebSocket client,
// heartbeats and session resumes for the same messages a few seconds sooner.
type discordReader struct {
	base      string
	channelID string
	token     string
	client    *http.Client
	every     time.Duration
	take      func(user, text string)
	logger    *log.Logger

	after        string
	lastProblem  string
	warnedNoText bool
}

type discordMessage struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	Author  struct {
		Username string `json:"username"`
		Bot      bool   `json:"bot"`
	} `json:"author"`
}

// errStop is a refusal that trying again will not fix.
type errStop struct{ message string }

func (e errStop) Error() string { return e.message }

func newDiscordReader(channelID, token string, take func(user, text string), logger *log.Logger) *discordReader {
	return &discordReader{
		base: discordAPI, channelID: channelID, token: token, take: take, logger: logger,
		every:  discordEvery,
		client: &http.Client{Timeout: 20 * time.Second},
	}
}

func (d *discordReader) run(ctx context.Context) {
	wait := d.every
	for {
		retryAfter, err := d.poll(ctx)
		if ctx.Err() != nil {
			return
		}
		var stop errStop
		switch {
		case errors.As(err, &stop):
			d.logger.Printf("discord: %s. Squire Link stopped reading Discord. Fix that and restart it.", stop.message)
			return
		case err != nil:
			// A problem is logged once while it lasts, not on every retry.
			if err.Error() != d.lastProblem {
				d.logger.Printf("discord: %v; trying again", err)
				d.lastProblem = err.Error()
			}
			wait = min(max(wait*2, d.every), readerRetryMax)
		default:
			if d.lastProblem != "" {
				d.logger.Printf("discord: reading channel %s again", d.channelID)
				d.lastProblem = ""
			}
			wait = d.every
		}
		if retryAfter > wait {
			wait = retryAfter
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// poll reads the messages since the last one seen. The first poll only notes
// the newest message, so orders written before Squire Link started are not
// replayed. A rate limit comes back as how long to wait.
func (d *discordReader) poll(ctx context.Context) (time.Duration, error) {
	query := "limit=100&after=" + d.after
	if d.after == "" {
		query = "limit=1"
	}
	messages, wait, limited, err := d.pollPage(ctx, query)
	if err != nil || limited {
		return wait, err
	}
	if d.after == "" {
		d.after = "0"
		if len(messages) > 0 {
			d.after = messages[0].ID
		}
		return wait, nil
	}
	cursor := d.after
	pages := [][]discordMessage{messages}
	for len(messages) == 100 {
		sort.Slice(messages, func(i, j int) bool { return snowflakeLess(messages[i].ID, messages[j].ID) })
		if !snowflakeLess(cursor, messages[0].ID) {
			break
		}
		if wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return 0, ctx.Err()
			case <-timer.C:
			}
		}
		messages, wait, limited, err = d.pollPage(ctx, "limit=100&before="+messages[0].ID)
		if err != nil || limited {
			return wait, err
		}
		pages = append(pages, messages)
	}
	for i := len(pages) - 1; i >= 0; i-- {
		messages = pages[i]
		sort.Slice(messages, func(a, b int) bool { return snowflakeLess(messages[a].ID, messages[b].ID) })
		for _, m := range messages {
			if !snowflakeLess(d.after, m.ID) {
				continue
			}
			if !m.Author.Bot {
				if m.Content == "" && !d.warnedNoText {
					d.warnedNoText = true
					d.logger.Printf("discord: a message came with no text. Turn on the Message Content intent for the bot in the Discord developer portal.")
				}
				d.take(m.Author.Username, m.Content)
			}
			d.after = m.ID
		}
	}
	return wait, nil
}

func (d *discordReader) pollPage(ctx context.Context, query string) ([]discordMessage, time.Duration, bool, error) {
	address := d.base + "/channels/" + d.channelID + "/messages?" + query
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, 0, false, errStop{"the channel address is invalid"}
	}
	request.Header.Set("Authorization", "Bot "+d.token)
	request.Header.Set("User-Agent", "DiscordBot (https://github.com/neostryder/squire-link, "+version+")")
	response, err := d.client.Do(request)
	if err != nil {
		return nil, 0, false, errors.New("Discord did not answer")
	}
	defer response.Body.Close()
	body := io.LimitReader(response.Body, 4<<20)
	var wait time.Duration
	if response.Header.Get("X-RateLimit-Remaining") == "0" {
		if reset, err := strconv.ParseFloat(response.Header.Get("X-RateLimit-Reset-After"), 64); err == nil {
			wait = max(wait, time.Duration(reset*float64(time.Second)))
		}
	}
	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusTooManyRequests:
		var limit struct {
			RetryAfter float64 `json:"retry_after"`
		}
		_ = json.NewDecoder(body).Decode(&limit)
		wait = max(wait, time.Duration(limit.RetryAfter*float64(time.Second)))
		if header, err := strconv.ParseFloat(response.Header.Get("Retry-After"), 64); err == nil && time.Duration(header*float64(time.Second)) > wait {
			wait = time.Duration(header * float64(time.Second))
		}
		return nil, max(wait, time.Second), true, nil
	case http.StatusUnauthorized:
		return nil, 0, false, errStop{"Discord refused the bot token (401). Store the right one with: squire-link key set discord"}
	case http.StatusForbidden:
		return nil, 0, false, errStop{"Discord says the bot cannot read channel " + d.channelID + " (403). Give the bot the View Channel and Read Message History permissions there"}
	case http.StatusNotFound:
		return nil, 0, false, errStop{"Discord has no channel " + d.channelID + " that the bot can see (404)"}
	default:
		return nil, 0, false, fmt.Errorf("Discord answered %d", response.StatusCode)
	}
	var messages []discordMessage
	if err := json.NewDecoder(body).Decode(&messages); err != nil {
		return nil, 0, false, errors.New("Discord sent messages Squire Link cannot read")
	}
	return messages, wait, false, nil
}

func snowflakeLess(a, b string) bool {
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}
