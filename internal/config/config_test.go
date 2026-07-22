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

// minimalValidConfig returns the smallest Config that passes Validate() with
// no errors, so tests can tweak a single field under test in isolation.
func minimalValidConfig() *Config {
	return &Config{
		DBPath:   "test.db",
		Timezone: "UTC",
		APIPort:  8080,
	}
}

func TestLLMKeyFallback(t *testing.T) {
	tests := []struct {
		name   string
		llmKey string
		gemKey string
		want   string
	}{
		{"llm key wins", "or-key", "g-key", "or-key"},
		{"falls back to gemini", "", "g-key", "g-key"},
		{"both empty", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Config{LLMAPIKey: tt.llmKey, GeminiAPIKey: tt.gemKey}
			if got := c.LLMKey(); got != tt.want {
				t.Errorf("LLMKey() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestValidateLLMEndpoint(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		key      string
		wantErr  bool
	}{
		{"unset is fine", "", "", false},
		{"https ok", "https://openrouter.ai/api/v1/chat/completions", "k", false},
		{"http ok", "http://localhost:8080/v1/chat/completions", "k", false},
		{"bad scheme", "ftp://openrouter.ai/x", "k", true},
		{"not a url", "openrouter.ai", "k", true},
		{"endpoint without any key", "https://openrouter.ai/api/v1/chat/completions", "", true},
		{"key without endpoint", "", "k", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := minimalValidConfig()
			c.LLMEndpoint = tt.endpoint
			c.LLMAPIKey = tt.key
			err := c.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
