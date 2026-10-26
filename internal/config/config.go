package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config holds all application configuration.
type Config struct {
	// Database
	DBPath         string
	AttachmentPath string

	// Slack
	SlackBotToken   string
	SlackAppToken   string
	SlackSignSecret string

	// Google Workspace
	GoogleCredsFile         string
	GoogleTokenFile         string
	GoogleScopes            []string
	GoogleDriveFolders      []string
	GoogleImpersonateEmail  string
	GoogleDefaultCalendarID string

	// Discord
	DiscordBotToken string
	DiscordGuildID  string

	// Notion
	NotionAPIKey    string
	NotionRootPages []string

	// Gemini AI
	GeminiAPIKey   string
	GeminiModel    string
	GeminiProModel string
	// LLMEndpoint, when set, switches the LLM client to an OpenAI-compatible
	// chat/completions endpoint (full URL, e.g. OpenRouter). Empty = native Gemini.
	LLMEndpoint string
	// LLMAPIKey is the bearer key for LLMEndpoint; falls back to GeminiAPIKey.
	LLMAPIKey string

	// Bot
	SystemPromptFile string
	Language         string
	Timezone         string

	// Knowledge Base Repo
	KBRepoDir string

	// API Server
	APIHost string
	APIPort int

	// Console Auth
	ConsoleUsername string
	ConsolePassword string

	// Sync intervals
	SlackSyncInterval   time.Duration
	GoogleSyncInterval  time.Duration
	NotionSyncInterval  time.Duration
	DiscordSyncInterval time.Duration

	// Logging
	LogLevel string

	// Plugins
	// PluginSecretKey is the base64 of 32 random bytes that seals plugin
	// account credentials. Empty disables plugins that need accounts.
	PluginSecretKey   string
	DaouOfficeEnabled bool
	// DaouOfficeCommand is the plugin's command line, split on spaces with
	// shell-style quoting but never run through a shell.
	DaouOfficeCommand string
	DaouOfficeDataDir string
	DaouOfficeConfig  string
}

// DefaultDaouOfficeCommand starts the plugin from a checkout of this repository.
const DefaultDaouOfficeCommand = "node --disable-warning=ExperimentalWarning plugins/daouoffice/src/server.mjs"

// Load reads configuration from environment variables with sensible defaults.
func Load() *Config {
	cfg := &Config{
		DBPath:         envOrDefault("DB_PATH", "xylolabs-kb.db"),
		AttachmentPath: envOrDefault("ATTACHMENT_PATH", "attachments"),

		SlackBotToken:   os.Getenv("SLACK_BOT_TOKEN"),
		SlackAppToken:   os.Getenv("SLACK_APP_TOKEN"),
		SlackSignSecret: os.Getenv("SLACK_SIGNING_SECRET"),

		GoogleCredsFile:    envOrDefault("GOOGLE_CREDENTIALS_FILE", "credentials.json"),
		GoogleTokenFile:    envOrDefault("GOOGLE_TOKEN_FILE", "token.json"),
		GoogleDriveFolders: splitEnv("GOOGLE_DRIVE_FOLDERS", nil),
		GoogleScopes: splitEnv("GOOGLE_SCOPES", []string{
			"https://www.googleapis.com/auth/drive",
			"https://www.googleapis.com/auth/documents",
			"https://www.googleapis.com/auth/spreadsheets",
			"https://www.googleapis.com/auth/presentations",
			"https://www.googleapis.com/auth/calendar",
			"https://www.googleapis.com/auth/tasks",
			"https://www.googleapis.com/auth/gmail.send",
		}),
		GoogleImpersonateEmail:  os.Getenv("GOOGLE_IMPERSONATE_EMAIL"),
		GoogleDefaultCalendarID: os.Getenv("GOOGLE_DEFAULT_CALENDAR_ID"),

		DiscordBotToken: os.Getenv("DISCORD_BOT_TOKEN"),
		DiscordGuildID:  os.Getenv("DISCORD_GUILD_ID"),

		NotionAPIKey:    os.Getenv("NOTION_API_KEY"),
		NotionRootPages: splitEnv("NOTION_ROOT_PAGES", nil),

		GeminiAPIKey:   os.Getenv("GEMINI_API_KEY"),
		GeminiModel:    envOrDefault("GEMINI_MODEL", "gemini-3.8-flash"),
		GeminiProModel: envOrDefault("GEMINI_PRO_MODEL", "gemini-3.8-flash"),
		LLMEndpoint:    os.Getenv("LLM_ENDPOINT"),
		LLMAPIKey:      os.Getenv("LLM_API_KEY"),

		SystemPromptFile: envOrDefault("SYSTEM_PROMPT_FILE", ""),
		Language:         envOrDefault("LANGUAGE", "en"),
		Timezone:         envOrDefault("TIMEZONE", "Asia/Seoul"),

		KBRepoDir: envOrDefault("KB_REPO_DIR", ""),

		APIHost: envOrDefault("API_HOST", "0.0.0.0"),
		APIPort: envOrDefaultInt("API_PORT", 8080),

		ConsoleUsername: envOrDefault("CONSOLE_USERNAME", "admin"),
		ConsolePassword: envOrDefault("CONSOLE_PASSWORD", ""),

		SlackSyncInterval:   envOrDefaultDuration("SLACK_SYNC_INTERVAL", 1*time.Hour),
		GoogleSyncInterval:  envOrDefaultDuration("GOOGLE_SYNC_INTERVAL", 15*time.Minute),
		NotionSyncInterval:  envOrDefaultDuration("NOTION_SYNC_INTERVAL", 10*time.Minute),
		DiscordSyncInterval: envOrDefaultDuration("DISCORD_SYNC_INTERVAL", 5*time.Minute),

		LogLevel: envOrDefault("LOG_LEVEL", "info"),

		PluginSecretKey:   os.Getenv("PLUGIN_SECRET_KEY"),
		DaouOfficeEnabled: envBool("DAOUOFFICE_ENABLED"),
		DaouOfficeCommand: envOrDefault("DAOUOFFICE_COMMAND", DefaultDaouOfficeCommand),
		DaouOfficeDataDir: envOrDefault("DAOUOFFICE_DATA_DIR", "data/plugins/daouoffice"),
		DaouOfficeConfig:  envOrDefault("DAOUOFFICE_CONFIG", "configs/daouoffice.json"),
	}
	return cfg
}

// DaouOfficeArgv splits DaouOfficeCommand into arguments.
func (c *Config) DaouOfficeArgv() ([]string, error) {
	argv, err := SplitCommand(c.DaouOfficeCommand)
	if err != nil {
		return nil, fmt.Errorf("DAOUOFFICE_COMMAND: %w", err)
	}
	if len(argv) == 0 {
		return nil, fmt.Errorf("DAOUOFFICE_COMMAND is empty")
	}
	return argv, nil
}

// SplitCommand splits a command line into arguments the way a POSIX shell
// would for plain words: whitespace separates arguments, single quotes keep
// everything literally, double quotes keep spaces and honor \" and \\, and a
// backslash outside quotes escapes the next character. Nothing is expanded
// ($VAR, globs, ~) and there are no operators, because the result is
// executed directly, never by a shell.
func SplitCommand(s string) ([]string, error) {
	var args []string
	var cur strings.Builder
	inWord := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch == '\'':
			end := strings.IndexByte(s[i+1:], '\'')
			if end < 0 {
				return nil, fmt.Errorf("unterminated single quote")
			}
			cur.WriteString(s[i+1 : i+1+end])
			i += end + 1
			inWord = true
		case ch == '"':
			i++
			for ; i < len(s) && s[i] != '"'; i++ {
				if s[i] == '\\' && i+1 < len(s) && (s[i+1] == '"' || s[i+1] == '\\') {
					i++
				}
				cur.WriteByte(s[i])
			}
			if i >= len(s) {
				return nil, fmt.Errorf("unterminated double quote")
			}
			inWord = true
		case ch == '\\':
			if i+1 >= len(s) {
				return nil, fmt.Errorf("trailing backslash")
			}
			i++
			cur.WriteByte(s[i])
			inWord = true
		case ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r':
			if inWord {
				args = append(args, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteByte(ch)
			inWord = true
		}
	}
	if inWord {
		args = append(args, cur.String())
	}
	return args, nil
}

// SlackEnabled returns true if Slack credentials are configured.
func (c *Config) SlackEnabled() bool {
	return c.SlackBotToken != "" && c.SlackAppToken != ""
}

// GoogleEnabled returns true if Google credentials are configured.
func (c *Config) GoogleEnabled() bool {
	return c.GoogleCredsFile != "" && fileExists(c.GoogleCredsFile)
}

// DiscordEnabled returns true if Discord credentials are configured.
func (c *Config) DiscordEnabled() bool {
	return c.DiscordBotToken != "" && c.DiscordGuildID != ""
}

// NotionEnabled returns true if Notion credentials are configured.
func (c *Config) NotionEnabled() bool {
	return c.NotionAPIKey != ""
}

// GeminiEnabled returns true if Gemini API key is configured.
func (c *Config) GeminiEnabled() bool {
	return c.LLMKey() != ""
}

// LLMKey returns the bearer key for the configured LLM endpoint:
// LLM_API_KEY when set, else GEMINI_API_KEY.
func (c *Config) LLMKey() string {
	if c.LLMAPIKey != "" {
		return c.LLMAPIKey
	}
	return c.GeminiAPIKey
}

// ConsoleAuthEnabled returns true if console password is set.
func (c *Config) ConsoleAuthEnabled() bool {
	return c.ConsolePassword != ""
}

// Validate checks that the configuration is usable and returns any errors found.
func (c *Config) Validate() []string {
	var errs []string

	// Check DB directory is writable
	dbDir := filepath.Dir(c.DBPath)
	if dbDir != "" && dbDir != "." {
		if info, err := os.Stat(dbDir); err != nil {
			errs = append(errs, fmt.Sprintf("DB_PATH directory %q does not exist", dbDir))
		} else if !info.IsDir() {
			errs = append(errs, fmt.Sprintf("DB_PATH directory %q is not a directory", dbDir))
		}
	}

	// Check timezone is valid
	if _, err := time.LoadLocation(c.Timezone); err != nil {
		errs = append(errs, fmt.Sprintf("invalid TIMEZONE %q: %v", c.Timezone, err))
	}

	// Check API port range
	if c.APIPort < 1 || c.APIPort > 65535 {
		errs = append(errs, fmt.Sprintf("API_PORT %d is out of range (1-65535)", c.APIPort))
	}

	// Check Gemini model is set if API key is provided
	if c.GeminiAPIKey != "" && c.GeminiModel == "" {
		errs = append(errs, "GEMINI_API_KEY is set but GEMINI_MODEL is empty")
	}

	// Check LLM endpoint is a valid http(s) URL and a key is available
	if c.LLMEndpoint != "" {
		u, err := url.Parse(c.LLMEndpoint)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, fmt.Sprintf("LLM_ENDPOINT %q is not an http(s) URL", c.LLMEndpoint))
		}
		if c.LLMKey() == "" {
			errs = append(errs, "LLM_ENDPOINT is set but neither LLM_API_KEY nor GEMINI_API_KEY is")
		}
	}

	// Check LLM_API_KEY isn't set without an endpoint (it would silently go
	// unused, or worse, get sent to the native Gemini endpoint instead)
	if c.LLMAPIKey != "" && c.LLMEndpoint == "" {
		errs = append(errs, "LLM_API_KEY is set but LLM_ENDPOINT is not — the key would be sent to the native Gemini endpoint")
	}

	// Check system prompt file exists if specified
	if c.SystemPromptFile != "" {
		if _, err := os.Stat(c.SystemPromptFile); err != nil {
			errs = append(errs, fmt.Sprintf("SYSTEM_PROMPT_FILE %q not found: %v", c.SystemPromptFile, err))
		}
	}

	// Check the plugin settings when the DaouOffice plugin is turned on
	if c.DaouOfficeEnabled {
		if c.PluginSecretKey == "" {
			errs = append(errs, "DAOUOFFICE_ENABLED is set but PLUGIN_SECRET_KEY is not, so the plugin stays off")
		}
		if _, err := c.DaouOfficeArgv(); err != nil {
			errs = append(errs, err.Error())
		}
		if !fileExists(c.DaouOfficeConfig) {
			errs = append(errs, fmt.Sprintf("DAOUOFFICE_CONFIG %q not found; the plugin starts with empty company rules", c.DaouOfficeConfig))
		}
	}

	// Check Google creds file exists if impersonate email is set
	if c.GoogleImpersonateEmail != "" && !fileExists(c.GoogleCredsFile) {
		errs = append(errs, fmt.Sprintf("GOOGLE_IMPERSONATE_EMAIL is set but credentials file %q not found", c.GoogleCredsFile))
	}

	return errs
}

// Location returns the configured timezone as a *time.Location.
// Falls back to UTC if the timezone string is invalid.
func (c *Config) Location() *time.Location {
	loc, err := time.LoadLocation(c.Timezone)
	if err != nil {
		return time.UTC
	}
	return loc
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envBool(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func envOrDefaultInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func envOrDefaultDuration(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}

func splitEnv(key string, fallback []string) []string {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	var parts []string
	start := 0
	for i := 0; i < len(v); i++ {
		if v[i] == ',' {
			part := trimSpace(v[start:i])
			if part != "" {
				parts = append(parts, part)
			}
			start = i + 1
		}
	}
	part := trimSpace(v[start:])
	if part != "" {
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return fallback
	}
	return parts
}

func trimSpace(s string) string {
	start := 0
	for start < len(s) && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	end := len(s)
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
