package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/zalando/go-keyring"
)

const maxBodyBytes = 1 << 20

type secretStore interface {
	Get(service, user string) (string, error)
	Set(service, user, secret string) error
	Delete(service, user string) error
}

type osKeyring struct{}

func (osKeyring) Get(service, user string) (string, error) { return keyring.Get(service, user) }
func (osKeyring) Set(service, user, secret string) error   { return keyring.Set(service, user, secret) }
func (osKeyring) Delete(service, user string) error        { return keyring.Delete(service, user) }

type relay struct {
	cfg        config
	configFile string
	mode       string
	keys       secretStore
	client     *http.Client
	logger     *log.Logger
	now        func() time.Time
	orders     *orderQueue

	// skipMu guards skip: the servers recently found busy or not answering,
	// and until when to pass them over.
	skipMu sync.Mutex
	skip   map[string]skipped
}

type skipped struct {
	until  time.Time
	reason string
}

const (
	busySkip     = 5 * time.Second
	downSkip     = 30 * time.Second
	probeTimeout = 600 * time.Millisecond
)

func newRelay(cfg config, configFile, mode string, keys secretStore, logger *log.Logger) *relay {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &relay{
		cfg: cfg, configFile: configFile, mode: mode, keys: keys, logger: logger,
		now:    time.Now,
		orders: newOrderQueue(cfg.Channel, time.Now, logger),
		skip:   map[string]skipped{},
		client: &http.Client{
			Timeout:       30 * time.Second,
			Transport:     transport,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

func (h *relay) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("Vary", "Origin")
	origin := r.Header.Get("Origin")
	if origin != "" {
		if !originAllowed(origin, h.cfg.Origins) {
			writeError(w, http.StatusForbidden, fmt.Sprintf("origin %q is not allowed; edit %s", origin, h.configFile))
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", origin)
	}
	if r.Method == http.MethodOptions {
		if origin == "" {
			writeError(w, http.StatusForbidden, "preflight requires an allowed Origin")
			return
		}
		w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Squire-Link-Token")
		w.Header().Set("Access-Control-Allow-Private-Network", "true")
		w.Header().Set("Access-Control-Max-Age", "600")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if h.mode == "serve" && origin == "" && !h.validToken(r.Header.Get("X-Squire-Link-Token")) {
		writeError(w, http.StatusForbidden, "serve mode requires a valid X-Squire-Link-Token")
		return
	}
	if r.URL.Path == "/health" {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "GET required")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			OK      bool     `json:"ok"`
			Mode    string   `json:"mode"`
			Routes  []string `json:"routes"`
			Version string   `json:"version"`
		}{true, h.mode, routeNames(h.cfg.Routes), version})
		return
	}
	if r.URL.Path == "/v1/orders" {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "GET required")
			return
		}
		if h.mode == "serve" && !h.validToken(r.Header.Get("X-Squire-Link-Token")) {
			writeError(w, http.StatusForbidden, "serve mode requires a valid X-Squire-Link-Token")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		orders, more := h.orders.drain(orderPageSize)
		_ = json.NewEncoder(w).Encode(struct {
			Orders []chatOrder `json:"orders"`
			More   bool        `json:"more"`
		}{orders, more})
		return
	}
	const prefix = "/v1/systemone/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		writeError(w, http.StatusNotFound, "unknown path")
		return
	}
	name := strings.TrimPrefix(r.URL.Path, prefix)
	selected, ok := h.cfg.Routes[name]
	if !ok {
		writeError(w, http.StatusNotFound, "route is not configured")
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	if h.mode == "serve" && !h.validToken(r.Header.Get("X-Squire-Link-Token")) {
		writeError(w, http.StatusForbidden, "serve mode requires a valid X-Squire-Link-Token")
		return
	}
	h.relayRequest(w, r, name, selected)
}

func (h *relay) validToken(provided string) bool {
	if provided == "" {
		return false
	}
	stored, err := h.keys.Get("squire-link", "link-token")
	if err != nil || stored == "" {
		return false
	}
	a := sha256.Sum256([]byte(provided))
	b := sha256.Sum256([]byte(stored))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

func (h *relay) relayRequest(w http.ResponseWriter, r *http.Request, name string, selected route) {
	start := time.Now()
	status := http.StatusBadGateway
	var size int64
	answered := ""
	defer func() {
		h.logger.Printf("time=%s route=%s status=%d latency=%s bytes=%d target=%s", start.UTC().Format(time.RFC3339), name, status, time.Since(start).Round(time.Millisecond), size, answered)
	}()
	if r.ContentLength > maxBodyBytes {
		status = http.StatusRequestEntityTooLarge
		writeError(w, status, "request body exceeds 1 MiB")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			status = http.StatusRequestEntityTooLarge
			writeError(w, status, "request body exceeds 1 MiB")
		} else {
			status = http.StatusBadRequest
			writeError(w, status, "cannot read request body")
		}
		return
	}
	secret := ""
	if selected.Key != "" {
		var err error
		secret, err = h.keys.Get("squire-link", selected.Key)
		if err != nil {
			status = http.StatusServiceUnavailable
			writeError(w, status, "route key is unavailable in the OS keychain")
			return
		}
	}
	targets := append([]string{selected.Target}, selected.Fallbacks...)
	pooled := len(targets) > 1
	var response *http.Response
	var passed []string
	for _, target := range targets {
		if pooled {
			if reason, ok := h.skipping(target); ok {
				passed = append(passed, target+" "+reason+" (recently)")
				continue
			}
			if reason, wait := h.busy(r, target); reason != "" {
				h.remember(target, reason, wait)
				passed = append(passed, target+" "+reason)
				continue
			}
		}
		upstream, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target, bytes.NewReader(body))
		if err != nil {
			writeError(w, status, "invalid configured target")
			return
		}
		upstream.Header.Set("Content-Type", "application/json")
		if secret != "" {
			upstream.Header.Set("Authorization", "Bearer "+secret)
		}
		got, err := h.client.Do(upstream)
		if err != nil {
			if !pooled {
				writeError(w, status, "upstream request failed")
				return
			}
			h.remember(target, "not answering", downSkip)
			passed = append(passed, target+" not answering")
			continue
		}
		// A server error is worth trying elsewhere. A refusal of the request
		// itself (4xx) would be refused by another server too.
		if pooled && got.StatusCode >= 500 {
			got.Body.Close()
			h.remember(target, fmt.Sprintf("error %d", got.StatusCode), busySkip)
			passed = append(passed, fmt.Sprintf("%s error %d", target, got.StatusCode))
			continue
		}
		response, answered = got, target
		break
	}
	if response == nil {
		writeError(w, status, "no server could take the request: "+strings.Join(passed, "; "))
		return
	}
	defer response.Body.Close()
	status = response.StatusCode
	if contentType := response.Header.Get("Content-Type"); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	w.WriteHeader(status)
	size, _ = io.Copy(w, response.Body)
}

func (h *relay) skipping(target string) (string, bool) {
	h.skipMu.Lock()
	defer h.skipMu.Unlock()
	s, ok := h.skip[target]
	if !ok || !h.now().Before(s.until) {
		return "", false
	}
	return s.reason, true
}

func (h *relay) remember(target, reason string, wait time.Duration) {
	h.skipMu.Lock()
	defer h.skipMu.Unlock()
	h.skip[target] = skipped{until: h.now().Add(wait), reason: reason}
}

// busy reads a Laya server's /load report to see whether it can take a
// request. An empty reason means go ahead; a server with no load report is
// taken as ready.
func (h *relay) busy(r *http.Request, target string) (string, time.Duration) {
	u, err := url.Parse(target)
	if err != nil {
		return "invalid address", downSkip
	}
	probe := url.URL{Scheme: u.Scheme, Host: u.Host, Path: "/load"}
	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, probe.String(), nil)
	if err != nil {
		return "invalid address", downSkip
	}
	response, err := h.client.Do(request)
	if err != nil {
		return "not answering", downSkip
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return "", 0
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Sprintf("load check answered %d", response.StatusCode), downSkip
	}
	var load struct {
		Busy  bool  `json:"busy"`
		Ready *bool `json:"ready"`
	}
	if json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&load) != nil {
		return "", 0
	}
	if load.Busy {
		return "busy", busySkip
	}
	if load.Ready != nil && !*load.Ready {
		return "not ready", busySkip
	}
	return "", 0
}

func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Error string `json:"error"`
	}{message})
}
