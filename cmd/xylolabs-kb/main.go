package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/slack-go/slack"

	"github.com/xylolabsinc/xylolabs-kb/internal/api"
	"github.com/xylolabsinc/xylolabs-kb/internal/bot"
	"github.com/xylolabsinc/xylolabs-kb/internal/config"
	"github.com/xylolabsinc/xylolabs-kb/internal/extractor"
	"github.com/xylolabsinc/xylolabs-kb/internal/gemini"
	googleconn "github.com/xylolabsinc/xylolabs-kb/internal/google"
	"github.com/xylolabsinc/xylolabs-kb/internal/kb"
	"github.com/xylolabsinc/xylolabs-kb/internal/kbrepo"
	notionconn "github.com/xylolabsinc/xylolabs-kb/internal/notion"
	slackconn "github.com/xylolabsinc/xylolabs-kb/internal/slack"
	"github.com/xylolabsinc/xylolabs-kb/internal/storage"
	"github.com/xylolabsinc/xylolabs-kb/internal/tools"
	"github.com/xylolabsinc/xylolabs-kb/internal/worker"
)

func main() {
	cfg := config.Load()

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
		geminiClient = gemini.NewClient(cfg.GeminiAPIKey, cfg.GeminiModel, logger)
		logger.Info("gemini client enabled", "model", cfg.GeminiModel)
	} else {
		logger.Info("gemini client disabled (missing API key)")
	}

	// Initialize content extractor (works with or without Gemini; image extraction needs Gemini)
	ext := extractor.New(geminiClient, logger)

	// Done channel for connector lifecycle
	done := make(chan struct{})

	// Outer-scope handles for tool executor wiring after all connectors init.
	var botHandler *bot.Bot
	var googleConn *googleconn.Connector

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
				botHandler = bot.New(slackAPI, geminiClient, kbReader, authResp.UserID, cfg.SlackBotToken, cfg.GeminiProModel, cfg.SystemPromptFile, logger)
				botHandler.SetExtractor(ext)
				slackConn.SetBot(botHandler, authResp.UserID)
				logger.Info("slack bot enabled", "bot_user_id", authResp.UserID)
			}
		}

		go func() {
			if err := slackConn.Start(done); err != nil {
				logger.Error("slack connector failed", "error", err)
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
			}
		}()
		logger.Info("notion connector enabled")
	} else {
		logger.Info("notion connector disabled (missing credentials)")
	}

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
		toolExecutor := tools.NewToolExecutor(gw, nw, cfg.GoogleDefaultCalendarID, logger)
		botHandler.SetToolExecutor(toolExecutor)
		logger.Info("tool executor enabled",
			"google_drive", gw != nil,
			"notion", nw != nil,
		)
	}

	// Start scheduler
	scheduler.Start()

	// Start API server
	server := api.NewServer(cfg.APIHost, cfg.APIPort, engine, store, scheduler, syncManager, logger)
	go func() {
		if err := server.Start(); err != nil {
			logger.Error("api server failed", "error", err)
			os.Exit(1)
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

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("api server shutdown error", "error", err)
	}

	logger.Info("xylolabs-kb stopped")
}
