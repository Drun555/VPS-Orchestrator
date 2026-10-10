package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigFrom(t *testing.T) {
	temporary := t.TempDir()
	path := filepath.Join(temporary, ".env")
	content := "# comment\nPORT=43123\nBASE_URL='https://example.org/root/'\nCERTBOT_EMAIL=admin@example.org\nLISTEN_ADDRESS=127.0.0.1\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := loadConfigFrom(path, temporary)
	if err != nil {
		t.Fatal(err)
	}
	if config.Port != 43123 || config.BaseURL != "https://example.org/root" || config.ListenAddress != "127.0.0.1" {
		t.Fatalf("unexpected config: %+v", config)
	}
}

func TestLoadConfigRejectsUnsafeBaseURL(t *testing.T) {
	temporary := t.TempDir()
	path := filepath.Join(temporary, ".env")
	if err := os.WriteFile(path, []byte("BASE_URL=https://user:pass@example.org?x=1\nCERTBOT_EMAIL=a@example.org\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfigFrom(path, temporary); err == nil {
		t.Fatal("expected invalid BASE_URL error")
	}
}

func TestParseSSHAddress(t *testing.T) {
	tests := map[string]string{
		"192.0.2.1":        "192.0.2.1:22",
		"example.org:2200": "example.org:2200",
		"2001:db8::1":      "[2001:db8::1]:22",
	}
	for input, expected := range tests {
		_, _, normalized, err := parseSSHAddress(input)
		if err != nil {
			t.Fatalf("%s: %v", input, err)
		}
		if normalized != expected {
			t.Fatalf("%s: got %s, want %s", input, normalized, expected)
		}
	}
}

func TestEnvironmentWithoutEnvFile(t *testing.T) {
	t.Setenv("BASE_URL", "https://env.example.org")
	t.Setenv("CERTBOT_EMAIL", "env@example.org")
	t.Setenv("DATA_DIR", t.TempDir())
	t.Setenv("PORT", "4567")
	config, err := loadConfigFrom(filepath.Join(t.TempDir(), "missing"), "/unused")
	if err != nil {
		t.Fatal(err)
	}
	if config.BaseURL != "https://env.example.org" || config.Port != 4567 || config.DataDir != os.Getenv("DATA_DIR") {
		t.Fatalf("bad environment config: %+v", config)
	}
}
