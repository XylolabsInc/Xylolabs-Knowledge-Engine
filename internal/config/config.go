package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Config holds all application configuration.
type Config struct {
	// Database
	DBPath          string
	AttachmentPath  string

	// Slack
	SlackBotToken    string
	SlackAppToken    string
	SlackSignSecret  string

	// Google Workspace
	GoogleCredsFile        string
	GoogleTokenFile        string
	GoogleScopes           []string
	GoogleDriveFolders     []string
	GoogleImpersonateEmail  string
	GoogleDefaultCalendarID string

	// Discord
	DiscordBotToken string
	DiscordGuildID  string

	// Notion
	NotionAPIKey     string
	NotionRootPages  []string

	// Gemini AI
	GeminiAPIKey   string
	GeminiModel    string
	GeminiProModel string

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
}

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
		GoogleScopes:       splitEnv("GOOGLE_SCOPES", []string{
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
		GeminiModel:    envOrDefault("GEMINI_MODEL", "gemini-3.1-flash-lite-preview"),
		GeminiProModel: envOrDefault("GEMINI_PRO_MODEL", "gemini-3.1-pro-preview"),

		SystemPromptFile: envOrDefault("SYSTEM_PROMPT_FILE", ""),
		Language:         envOrDefault("LANGUAGE", "en"),
		Timezone:         envOrDefault("TIMEZONE", "Asia/Seoul"),

		KBRepoDir: envOrDefault("KB_REPO_DIR", ""),

		APIHost: envOrDefault("API_HOST", "0.0.0.0"),
		APIPort: envOrDefaultInt("API_PORT", 8080),

		ConsoleUsername: envOrDefault("CONSOLE_USERNAME", "admin"),
		ConsolePassword: envOrDefault("CONSOLE_PASSWORD", ""),

		SlackSyncInterval:  envOrDefaultDuration("SLACK_SYNC_INTERVAL", 1*time.Hour),
		GoogleSyncInterval: envOrDefaultDuration("GOOGLE_SYNC_INTERVAL", 15*time.Minute),
		NotionSyncInterval:  envOrDefaultDuration("NOTION_SYNC_INTERVAL", 10*time.Minute),
		DiscordSyncInterval: envOrDefaultDuration("DISCORD_SYNC_INTERVAL", 5*time.Minute),

		LogLevel: envOrDefault("LOG_LEVEL", "info"),
	}
	return cfg
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
	return c.GeminiAPIKey != ""
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

	// Check system prompt file exists if specified
	if c.SystemPromptFile != "" {
		if _, err := os.Stat(c.SystemPromptFile); err != nil {
			errs = append(errs, fmt.Sprintf("SYSTEM_PROMPT_FILE %q not found: %v", c.SystemPromptFile, err))
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
