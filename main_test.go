package main

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"
)

func TestTokenAndKeyCommands(t *testing.T) {
	keyring.MockInit()
	var out, errOut bytes.Buffer
	if err := runCLI([]string{"token"}, strings.NewReader(""), &out, &errOut, osKeyring{}); err != nil {
		t.Fatal(err)
	}
	token := strings.TrimSpace(out.String())
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != 32 {
		t.Fatalf("token is not 32 URL-safe random bytes: %q", token)
	}
	out.Reset()
	if err := runCLI([]string{"token"}, strings.NewReader(""), &out, &errOut, osKeyring{}); err != nil || strings.TrimSpace(out.String()) != token {
		t.Fatalf("token was not persisted: %v", err)
	}
	out.Reset()
	secret := "test-key-secret"
	if err := runCLI([]string{"key", "set", "jev"}, strings.NewReader(secret+"\n"), &out, &errOut, osKeyring{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), secret) || !strings.Contains(out.String(), "15 bytes") {
		t.Fatalf("key set output: %q", out.String())
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"routes":{"jev":{"target":"https://example.com/v1/systemone","key":"jev"}},"origins":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runCLI([]string{"key", "list", "--config", path}, strings.NewReader(""), &out, &errOut, osKeyring{}); err != nil {
		t.Fatal(err)
	}
	if out.String() != "jev: set\n" {
		t.Fatalf("key list output: %q", out.String())
	}
	if got, err := keyring.Get("squire-link", "jev"); err != nil || got != secret {
		t.Fatalf("stored key not found: %v", err)
	}
}

func TestDefaultConfigCreation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "squire-link", "config.json")
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Routes) != 2 || len(cfg.Origins) != 4 {
		t.Fatalf("wrong defaults: %+v", cfg)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "Bearer") || strings.Contains(string(data), "link-token") {
		t.Fatal("config unexpectedly contains a secret")
	}
}
