package config

import (
	"os"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	// Clear env vars that might interfere
	os.Unsetenv("DB_PATH")
	os.Unsetenv("API_PORT")
	os.Unsetenv("SLACK_BOT_TOKEN")

	cfg := Load()
	if cfg.DBPath != "xylolabs-kb.db" {
		t.Errorf("DBPath = %q, want default", cfg.DBPath)
	}
	if cfg.APIPort != 8080 {
		t.Errorf("APIPort = %d, want 8080", cfg.APIPort)
	}
	if cfg.Timezone != "Asia/Seoul" {
		t.Errorf("Timezone = %q, want Asia/Seoul", cfg.Timezone)
	}
}

func TestSlackEnabled(t *testing.T) {
	cfg := &Config{}
	if cfg.SlackEnabled() {
		t.Error("SlackEnabled() should be false with empty tokens")
	}
	cfg.SlackBotToken = "xoxb-test"
	cfg.SlackAppToken = "xapp-test"
	if !cfg.SlackEnabled() {
		t.Error("SlackEnabled() should be true with both tokens set")
	}
}

func TestGeminiEnabled(t *testing.T) {
	cfg := &Config{}
	if cfg.GeminiEnabled() {
		t.Error("GeminiEnabled() should be false without API key")
	}
	cfg.GeminiAPIKey = "test-key"
	if !cfg.GeminiEnabled() {
		t.Error("GeminiEnabled() should be true with API key")
	}
}

func TestLocation(t *testing.T) {
	cfg := &Config{Timezone: "Asia/Seoul"}
	loc := cfg.Location()
	if loc.String() != "Asia/Seoul" {
		t.Errorf("Location() = %q, want Asia/Seoul", loc.String())
	}

	cfg.Timezone = "Invalid/Zone"
	loc = cfg.Location()
	if loc.String() != "UTC" {
		t.Errorf("Location() should fall back to UTC, got %q", loc.String())
	}
}

func TestEnvOrDefault(t *testing.T) {
	os.Setenv("TEST_VAR_XKE", "custom")
	defer os.Unsetenv("TEST_VAR_XKE")

	got := envOrDefault("TEST_VAR_XKE", "default")
	if got != "custom" {
		t.Errorf("got %q, want %q", got, "custom")
	}

	got = envOrDefault("NONEXISTENT_VAR_XKE", "fallback")
	if got != "fallback" {
		t.Errorf("got %q, want %q", got, "fallback")
	}
}
