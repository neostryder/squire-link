package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zalando/go-keyring"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func testQueue(c channelConfig) (*orderQueue, *clock) {
	clk := &clock{t: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)}
	return newOrderQueue(c, clk.now, nil), clk
}

func TestOrderPrefix(t *testing.T) {
	for _, tc := range []struct {
		message, want string
		ok            bool
	}{
		{"!squire run from uniques", "run from uniques", true},
		{"  !SQUIRE   run   away  ", "run away", true},
		{"!squire\trest", "rest", true},
		{"!squirely run", "", false},
		{"hello !squire run", "", false},
		{"!squire", "", false},
		{"!squire    ", "", false},
		{"squire run", "", false},
		{"!squire keep\x07 two flasks", "keep two flasks", true},
	} {
		got, ok := orderText("!squire", tc.message)
		if got != tc.want || ok != tc.ok {
			t.Errorf("orderText(%q) = %q, %v; want %q, %v", tc.message, got, ok, tc.want, tc.ok)
		}
	}
}

func TestOrderCap(t *testing.T) {
	long := strings.Repeat("\u00e9", 400)
	got, ok := orderText("!squire", "!squire "+long)
	if !ok || len([]rune(got)) != maxOrderRunes {
		t.Fatalf("capped to %d characters, want %d", len([]rune(got)), maxOrderRunes)
	}
	q, clk := testQueue(channelConfig{Prefix: "!squire", CooldownSeconds: 0})
	for i := 0; i < maxQueued+5; i++ {
		clk.t = clk.t.Add(time.Second)
		q.take("twitch", "grip", "!squire order "+string(rune('a'+i%26)))
	}
	orders, _ := q.drain(maxQueued)
	if n := len(orders); n != maxQueued {
		t.Fatalf("queue held %d orders, want %d", n, maxQueued)
	}
}

func TestOrdersEndpointPagesAndLogsDrops(t *testing.T) {
	var logs bytes.Buffer
	h := testRelay(defaultConfig(), "local", &logs)
	h.orders.cooldown = 0
	for i := 0; i < maxQueued+5; i++ {
		h.orders.take("twitch", "grip", fmt.Sprintf("!squire order %d", i))
	}
	for page, wantCount := range []int{50, 50, 0} {
		response := getOrders(h, "")
		var got struct {
			Orders []chatOrder `json:"orders"`
			More   bool        `json:"more"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if response.Code != http.StatusOK || len(got.Orders) != wantCount || got.More != (page == 0) {
			t.Fatalf("page %d: status=%d body=%s", page, response.Code, response.Body.String())
		}
		for i, order := range got.Orders {
			if want := fmt.Sprintf("order %d", 5+page*50+i); order.Text != want {
				t.Fatalf("page %d order %d: %q, want %q", page, i, order.Text, want)
			}
		}
	}
	if !strings.Contains(logs.String(), "dropped=5") {
		t.Fatalf("missing drop count: %s", logs.String())
	}
}

func TestCooldownEntriesExpireAndStayBounded(t *testing.T) {
	q, clk := testQueue(channelConfig{Prefix: "!squire", CooldownSeconds: 60})
	for i := 0; i < maxCooldownEntries+20; i++ {
		if !q.take("twitch", fmt.Sprintf("user%d", i), "!squire rest") {
			t.Fatal("distinct viewer refused")
		}
	}
	if len(q.last) > maxCooldownEntries {
		t.Fatalf("cooldown entries: %d", len(q.last))
	}
	clk.t = clk.t.Add(60 * time.Second)
	q.take("twitch", "new", "!squire rest")
	if len(q.last) != 1 {
		t.Fatalf("expired cooldown entries remain: %d", len(q.last))
	}
}

func TestAllowAndBlockLists(t *testing.T) {
	q, _ := testQueue(channelConfig{Prefix: "!squire", Block: []string{"Troll"}})
	if q.take("twitch", "troll", "!squire dive") || q.take("discord", "TROLL", "!squire dive") {
		t.Fatal("a blocked user's order was taken")
	}
	if !q.take("twitch", "grip", "!squire rest") {
		t.Fatal("an unlisted user was refused with no allow list")
	}
	q, _ = testQueue(channelConfig{Prefix: "!squire", Allow: []string{"Grip", "fang"}, Block: []string{"fang"}})
	if !q.take("twitch", "grip", "!squire rest") {
		t.Fatal("an allowed user was refused")
	}
	if q.take("twitch", "wolf", "!squire rest") {
		t.Fatal("a user missing from the allow list was taken")
	}
	if q.take("twitch", "fang", "!squire rest") {
		t.Fatal("the block list did not win over the allow list")
	}
}

func TestCooldown(t *testing.T) {
	q, clk := testQueue(channelConfig{Prefix: "!squire", CooldownSeconds: 60})
	if !q.take("twitch", "Grip", "!squire run") {
		t.Fatal("first order refused")
	}
	clk.t = clk.t.Add(59 * time.Second)
	if q.take("twitch", "grip", "!squire fight") {
		t.Fatal("a second order inside the cooldown was taken")
	}
	if q.take("twitch", "grip", "just chatting") {
		t.Fatal("a message without the prefix was taken")
	}
	if !q.take("twitch", "fang", "!squire fight") || !q.take("discord", "grip", "!squire fight") {
		t.Fatal("the cooldown held back another user or platform")
	}
	clk.t = clk.t.Add(time.Second)
	if !q.take("twitch", "grip", "!squire fight") {
		t.Fatal("an order after the cooldown was refused")
	}
	got, _ := q.drain(maxQueued)
	if len(got) != 4 || got[0].Text != "run" || got[0].User != "Grip" || got[0].Platform != "twitch" || !got[0].At.Equal(time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("queued: %+v", got)
	}
}

func getOrders(h *relay, origin string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, "/v1/orders", nil)
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request)
	return response
}

func TestOrdersEndpointReturnsAndClears(t *testing.T) {
	h := testRelay(defaultConfig(), "local", new(bytes.Buffer))
	h.orders.take("twitch", "Grip", "!squire run from uniques")
	h.orders.take("discord", "fang", "!squire keep two flasks of oil")
	response := getOrders(h, "https://angband.rpgm.world")
	if response.Code != http.StatusOK || response.Header().Get("Access-Control-Allow-Origin") != "https://angband.rpgm.world" || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status %d headers %v", response.Code, response.Header())
	}
	var got struct {
		Orders []map[string]any `json:"orders"`
		More   bool             `json:"more"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Orders) != 2 || got.More || got.Orders[0]["text"] != "run from uniques" || got.Orders[0]["platform"] != "twitch" || got.Orders[0]["user"] != "Grip" || got.Orders[1]["user"] != "fang" {
		t.Fatalf("orders: %s", response.Body.String())
	}
	if _, err := time.Parse(time.RFC3339, got.Orders[0]["at"].(string)); err != nil {
		t.Fatalf("at is not a timestamp: %v", got.Orders[0]["at"])
	}
	response = getOrders(h, "")
	if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != `{"orders":[],"more":false}` {
		t.Fatalf("second read: %d %s", response.Code, response.Body.String())
	}
}

func TestOrdersEndpointOriginRules(t *testing.T) {
	h := testRelay(defaultConfig(), "local", new(bytes.Buffer))
	h.orders.take("twitch", "grip", "!squire rest")
	response := getOrders(h, "https://angband.rpgm.world.evil.com")
	if response.Code != http.StatusForbidden || response.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("bad origin: %d", response.Code)
	}
	orders, _ := h.orders.drain(maxQueued)
	if len(orders) != 1 {
		t.Fatal("a refused page emptied the queue")
	}
	response = httptest.NewRecorder()
	h.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/orders", nil))
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status %d", response.Code)
	}
	preflight := httptest.NewRequest(http.MethodOptions, "/v1/orders", nil)
	preflight.Header.Set("Origin", "http://localhost:3000")
	response = httptest.NewRecorder()
	h.ServeHTTP(response, preflight)
	if response.Code != http.StatusNoContent || !strings.Contains(response.Header().Get("Access-Control-Allow-Methods"), "GET") {
		t.Fatalf("preflight: %d %v", response.Code, response.Header())
	}

	serve := testRelay(defaultConfig(), "serve", new(bytes.Buffer))
	if err := keyring.Set("squire-link", "link-token", "token-secret"); err != nil {
		t.Fatal(err)
	}
	serve.orders.take("twitch", "grip", "!squire rest")
	if response := getOrders(serve, "https://angband.rpgm.world"); response.Code != http.StatusForbidden {
		t.Fatalf("serve mode without a token: %d", response.Code)
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/orders", nil)
	request.Header.Set("X-Squire-Link-Token", "token-secret")
	response = httptest.NewRecorder()
	serve.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"rest"`) {
		t.Fatalf("serve mode with a token: %d %s", response.Code, response.Body.String())
	}
}

func TestChannelConfigDefaults(t *testing.T) {
	c := defaultConfig().Channel
	if c.Prefix != "!squire" || c.CooldownSeconds != 60 || c.Twitch.Enabled || c.Discord.Enabled || len(c.Allow) != 0 || len(c.Block) != 0 {
		t.Fatalf("channel defaults: %+v", c)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"routes":{},"origins":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Channel.Prefix != "!squire" || cfg.Channel.CooldownSeconds != 60 || cfg.Channel.Twitch.Enabled || cfg.Channel.Discord.Enabled {
		t.Fatalf("a config without a channel section read as %+v", cfg.Channel)
	}
	if err := os.WriteFile(path, []byte(`{"routes":{},"origins":[],"channel":{"cooldownSeconds":0,"twitch":{"enabled":true,"channel":"#Grip_Plays"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if cfg, err = loadConfig(path); err != nil || cfg.Channel.Prefix != "!squire" || cfg.Channel.CooldownSeconds != 0 || !cfg.Channel.Twitch.Enabled {
		t.Fatalf("partial channel section: %+v %v", cfg.Channel, err)
	}
	fresh := filepath.Join(t.TempDir(), "squire-link", "config.json")
	if _, err := loadConfig(fresh); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(fresh)
	if err != nil || !strings.Contains(string(data), `"prefix": "!squire"`) || !strings.Contains(string(data), `"cooldownSeconds": 60`) {
		t.Fatalf("new config file lacks the channel section: %s", data)
	}
	for _, bad := range []func(*channelConfig){
		func(c *channelConfig) { c.Prefix = "" },
		func(c *channelConfig) { c.Prefix = "! squire" },
		func(c *channelConfig) { c.CooldownSeconds = -1 },
		func(c *channelConfig) { c.Twitch.Enabled = true },
		func(c *channelConfig) { c.Twitch.Enabled, c.Twitch.Channel = true, "not a channel" },
		func(c *channelConfig) { c.Discord.Enabled = true },
		func(c *channelConfig) { c.Discord.Enabled, c.Discord.ChannelID = true, "general" },
	} {
		c := defaultChannelConfig()
		bad(&c)
		if c.validate() == nil {
			t.Errorf("accepted %+v", c)
		}
	}
}

func TestKeyListNamesTheDiscordKeyWhenDiscordIsOn(t *testing.T) {
	keyring.MockInit()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"routes":{},"origins":[],"channel":{"discord":{"enabled":true,"channelId":"123456789012345678"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runCLI([]string{"key", "list", "--config", path}, strings.NewReader(""), &out, new(bytes.Buffer), osKeyring{}); err != nil {
		t.Fatal(err)
	}
	if out.String() != "discord: not set\n" {
		t.Fatalf("key list: %q", out.String())
	}
}

type taken struct{ user, text string }

func TestTwitchReaderAgainstAFakeServer(t *testing.T) {
	client, server := net.Pipe()
	var logs bytes.Buffer
	var got []taken
	reader := &twitchReader{
		channel: "grip_plays",
		dial:    func(context.Context) (net.Conn, error) { return client, nil },
		take:    func(user, text string) { got = append(got, taken{user, text}) },
		logger:  log.New(&logs, "", 0),
		retry:   time.Millisecond,
	}
	result := make(chan error, 1)
	joined := make(chan bool, 1)
	go func() {
		ok, err := reader.session(context.Background())
		joined <- ok
		result <- err
	}()
	in := bufio.NewReader(server)
	nick, _ := in.ReadString('\n')
	join, _ := in.ReadString('\n')
	if !strings.HasPrefix(nick, "NICK justinfan") || join != "JOIN #grip_plays\r\n" {
		t.Fatalf("login: %q %q", nick, join)
	}
	send := func(line string) {
		if _, err := server.Write([]byte(line + "\r\n")); err != nil {
			t.Fatal(err)
		}
	}
	send(":justinfan1.tmi.twitch.tv 001 justinfan1 :Welcome, GLHF!")
	send(":justinfan1!justinfan1@justinfan1.tmi.twitch.tv JOIN #grip_plays")
	send("PING :tmi.twitch.tv")
	if pong, _ := in.ReadString('\n'); pong != "PONG :tmi.twitch.tv\r\n" {
		t.Fatalf("pong: %q", pong)
	}
	send(":grip!grip@grip.tmi.twitch.tv PRIVMSG #grip_plays :!squire run from uniques")
	send("@badge-info=;color=#FF0000;display-name=Fang :fang!fang@fang.tmi.twitch.tv PRIVMSG #grip_plays :hello there: all")
	send("RECONNECT")
	err := <-result
	if !<-joined || err == nil || !strings.Contains(err.Error(), "reconnect") {
		t.Fatalf("session end: %v", err)
	}
	want := []taken{{"grip", "!squire run from uniques"}, {"fang", "hello there: all"}}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("messages: %+v", got)
	}
	if !strings.Contains(logs.String(), "reading chat in #grip_plays") {
		t.Fatalf("log: %q", logs.String())
	}
}

func TestTwitchReaderRetriesAndStops(t *testing.T) {
	var logs bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	dials := 0
	reader := &twitchReader{
		channel: "grip_plays",
		dial: func(context.Context) (net.Conn, error) {
			dials++
			if dials == 3 {
				cancel()
			}
			return nil, errors.New("no route")
		},
		take:   func(string, string) {},
		logger: log.New(&logs, "", 0),
		retry:  time.Millisecond,
	}
	done := make(chan struct{})
	go func() { reader.run(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("reader did not stop")
	}
	if dials != 3 || !strings.Contains(logs.String(), "cannot connect") {
		t.Fatalf("dials=%d log=%q", dials, logs.String())
	}
}

func fakeDiscord(t *testing.T, handle func(w http.ResponseWriter, r *http.Request)) (*discordReader, *bytes.Buffer, *[]taken) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(handle))
	t.Cleanup(server.Close)
	var logs bytes.Buffer
	got := &[]taken{}
	reader := newDiscordReader("123456789012345678", "bot-token-secret", func(user, text string) { *got = append(*got, taken{user, text}) }, log.New(&logs, "", 0))
	reader.base = server.URL
	reader.every = time.Millisecond
	return reader, &logs, got
}

func TestDiscordReaderAgainstAFakeServer(t *testing.T) {
	var queries []string
	reader, logs, got := fakeDiscord(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/channels/123456789012345678/messages" || r.Header.Get("Authorization") != "Bot bot-token-secret" || !strings.HasPrefix(r.Header.Get("User-Agent"), "DiscordBot (") {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		queries = append(queries, r.URL.RawQuery)
		switch r.URL.Query().Get("after") {
		case "":
			_, _ = w.Write([]byte(`[{"id":"1000","content":"!squire an old order","author":{"username":"old"}}]`))
		case "1000":
			_, _ = w.Write([]byte(`[
				{"id":"1003","content":"!squire from a bot","author":{"username":"helper","bot":true}},
				{"id":"1002","content":"!squire keep two flasks","author":{"username":"fang"}},
				{"id":"1001","content":"!squire run from uniques","author":{"username":"Grip"}}
			]`))
		default:
			_, _ = w.Write([]byte(`[]`))
		}
	})
	for i := 0; i < 3; i++ {
		if _, err := reader.poll(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(queries) != 3 || queries[0] != "limit=1" || queries[1] != "limit=100&after=1000" || queries[2] != "limit=100&after=1003" {
		t.Fatalf("queries: %v", queries)
	}
	want := []taken{{"Grip", "!squire run from uniques"}, {"fang", "!squire keep two flasks"}}
	if len(*got) != 2 || (*got)[0] != want[0] || (*got)[1] != want[1] {
		t.Fatalf("messages: %+v", *got)
	}
	if strings.Contains(logs.String(), "bot-token-secret") {
		t.Fatal("token leaked in log")
	}
}

func TestDiscordReaderCatchesUpAcrossPages(t *testing.T) {
	var queries []string
	reader, _, got := fakeDiscord(t, func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.RawQuery)
		if r.URL.Query().Get("after") == "" && r.URL.Query().Get("before") == "" {
			_, _ = w.Write([]byte(`[{"id":"1000","content":"old","author":{"username":"old"}}]`))
			return
		}
		before := 1206
		if raw := r.URL.Query().Get("before"); raw != "" {
			before, _ = strconv.Atoi(raw)
		}
		var messages []discordMessage
		for id := before - 1; id > 1000 && len(messages) < 100; id-- {
			var m discordMessage
			m.ID = strconv.Itoa(id)
			m.Content = "!squire order " + m.ID
			m.Author.Username = "grip"
			messages = append(messages, m)
		}
		_ = json.NewEncoder(w).Encode(messages)
	})
	if _, err := reader.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(queries) != 4 || queries[1] != "limit=100&after=1000" || queries[2] != "limit=100&before=1106" || queries[3] != "limit=100&before=1006" {
		t.Fatalf("queries: %v", queries)
	}
	if len(*got) != 205 || (*got)[0].text != "!squire order 1001" || (*got)[204].text != "!squire order 1205" || reader.after != "1205" {
		t.Fatalf("read %d messages, cursor %s", len(*got), reader.after)
	}
}

func TestDiscordReaderWaitsForRateLimitBeforeNextPage(t *testing.T) {
	var queries []string
	reader, _, got := fakeDiscord(t, func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.RawQuery)
		switch len(queries) {
		case 1:
			_, _ = w.Write([]byte(`[{"id":"1000"}]`))
		case 2, 4:
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset-After", "0.01")
			var messages []discordMessage
			for id := 1101; id > 1001; id-- {
				messages = append(messages, discordMessage{ID: strconv.Itoa(id)})
			}
			_ = json.NewEncoder(w).Encode(messages)
		case 3:
			w.Header().Set("Retry-After", "0.02")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"retry_after":0.01}`))
		default:
			_, _ = w.Write([]byte(`[{"id":"1001","content":"!squire first","author":{"username":"grip"}}]`))
		}
	})
	if _, err := reader.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	wait, err := reader.poll(context.Background())
	if err != nil || wait < 20*time.Millisecond || time.Since(start) < 8*time.Millisecond || reader.after != "1000" || len(*got) != 0 {
		t.Fatalf("rate limited: wait=%s err=%v cursor=%s taken=%d requests=%v", wait, err, reader.after, len(*got), queries)
	}
	if _, err := reader.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(*got) != 101 || reader.after != "1101" || len(queries) != 5 {
		t.Fatalf("retry: messages=%d cursor=%s", len(*got), reader.after)
	}
}

func TestDiscordReaderRateLimitAndRefusal(t *testing.T) {
	status := http.StatusTooManyRequests
	reader, logs, _ := fakeDiscord(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"message":"You are being rate limited.","retry_after":2.5}`))
	})
	wait, err := reader.poll(context.Background())
	if err != nil || wait != 2500*time.Millisecond {
		t.Fatalf("rate limit: wait=%s err=%v", wait, err)
	}
	status = http.StatusUnauthorized
	done := make(chan struct{})
	go func() { reader.run(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("reader kept going after the token was refused")
	}
	if !strings.Contains(logs.String(), "squire-link key set discord") || strings.Contains(logs.String(), "bot-token-secret") {
		t.Fatalf("log: %q", logs.String())
	}
}

func TestDiscordReaderLogsAFailureOnceAndRecovers(t *testing.T) {
	failing := true
	calls := 0
	ctx, cancel := context.WithCancel(context.Background())
	reader, logs, _ := fakeDiscord(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 6 {
			cancel()
		}
		if failing && calls < 4 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`[]`))
	})
	reader.run(ctx)
	if n := strings.Count(logs.String(), "Discord answered 502"); n != 1 {
		t.Fatalf("logged the failure %d times: %q", n, logs.String())
	}
	if !strings.Contains(logs.String(), "reading channel 123456789012345678 again") {
		t.Fatalf("no recovery line: %q", logs.String())
	}
}

func TestStartChannelWithoutADiscordToken(t *testing.T) {
	keyring.MockInit()
	var logs bytes.Buffer
	c := defaultChannelConfig()
	c.Discord.Enabled, c.Discord.ChannelID = true, "123456789012345678"
	q, _ := testQueue(c)
	startChannel(context.Background(), c, osKeyring{}, q, log.New(&logs, "", 0))
	if !strings.Contains(logs.String(), "no bot token is stored") {
		t.Fatalf("log: %q", logs.String())
	}
}
