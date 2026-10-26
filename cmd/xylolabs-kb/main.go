package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/slack-go/slack"

	"github.com/xylolabsinc/xylolabs-kb/internal/api"
	"github.com/xylolabsinc/xylolabs-kb/internal/bot"
	"github.com/xylolabsinc/xylolabs-kb/internal/config"
	discordconn "github.com/xylolabsinc/xylolabs-kb/internal/discord"
	"github.com/xylolabsinc/xylolabs-kb/internal/extractor"
	"github.com/xylolabsinc/xylolabs-kb/internal/gemini"
	googleconn "github.com/xylolabsinc/xylolabs-kb/internal/google"
	"github.com/xylolabsinc/xylolabs-kb/internal/kb"
	"github.com/xylolabsinc/xylolabs-kb/internal/kbrepo"
	notionconn "github.com/xylolabsinc/xylolabs-kb/internal/notion"
	"github.com/xylolabsinc/xylolabs-kb/internal/plugin"
	"github.com/xylolabsinc/xylolabs-kb/internal/plugin/mcpstdio"
	slackconn "github.com/xylolabsinc/xylolabs-kb/internal/slack"
	"github.com/xylolabsinc/xylolabs-kb/internal/storage"
	"github.com/xylolabsinc/xylolabs-kb/internal/tools"
	"github.com/xylolabsinc/xylolabs-kb/internal/worker"
)

func main() {
	cfg := config.Load()

	// Validate configuration
	if errs := cfg.Validate(); len(errs) > 0 {
		for _, e := range errs {
			fmt.Fprintf(os.Stderr, "config warning: %s\n", e)
		}
	}

	// Set up structured logging
	var logLevel slog.Level
	switch cfg.LogLevel {
	case "debug":
		logLevel = slog.LevelDebug
	case "warn":
		logLevel = slog.LevelWarn
	case "error":
		logLevel = slog.LevelError
	default:
		logLevel = slog.LevelInfo
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel}))
	slog.SetDefault(logger)

	logger.Info("starting xylolabs-kb",
		"db_path", cfg.DBPath,
		"api_port", cfg.APIPort,
		"log_level", cfg.LogLevel,
	)

	// Initialize storage
	store, err := storage.New(cfg.DBPath, logger)
	if err != nil {
		logger.Error("failed to initialize storage", "error", err)
		os.Exit(1)
	}
	defer store.Close()

	// Initialize KB engine
	engine := kb.NewEngine(store, logger)

	// Initialize sync manager and scheduler
	syncManager := worker.NewSyncManager(store, logger)
	scheduler := worker.NewScheduler(logger)

	// Initialize Gemini client (used by bot and extractor)
	var geminiClient *gemini.Client
	if cfg.GeminiEnabled() {
		geminiClient = gemini.NewClient(cfg.LLMKey(), cfg.GeminiModel, logger)
		if cfg.LLMEndpoint != "" {
			geminiClient.WithEndpoint(cfg.LLMEndpoint)
			logger.Info("llm client enabled (openai-compatible endpoint)", "endpoint", cfg.LLMEndpoint, "model", cfg.GeminiModel)
		} else {
			logger.Info("gemini client enabled", "model", cfg.GeminiModel)
		}
	} else {
		logger.Info("llm client disabled (missing API key)")
	}

	// Initialize content extractor (works with or without Gemini; image extraction needs Gemini)
	ext := extractor.New(geminiClient, logger)

	// Done channel for connector lifecycle
	done := make(chan struct{})

	// Monitor connector health
	connErrors := make(chan error, 5)

	// Outer-scope handles for tool executor wiring after all connectors init.
	var botHandler *bot.Bot
	var slackPlatform *bot.SlackPlatform
	var discordBotHandler *bot.Bot
	var discordPlatform *bot.DiscordPlatform
	var googleConn *googleconn.Connector
	var jobScheduler *worker.JobScheduler
	var discordJobScheduler *worker.JobScheduler

	// Initialize connectors based on configuration
	if cfg.SlackEnabled() {
		slackConn := slackconn.NewConnector(cfg.SlackBotToken, cfg.SlackAppToken, engine, store, logger)
		slackConn.SetExtractor(ext)
		syncManager.AddConnector(slackConn)
		scheduler.Register("slack", cfg.SlackSyncInterval, slackConn.Sync)

		// Set up bot if Gemini is configured
		if geminiClient != nil {
			slackAPI := slack.New(cfg.SlackBotToken)
			authResp, err := slackAPI.AuthTest()
			if err != nil {
				logger.Error("failed to get slack bot user ID", "error", err)
			} else {
				var kbReader *kbrepo.Reader
				if cfg.KBRepoDir != "" {
					kbReader = kbrepo.NewReader(cfg.KBRepoDir, logger)
					logger.Info("kb repo reader enabled", "dir", cfg.KBRepoDir)
				}
				slackPlatform = bot.NewSlackPlatform(slackAPI, authResp.UserID, cfg.SlackBotToken, logger)
				botHandler = bot.New(slackPlatform, geminiClient, kbReader, cfg.GeminiProModel, cfg.SystemPromptFile, cfg.Language, cfg.Location(), logger)
				botHandler.SetExtractor(ext)
				slackConn.SetBot(botHandler, authResp.UserID)
				slackPlatform.StartCleanup()
				logger.Info("slack bot enabled", "bot_user_id", authResp.UserID)
			}
		}

		go func() {
			if err := slackConn.Start(done); err != nil {
				logger.Error("slack connector failed", "error", err)
				connErrors <- fmt.Errorf("slack: %w", err)
			}
		}()
		logger.Info("slack connector enabled")
	} else {
		logger.Info("slack connector disabled (missing credentials)")
	}

	if cfg.GoogleEnabled() {
		var err error
		googleConn, err = googleconn.NewConnector(cfg.GoogleCredsFile, cfg.GoogleTokenFile, cfg.GoogleScopes, cfg.GoogleDriveFolders, cfg.GoogleImpersonateEmail, engine, store, logger)
		if err != nil {
			logger.Error("failed to initialize google connector", "error", err)
			googleConn = nil
		} else {
			googleConn.SetExtractor(ext)
			syncManager.AddConnector(googleConn)
			scheduler.Register("google", cfg.GoogleSyncInterval, googleConn.Sync)

			go func() {
				if err := googleConn.Start(done); err != nil {
					logger.Error("google connector failed", "error", err)
					connErrors <- fmt.Errorf("google: %w", err)
				}
			}()
			logger.Info("google connector enabled")

			// Verify calendar write access for default calendar
			if cfg.GoogleDefaultCalendarID != "" {
				role, err := googleConn.VerifyCalendarAccess(cfg.GoogleDefaultCalendarID)
				if err != nil {
					logger.Warn("cannot verify calendar access", "calendar_id", cfg.GoogleDefaultCalendarID, "error", err)
				} else {
					logger.Info("default calendar access", "calendar_id", cfg.GoogleDefaultCalendarID, "access_role", role)
					if role != "writer" && role != "owner" {
						logger.Warn("impersonated user lacks write access to default calendar — event creation will fail. Grant 'Make changes to events' permission to the impersonated user.",
							"calendar_id", cfg.GoogleDefaultCalendarID,
							"access_role", role,
							"impersonate_email", cfg.GoogleImpersonateEmail,
						)
					}
				}
			}
		}
	} else {
		logger.Info("google connector disabled (missing credentials)")
	}

	if cfg.NotionEnabled() {
		notionConn := notionconn.NewConnector(cfg.NotionAPIKey, cfg.NotionRootPages, engine, store, logger)
		syncManager.AddConnector(notionConn)
		scheduler.Register("notion", cfg.NotionSyncInterval, notionConn.Sync)

		go func() {
			if err := notionConn.Start(done); err != nil {
				logger.Error("notion connector failed", "error", err)
				connErrors <- fmt.Errorf("notion: %w", err)
			}
		}()
		logger.Info("notion connector enabled")
	} else {
		logger.Info("notion connector disabled (missing credentials)")
	}

	if cfg.DiscordEnabled() {
		discordConn, err := discordconn.NewConnector(cfg.DiscordBotToken, cfg.DiscordGuildID, engine, store, logger)
		if err != nil {
			logger.Error("failed to initialize discord connector", "error", err)
		} else {
			discordConn.SetExtractor(ext)
			syncManager.AddConnector(discordConn)
			scheduler.Register("discord", cfg.DiscordSyncInterval, discordConn.Sync)

			// Set up Discord bot if Gemini is configured
			if geminiClient != nil {
				sess := discordConn.Session()
				botUser, err := sess.User("@me")
				if err != nil {
					logger.Error("failed to get discord bot user", "error", err)
				} else {
					var kbReader *kbrepo.Reader
					if cfg.KBRepoDir != "" {
						kbReader = kbrepo.NewReader(cfg.KBRepoDir, logger)
					}
					discordPlatform = bot.NewDiscordPlatform(sess, botUser.ID, cfg.DiscordGuildID, logger)
					discordBotHandler = bot.New(discordPlatform, geminiClient, kbReader, cfg.GeminiProModel, cfg.SystemPromptFile, cfg.Language, cfg.Location(), logger)
					discordBotHandler.SetExtractor(ext)
					discordConn.SetBot(discordBotHandler, botUser.ID)
					discordPlatform.StartCleanup()
					logger.Info("discord bot enabled", "bot_user_id", botUser.ID)
				}
			}

			go func() {
				if err := discordConn.Start(done); err != nil {
					logger.Error("discord connector failed", "error", err)
					connErrors <- fmt.Errorf("discord: %w", err)
				}
			}()
			logger.Info("discord connector enabled")
		}
	} else {
		logger.Info("discord connector disabled (missing credentials)")
	}

	// One plugin registry is shared by the Slack and Discord bots.
	pluginRegistry := newPluginRegistry(cfg, store, botHandler != nil || discordBotHandler != nil, logger)

	// Wire tool executor to bot after all connectors are initialized
	if botHandler != nil {
		var gw *tools.GoogleWriter
		if googleConn != nil {
			gw = tools.NewGoogleWriter(googleConn.DriveService(), googleConn.DocsService(), googleConn.SheetsService(), googleConn.SlidesService(), googleConn.CalendarService(), googleConn.TasksService(), googleConn.GmailService(), cfg.GoogleImpersonateEmail, logger)
		}
		var nw *tools.NotionWriter
		if cfg.NotionEnabled() {
			nw = tools.NewNotionWriter(cfg.NotionAPIKey, logger)
		}
		ss := tools.NewScreenshotter(logger)
		slackPoster := tools.NewSlackMessagePoster(slack.New(cfg.SlackBotToken), logger)
		sm := tools.NewSchedulerManager(store, slackPoster, slackPoster, cfg.Location(), logger)
		toolExecutor := tools.NewToolExecutor(gw, nw, cfg.GoogleDefaultCalendarID, ss, sm, store, logger)
		if pluginRegistry != nil {
			toolExecutor.SetPlugins(pluginRegistry)
			botHandler.SetPlugins(pluginRegistry)
		}
		botHandler.SetToolExecutor(toolExecutor)
		logger.Info("tool executor enabled",
			"google_drive", gw != nil,
			"notion", nw != nil,
		)

		// Start job scheduler for scheduled messages
		jobScheduler = worker.NewJobScheduler(store, slackPoster, cfg.Location(), logger)
		jobScheduler.Start()
	}

	// Wire Discord tool executor
	if discordBotHandler != nil {
		var gw *tools.GoogleWriter
		if googleConn != nil {
			gw = tools.NewGoogleWriter(googleConn.DriveService(), googleConn.DocsService(), googleConn.SheetsService(), googleConn.SlidesService(), googleConn.CalendarService(), googleConn.TasksService(), googleConn.GmailService(), cfg.GoogleImpersonateEmail, logger)
		}
		var nw *tools.NotionWriter
		if cfg.NotionEnabled() {
			nw = tools.NewNotionWriter(cfg.NotionAPIKey, logger)
		}
		ss := tools.NewScreenshotter(logger)
		discordPoster := bot.NewDiscordMessagePoster(discordPlatform.Session(), cfg.DiscordGuildID, logger)
		discordSM := tools.NewSchedulerManager(store, discordPoster, discordPoster, cfg.Location(), logger)
		discordToolExecutor := tools.NewToolExecutor(gw, nw, cfg.GoogleDefaultCalendarID, ss, discordSM, store, logger)
		if pluginRegistry != nil {
			discordToolExecutor.SetPlugins(pluginRegistry)
			discordBotHandler.SetPlugins(pluginRegistry)
		}
		discordBotHandler.SetToolExecutor(discordToolExecutor)
		logger.Info("discord tool executor enabled")

		discordJobScheduler = worker.NewJobScheduler(store, discordPoster, cfg.Location(), logger)
		discordJobScheduler.Start()
	}

	// Start plugins in the background: a slow or broken plugin must not delay
	// the bot. The janitor then expires pending actions and recovers ones a
	// previous run left running; it starts after the plugin so it can name it
	// and clean its inbox.
	if pluginRegistry != nil {
		go func() {
			startDaouOffice(done, cfg, pluginRegistry, logger)
			bot.RunPluginJanitor(done, pluginRegistry, time.Minute, logger, botHandler, discordBotHandler)
		}()
	}

	// Start scheduler
	scheduler.Start()

	// Start API server
	server := api.NewServer(cfg.APIHost, cfg.APIPort, engine, store, scheduler, syncManager, store, cfg.ConsoleUsername, cfg.ConsolePassword, cfg.KBRepoDir, ext, logger)
	go func() {
		if err := server.Start(); err != nil {
			logger.Error("api server failed", "error", err)
			os.Exit(1)
		}
	}()

	// Monitor connector health in background
	go func() {
		for err := range connErrors {
			logger.Error("CRITICAL: connector died unexpectedly", "error", err)
		}
	}()

	// Wait for shutdown signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh

	logger.Info("received shutdown signal", "signal", sig)

	// Graceful shutdown
	close(done)
	scheduler.Stop()
	if jobScheduler != nil {
		jobScheduler.Stop()
	}
	if slackPlatform != nil {
		slackPlatform.StopCleanup()
	}
	if discordJobScheduler != nil {
		discordJobScheduler.Stop()
	}
	if discordPlatform != nil {
		discordPlatform.StopCleanup()
	}

	if pluginRegistry != nil {
		if err := pluginRegistry.Close(); err != nil {
			logger.Warn("plugin shutdown error", "error", err)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("api server shutdown error", "error", err)
	}

	logger.Info("xylolabs-kb stopped")
}

// newPluginRegistry returns the plugin registry, or nil with the reason
// logged when no plugin can run.
func newPluginRegistry(cfg *config.Config, store *storage.SQLiteStore, botRunning bool, logger *slog.Logger) *plugin.Registry {
	switch {
	case !cfg.DaouOfficeEnabled:
		logger.Info("daouoffice plugin disabled (DAOUOFFICE_ENABLED is not true)")
		return nil
	case !botRunning:
		logger.Warn("daouoffice plugin disabled: no bot is running (needs Slack or Discord plus an LLM key)")
		return nil
	case cfg.PluginSecretKey == "":
		logger.Warn("daouoffice plugin disabled: PLUGIN_SECRET_KEY is not set, so accounts cannot be stored")
		return nil
	}
	vault, err := plugin.NewVault(cfg.PluginSecretKey)
	if err != nil {
		logger.Error("daouoffice plugin disabled: PLUGIN_SECRET_KEY is invalid", "error", err)
		return nil
	}
	return plugin.NewRegistry(store, vault, logger)
}

// startDaouOffice spawns the DaouOffice plugin and registers it once its MCP
// handshake succeeds. Its tools appear to the model from then on.
func startDaouOffice(done <-chan struct{}, cfg *config.Config, reg *plugin.Registry, logger *slog.Logger) {
	argv, err := cfg.DaouOfficeArgv()
	if err != nil {
		logger.Error("daouoffice plugin disabled: bad command", "error", err)
		return
	}
	dataDir, err := filepath.Abs(cfg.DaouOfficeDataDir)
	if err == nil {
		err = os.MkdirAll(dataDir, 0o700)
	}
	if err != nil {
		logger.Error("daouoffice plugin disabled: data directory", "path", cfg.DaouOfficeDataDir, "error", err)
		return
	}
	env := []string{"DAOUOFFICE_DATA_DIR=" + dataDir}
	// The plugin refuses a config path that does not exist, so pass it only
	// when it does; without it the plugin starts with empty company rules.
	if configPath, err := filepath.Abs(cfg.DaouOfficeConfig); err == nil {
		if _, statErr := os.Stat(configPath); statErr == nil {
			env = append(env, "DAOUOFFICE_CONFIG="+configPath)
		} else {
			logger.Warn("daouoffice company config not found; the plugin starts with empty rules", "path", configPath)
		}
	}
	for _, key := range []string{"PLAYWRIGHT_BROWSERS_PATH", "DAOUOFFICE_LOG_LEVEL"} {
		if v := os.Getenv(key); v != "" {
			env = append(env, key+"="+v)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	go func() {
		select {
		case <-done:
			cancel()
		case <-ctx.Done():
		}
	}()
	p, err := mcpstdio.New(ctx, mcpstdio.Config{
		Name: "daouoffice", Title: "다우오피스", Command: argv, Env: env, DataDir: dataDir,
	}, logger)
	if err != nil {
		logger.Error("daouoffice plugin disabled: start failed", "command", argv[0], "error", err)
		return
	}
	select {
	case <-done:
		_ = p.Close()
		return
	default:
	}
	if err := reg.Register(p); err != nil {
		logger.Error("daouoffice plugin disabled: register failed", "error", err)
		_ = p.Close()
		return
	}
	logger.Info("daouoffice plugin ready", "tools", len(p.Tools()), "accounts", len(p.AccountFields()) > 0)
}
