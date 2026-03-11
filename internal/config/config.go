package config

import (
	"os"
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
	GoogleCredsFile    string
	GoogleTokenFile    string
	GoogleScopes       []string
	GoogleDriveFolders []string

	// Notion
	NotionAPIKey     string
	NotionRootPages  []string

	// Gemini AI
	GeminiAPIKey   string
	GeminiModel    string
	GeminiProModel string

	// Knowledge Base Repo
	KBRepoDir string

	// API Server
	APIHost string
	APIPort int

	// Sync intervals
	SlackSyncInterval  time.Duration
	GoogleSyncInterval time.Duration
	NotionSyncInterval time.Duration

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
			"https://www.googleapis.com/auth/calendar.readonly",
		}),

		NotionAPIKey:    os.Getenv("NOTION_API_KEY"),
		NotionRootPages: splitEnv("NOTION_ROOT_PAGES", nil),

		GeminiAPIKey:   os.Getenv("GEMINI_API_KEY"),
		GeminiModel:    envOrDefault("GEMINI_MODEL", "gemini-3.1-flash-lite-preview"),
		GeminiProModel: envOrDefault("GEMINI_PRO_MODEL", "gemini-3.1-pro-preview"),

		KBRepoDir: envOrDefault("KB_REPO_DIR", ""),

		APIHost: envOrDefault("API_HOST", "0.0.0.0"),
		APIPort: envOrDefaultInt("API_PORT", 8080),

		SlackSyncInterval:  envOrDefaultDuration("SLACK_SYNC_INTERVAL", 5*time.Minute),
		GoogleSyncInterval: envOrDefaultDuration("GOOGLE_SYNC_INTERVAL", 15*time.Minute),
		NotionSyncInterval: envOrDefaultDuration("NOTION_SYNC_INTERVAL", 10*time.Minute),

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

// NotionEnabled returns true if Notion credentials are configured.
func (c *Config) NotionEnabled() bool {
	return c.NotionAPIKey != ""
}

// GeminiEnabled returns true if Gemini API key is configured.
func (c *Config) GeminiEnabled() bool {
	return c.GeminiAPIKey != ""
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
