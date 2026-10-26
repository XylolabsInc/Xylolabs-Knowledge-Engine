package config

import (
	"os"
	"reflect"
	"strings"
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

func TestSplitCommand(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    []string
		wantErr bool
	}{
		{"default", DefaultDaouOfficeCommand, []string{"node", "--disable-warning=ExperimentalWarning", "plugins/daouoffice/src/server.mjs"}, false},
		{"host command with extra spaces", "  /opt/node/bin/node   --disable-warning=ExperimentalWarning /opt/xylolabs-kb/plugins/daouoffice/src/server.mjs ",
			[]string{"/opt/node/bin/node", "--disable-warning=ExperimentalWarning", "/opt/xylolabs-kb/plugins/daouoffice/src/server.mjs"}, false},
		{"double quotes keep spaces", `node "/opt/my plugins/server.mjs"`, []string{"node", "/opt/my plugins/server.mjs"}, false},
		{"single quotes are literal", `node '/a b/$HOME\x.mjs'`, []string{"node", `/a b/$HOME\x.mjs`}, false},
		{"escapes in double quotes", `a "say \"hi\" \\ \n"`, []string{"a", `say "hi" \ \n`}, false},
		{"backslash outside quotes", `a b\ c`, []string{"a", "b c"}, false},
		{"adjacent quotes join", `a"b"'c'`, []string{"abc"}, false},
		{"empty quotes are an argument", `a ""`, []string{"a", ""}, false},
		{"shell operators stay literal", `node x.mjs; rm -rf / && $(id)`, []string{"node", "x.mjs;", "rm", "-rf", "/", "&&", "$(id)"}, false},
		{"empty", "   ", nil, false},
		{"unterminated single", `node 'x`, nil, true},
		{"unterminated double", `node "x`, nil, true},
		{"trailing backslash", `node x\`, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := SplitCommand(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("SplitCommand(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
			if !tt.wantErr && !reflect.DeepEqual(got, tt.want) {
				t.Errorf("SplitCommand(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestPluginConfig(t *testing.T) {
	for _, k := range []string{"PLUGIN_SECRET_KEY", "DAOUOFFICE_ENABLED", "DAOUOFFICE_COMMAND", "DAOUOFFICE_DATA_DIR", "DAOUOFFICE_CONFIG"} {
		t.Setenv(k, "")
	}
	cfg := Load()
	if cfg.DaouOfficeEnabled || cfg.PluginSecretKey != "" || cfg.DaouOfficeCommand != DefaultDaouOfficeCommand ||
		cfg.DaouOfficeDataDir != "data/plugins/daouoffice" || cfg.DaouOfficeConfig != "configs/daouoffice.json" {
		t.Errorf("defaults = %+v", cfg)
	}

	t.Setenv("DAOUOFFICE_ENABLED", "true")
	t.Setenv("DAOUOFFICE_COMMAND", "/opt/node/bin/node /opt/x/server.mjs")
	cfg = Load()
	argv, err := cfg.DaouOfficeArgv()
	if !cfg.DaouOfficeEnabled || err != nil || !reflect.DeepEqual(argv, []string{"/opt/node/bin/node", "/opt/x/server.mjs"}) {
		t.Errorf("enabled = %v, argv = %q, err = %v", cfg.DaouOfficeEnabled, argv, err)
	}

	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"disabled needs nothing", func(c *Config) {}, ""},
		{"enabled without a key", func(c *Config) {
			c.DaouOfficeEnabled, c.DaouOfficeCommand, c.DaouOfficeConfig = true, "node x.mjs", "config_test.go"
		}, "PLUGIN_SECRET_KEY"},
		{"enabled with a broken command", func(c *Config) {
			c.DaouOfficeEnabled, c.PluginSecretKey, c.DaouOfficeCommand, c.DaouOfficeConfig = true, "k", `node "x`, "config_test.go"
		}, "DAOUOFFICE_COMMAND"},
		{"enabled without the config file", func(c *Config) {
			c.DaouOfficeEnabled, c.PluginSecretKey, c.DaouOfficeCommand, c.DaouOfficeConfig = true, "k", "node x.mjs", "missing.json"
		}, "DAOUOFFICE_CONFIG"},
		{"enabled and complete", func(c *Config) {
			c.DaouOfficeEnabled, c.PluginSecretKey, c.DaouOfficeCommand, c.DaouOfficeConfig = true, "k", "node x.mjs", "config_test.go"
		}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := minimalValidConfig()
			tt.mutate(c)
			errs := c.Validate()
			joined := strings.Join(errs, "; ")
			if tt.wantErr == "" && len(errs) != 0 || tt.wantErr != "" && !strings.Contains(joined, tt.wantErr) {
				t.Errorf("Validate() = %q, want mention of %q", joined, tt.wantErr)
			}
		})
	}
}
