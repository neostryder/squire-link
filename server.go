package main

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
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
}

func newRelay(cfg config, configFile, mode string, keys secretStore, logger *log.Logger) *relay {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &relay{
		cfg: cfg, configFile: configFile, mode: mode, keys: keys, logger: logger,
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
	defer func() {
		h.logger.Printf("time=%s route=%s status=%d latency=%s bytes=%d", start.UTC().Format(time.RFC3339), name, status, time.Since(start).Round(time.Millisecond), size)
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
	upstream, err := http.NewRequestWithContext(r.Context(), http.MethodPost, selected.Target, bytes.NewReader(body))
	if err != nil {
		writeError(w, status, "invalid configured target")
		return
	}
	upstream.Header.Set("Content-Type", "application/json")
	if selected.Key != "" {
		secret, err := h.keys.Get("squire-link", selected.Key)
		if err != nil {
			status = http.StatusServiceUnavailable
			writeError(w, status, "route key is unavailable in the OS keychain")
			return
		}
		upstream.Header.Set("Authorization", "Bearer "+secret)
	}
	response, err := h.client.Do(upstream)
	if err != nil {
		writeError(w, status, "upstream request failed")
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

func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Error string `json:"error"`
	}{message})
}
