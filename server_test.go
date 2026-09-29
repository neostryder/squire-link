package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"
)

func testRelay(cfg config, mode string, logs *bytes.Buffer) *relay {
	keyring.MockInit()
	return newRelay(cfg, "/test/config.json", mode, osKeyring{}, log.New(logs, "", 0))
}

func TestOriginMatching(t *testing.T) {
	allowed := defaultConfig().Origins
	for _, tc := range []struct {
		origin string
		want   bool
	}{
		{"https://angband.rpgm.world", true},
		{"https://foo.itch.zone", true},
		{"https://a.b.itch.zone", true},
		{"http://localhost:3000", true},
		{"http://127.0.0.1:8765", true},
		{"https://evilangband.rpgm.world", false},
		{"https://angband.rpgm.world.evil.com", false},
		{"https://itch.zone", false},
		{"https://baditch.zone", false},
		{"http://localhost.evil.com:3000", false},
		{"https://angband.rpgm.world:1234", false},
		{"https://angband.rpgm.world.evil.com@evil.com", false},
		{"null", false},
	} {
		if got := originAllowed(tc.origin, allowed); got != tc.want {
			t.Errorf("originAllowed(%q) = %v, want %v", tc.origin, got, tc.want)
		}
	}
	if err := defaultConfig().validate(); err != nil {
		t.Fatalf("default config invalid: %v", err)
	}
}

func TestPreflightAndBadOrigin(t *testing.T) {
	h := testRelay(defaultConfig(), "local", new(bytes.Buffer))
	request := httptest.NewRequest(http.MethodOptions, "/v1/systemone/jev", nil)
	request.Header.Set("Origin", "https://angband.rpgm.world")
	request.Header.Set("Access-Control-Request-Private-Network", "true")
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d", response.Code)
	}
	for name, want := range map[string]string{
		"Access-Control-Allow-Origin":          "https://angband.rpgm.world",
		"Access-Control-Allow-Methods":         "POST, GET, OPTIONS",
		"Access-Control-Allow-Headers":         "Content-Type, Authorization, X-Squire-Link-Token",
		"Access-Control-Allow-Private-Network": "true",
		"Access-Control-Max-Age":               "600",
		"Vary":                                 "Origin",
	} {
		if got := response.Header().Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	bad := httptest.NewRequest(http.MethodGet, "/health", nil)
	bad.Header.Set("Origin", "https://angband.rpgm.world.evil.com")
	response = httptest.NewRecorder()
	h.ServeHTTP(response, bad)
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "/test/config.json") || !strings.Contains(response.Body.String(), "angband.rpgm.world.evil.com") {
		t.Fatalf("bad origin response: %d %s", response.Code, response.Body.String())
	}
	if response.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("bad origin received CORS permission")
	}
}

func TestConfiguredRoutingAndAuthorization(t *testing.T) {
	var gotBody, gotAuth, gotToken, gotContentType string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		gotAuth = r.Header.Get("Authorization")
		gotToken = r.Header.Get("X-Squire-Link-Token")
		gotContentType = r.Header.Get("Content-Type")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"done":true}`))
	}))
	defer upstream.Close()
	cfg := config{Routes: map[string]route{"jev": {Target: upstream.URL, Key: "jev"}}, Origins: defaultConfig().Origins}
	var logs bytes.Buffer
	h := testRelay(cfg, "local", &logs)
	secret := "private-secret-123"
	if err := keyring.Set("squire-link", "jev", secret); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/systemone/jev", strings.NewReader(`{"turn":1}`))
	request.Header.Set("Authorization", "Bearer page-secret")
	request.Header.Set("X-Squire-Link-Token", "page-token")
	request.Header.Set("Content-Type", "text/plain")
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || response.Body.String() != `{"done":true}` {
		t.Fatalf("relay response: %d %s", response.Code, response.Body.String())
	}
	if gotBody != `{"turn":1}` || gotAuth != "Bearer "+secret || gotToken != "" || gotContentType != "application/json" {
		t.Fatalf("upstream got body=%q auth=%q token=%q content-type=%q", gotBody, gotAuth, gotToken, gotContentType)
	}
	if strings.Contains(logs.String(), secret) || strings.Contains(logs.String(), "page-secret") || strings.Contains(logs.String(), "page-token") || strings.Contains(logs.String(), gotBody) {
		t.Fatalf("secret or body leaked in log: %q", logs.String())
	}
	if !strings.Contains(logs.String(), "route=jev") || !strings.Contains(logs.String(), "status=202") {
		t.Fatalf("missing relay log fields: %q", logs.String())
	}
	response = httptest.NewRecorder()
	h.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/systemone/other", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("unconfigured route status = %d", response.Code)
	}
	response = httptest.NewRecorder()
	h.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/systemone/jev", nil))
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("non-POST status = %d", response.Code)
	}
}

func TestBodyLimit(t *testing.T) {
	called := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer upstream.Close()
	h := testRelay(config{Routes: map[string]route{"laya": {Target: upstream.URL}}}, "local", new(bytes.Buffer))
	request := httptest.NewRequest(http.MethodPost, "/v1/systemone/laya", bytes.NewReader(bytes.Repeat([]byte("x"), maxBodyBytes+1)))
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request)
	if response.Code != http.StatusRequestEntityTooLarge || called {
		t.Fatalf("oversized body status=%d upstream called=%v", response.Code, called)
	}
}

func TestServeTokenRequiredAndStripped(t *testing.T) {
	var gotToken string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get("X-Squire-Link-Token")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	var logs bytes.Buffer
	h := testRelay(config{Routes: map[string]route{"laya": {Target: upstream.URL}}, Origins: defaultConfig().Origins}, "serve", &logs)
	if err := keyring.Set("squire-link", "link-token", "token-secret"); err != nil {
		t.Fatal(err)
	}
	for _, provided := range []string{"", "wrong"} {
		request := httptest.NewRequest(http.MethodPost, "/v1/systemone/laya", nil)
		request.Header.Set("Origin", "https://angband.rpgm.world")
		request.Header.Set("X-Squire-Link-Token", provided)
		response := httptest.NewRecorder()
		h.ServeHTTP(response, request)
		if response.Code != http.StatusForbidden {
			t.Fatalf("token %q status=%d", provided, response.Code)
		}
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/systemone/laya", nil)
	request.Header.Set("X-Squire-Link-Token", "token-secret")
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request)
	if response.Code != http.StatusOK || gotToken != "" {
		t.Fatalf("valid token response=%d forwarded token=%q", response.Code, gotToken)
	}
	if strings.Contains(logs.String(), "token-secret") {
		t.Fatal("token leaked in log")
	}
	response = httptest.NewRecorder()
	h.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/health", nil))
	if response.Code != http.StatusForbidden {
		t.Fatalf("serve health without Origin or token status=%d", response.Code)
	}
}

func TestRedirectDoesNotReachAnotherHost(t *testing.T) {
	called := false
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer other.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", other.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
		_, _ = w.Write([]byte("redirect body"))
	}))
	defer upstream.Close()
	h := testRelay(config{Routes: map[string]route{"laya": {Target: upstream.URL}}}, "local", new(bytes.Buffer))
	response := httptest.NewRecorder()
	h.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/systemone/laya", nil))
	if response.Code != http.StatusTemporaryRedirect || response.Body.String() != "redirect body" || called {
		t.Fatalf("redirect response=%d body=%q other called=%v", response.Code, response.Body.String(), called)
	}
}

func TestHealthDoesNotExposeTargetsOrKeys(t *testing.T) {
	h := testRelay(config{Routes: map[string]route{"jev": {Target: "https://private.example/v1/systemone", Key: "jev"}}}, "local", new(bytes.Buffer))
	response := httptest.NewRecorder()
	h.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/health", nil))
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || body["ok"] != true || body["mode"] != "local" || strings.Contains(response.Body.String(), "private.example") || strings.Contains(response.Body.String(), "key") {
		t.Fatalf("health response: %d %s", response.Code, response.Body.String())
	}
}

func TestLoopbackOnly(t *testing.T) {
	for _, address := range []string{"127.0.0.1:8765", "[::1]:8765", "127.4.5.6:1234"} {
		if err := checkLoopback(address); err != nil {
			t.Errorf("%s rejected: %v", address, err)
		}
	}
	for _, address := range []string{"0.0.0.0:8765", "[::]:8765", "192.168.1.1:8765", "localhost:8765", ":8765", "127.0.0.1:0"} {
		if err := checkLoopback(address); err == nil {
			t.Errorf("%s accepted", address)
		}
	}
}

// layaServer answers /load with the given report (or 404 when empty) and POSTs
// with the given status, counting the POSTs it receives.
func layaServer(t *testing.T, load string, status int, posts *int) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/load" {
			if load == "" {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte(load))
			return
		}
		*posts++
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"answers":{}}`))
	}))
	t.Cleanup(s.Close)
	return s
}

func postLaya(h *relay) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	h.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/systemone/laya", strings.NewReader("{}")))
	return response
}

func TestFallbackPassesOverABusyServer(t *testing.T) {
	var firstPosts, secondPosts int
	first := layaServer(t, `{"busy":true,"ready":true}`, http.StatusOK, &firstPosts)
	second := layaServer(t, `{"busy":false,"ready":true}`, http.StatusOK, &secondPosts)
	var logs bytes.Buffer
	h := testRelay(config{Routes: map[string]route{"laya": {Target: first.URL + "/v1/systemone", Fallbacks: []string{second.URL + "/v1/systemone"}}}}, "local", &logs)
	if response := postLaya(h); response.Code != http.StatusOK {
		t.Fatalf("status %d: %s", response.Code, response.Body.String())
	}
	if firstPosts != 0 || secondPosts != 1 {
		t.Fatalf("posts: first=%d second=%d", firstPosts, secondPosts)
	}
	if !strings.Contains(logs.String(), "target="+second.URL) {
		t.Fatalf("log does not name the server that answered: %s", logs.String())
	}
}

func TestFallbackMovesOnFromAServerErrorAndRemembersIt(t *testing.T) {
	var firstPosts, secondPosts int
	first := layaServer(t, "", http.StatusServiceUnavailable, &firstPosts)
	second := layaServer(t, "", http.StatusOK, &secondPosts)
	h := testRelay(config{Routes: map[string]route{"laya": {Target: first.URL + "/v1/systemone", Fallbacks: []string{second.URL + "/v1/systemone"}}}}, "local", new(bytes.Buffer))
	for i := 0; i < 2; i++ {
		if response := postLaya(h); response.Code != http.StatusOK {
			t.Fatalf("request %d status %d", i, response.Code)
		}
	}
	if firstPosts != 1 || secondPosts != 2 {
		t.Fatalf("posts: first=%d second=%d; the busy server should be skipped the second time", firstPosts, secondPosts)
	}
}

func TestFallbackNeverRetriesARefusedRequest(t *testing.T) {
	var firstPosts, secondPosts int
	first := layaServer(t, "", http.StatusBadRequest, &firstPosts)
	second := layaServer(t, "", http.StatusOK, &secondPosts)
	h := testRelay(config{Routes: map[string]route{"laya": {Target: first.URL + "/v1/systemone", Fallbacks: []string{second.URL + "/v1/systemone"}}}}, "local", new(bytes.Buffer))
	if response := postLaya(h); response.Code != http.StatusBadRequest || secondPosts != 0 {
		t.Fatalf("status %d, second posts %d", response.Code, secondPosts)
	}
}

func TestFallbackNamesEveryServerWhenNoneAnswers(t *testing.T) {
	h := testRelay(config{Routes: map[string]route{"laya": {Target: "http://127.0.0.1:1/v1/systemone", Fallbacks: []string{"http://127.0.0.1:2/v1/systemone"}}}}, "local", new(bytes.Buffer))
	response := postLaya(h)
	if response.Code != http.StatusBadGateway || !strings.Contains(response.Body.String(), "127.0.0.1:1") || !strings.Contains(response.Body.String(), "127.0.0.1:2") {
		t.Fatalf("status %d: %s", response.Code, response.Body.String())
	}
}

func TestConfigRejectsABadFallback(t *testing.T) {
	cfg := config{Routes: map[string]route{"laya": {Target: "http://localhost:8010/v1/systemone", Fallbacks: []string{"ftp://nope"}}}}
	if cfg.validate() == nil {
		t.Fatal("a non-http fallback was accepted")
	}
}
