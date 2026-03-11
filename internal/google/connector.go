package google

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"golang.org/x/time/rate"
	"google.golang.org/api/calendar/v3"
	"google.golang.org/api/docs/v1"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"
	"google.golang.org/api/sheets/v4"
	"google.golang.org/api/slides/v1"
	"google.golang.org/api/tasks/v1"

	"github.com/xylolabsinc/xylolabs-kb/internal/extractor"
	"github.com/xylolabsinc/xylolabs-kb/internal/kb"
)

// Connector manages Google Workspace integration.
type Connector struct {
	driveService    *drive.Service
	calendarService *calendar.Service
	docsService     *docs.Service
	sheetsService   *sheets.Service
	slidesService   *slides.Service
	gmailService    *gmail.Service
	tasksService    *tasks.Service
	engine          *kb.Engine
	store           kb.Storage
	logger          *slog.Logger
	credsFile       string
	tokenFile       string
	scopes          []string
	extractor       *extractor.Extractor
	driveFolders    []string
	impersonateEmail string
}

// NewConnector creates a Google Workspace connector.
func NewConnector(credsFile, tokenFile string, scopes []string, driveFolders []string, impersonateEmail string, engine *kb.Engine, store kb.Storage, logger *slog.Logger) (*Connector, error) {
	c := &Connector{
		engine:           engine,
		store:            store,
		logger:           logger.With("component", "google-connector"),
		credsFile:        credsFile,
		tokenFile:        tokenFile,
		scopes:           scopes,
		driveFolders:     driveFolders,
		impersonateEmail: impersonateEmail,
	}

	if err := c.initServices(); err != nil {
		return nil, fmt.Errorf("init google services: %w", err)
	}

	return c, nil
}

// Name returns the source identifier.
func (c *Connector) Name() kb.Source {
	return kb.SourceGoogle
}

// Start is a no-op for Google — there is no real-time event stream.
func (c *Connector) Start(done <-chan struct{}) error {
	c.logger.Info("google connector started (poll-based)")
	<-done
	return nil
}

// Stop gracefully shuts down the connector.
func (c *Connector) Stop() error {
	c.logger.Info("google connector stopped")
	return nil
}

// SetExtractor sets the content extractor for file processing.
func (c *Connector) SetExtractor(ext *extractor.Extractor) {
	c.extractor = ext
}

// DriveService returns the underlying Drive service for write operations.
func (c *Connector) DriveService() *drive.Service {
	return c.driveService
}

// DocsService returns the Google Docs service.
func (c *Connector) DocsService() *docs.Service {
	return c.docsService
}

// SheetsService returns the Google Sheets service.
func (c *Connector) SheetsService() *sheets.Service {
	return c.sheetsService
}

// SlidesService returns the Google Slides service.
func (c *Connector) SlidesService() *slides.Service {
	return c.slidesService
}

// CalendarService returns the Google Calendar service.
func (c *Connector) CalendarService() *calendar.Service {
	return c.calendarService
}

// GmailService returns the Gmail service.
func (c *Connector) GmailService() *gmail.Service {
	return c.gmailService
}

// TasksService returns the Google Tasks service.
func (c *Connector) TasksService() *tasks.Service {
	return c.tasksService
}

// Sync fetches new and updated files from Google Drive.
func (c *Connector) Sync() error {
	ctx := context.Background()
	c.logger.Info("starting google sync")

	syncState, err := c.store.GetSyncState(kb.SourceGoogle)
	if err != nil {
		return fmt.Errorf("get google sync state: %w", err)
	}

	var pageToken string
	if syncState != nil && syncState.Cursor != "" {
		pageToken = syncState.Cursor
	}

	driveClient := &DriveClient{
		service:   c.driveService,
		logger:    c.logger,
		limiter:   rate.NewLimiter(rate.Limit(10), 10),
		extractor: c.extractor,
		folderIDs: c.driveFolders,
	}

	var totalFiles int

	if pageToken != "" {
		// Incremental sync using changes API
		count, newToken, err := driveClient.SyncChanges(ctx, pageToken, c.engine)
		if err != nil {
			return fmt.Errorf("sync changes: %w", err)
		}
		totalFiles = count
		pageToken = newToken
	} else {
		// Full sync — list all files
		count, err := driveClient.SyncAllFiles(ctx, c.engine)
		if err != nil {
			return fmt.Errorf("sync all files: %w", err)
		}
		totalFiles = count

		// Get start page token for future incremental syncs
		startToken, err := driveClient.GetStartPageToken(ctx)
		if err != nil {
			c.logger.Warn("failed to get start page token", "error", err)
		} else {
			pageToken = startToken
		}
	}

	now := time.Now().UTC()
	newState := kb.SyncState{
		Source:     kb.SourceGoogle,
		LastSyncAt: now,
		Cursor:     pageToken,
		Metadata:   map[string]string{"files_synced": fmt.Sprintf("%d", totalFiles)},
	}
	if err := c.store.SetSyncState(newState); err != nil {
		return fmt.Errorf("set google sync state: %w", err)
	}

	// Sync Google Calendar events
	calClient := &CalendarClient{
		service: c.calendarService,
		logger:  c.logger,
		limiter: rate.NewLimiter(rate.Limit(10), 10),
	}
	calCount, err := calClient.SyncEvents(ctx, c.engine)
	if err != nil {
		c.logger.Warn("calendar sync failed", "error", err)
	} else {
		totalFiles += calCount
	}

	c.logger.Info("google sync complete", "files", totalFiles)
	return nil
}

func (c *Connector) initServices() error {
	ctx := context.Background()

	credBytes, err := os.ReadFile(c.credsFile)
	if err != nil {
		return fmt.Errorf("read credentials file: %w", err)
	}

	httpClient, err := c.buildHTTPClient(ctx, credBytes)
	if err != nil {
		return fmt.Errorf("build http client: %w", err)
	}

	driveSvc, err := drive.NewService(ctx, option.WithHTTPClient(httpClient))
	if err != nil {
		return fmt.Errorf("create drive service: %w", err)
	}
	c.driveService = driveSvc

	// Calendar, Tasks, Gmail use impersonated client when available (domain-wide delegation).
	impersonatedClient := httpClient
	if c.impersonateEmail != "" {
		credBytes, err := os.ReadFile(c.credsFile)
		if err != nil {
			return fmt.Errorf("read credentials for impersonation: %w", err)
		}
		ic, err := c.buildImpersonatedHTTPClient(ctx, credBytes)
		if err != nil {
			c.logger.Warn("failed to create impersonated client, falling back to service account", "error", err)
		} else {
			impersonatedClient = ic
			c.logger.Info("impersonated client created", "email", c.impersonateEmail)
		}
	}

	calSvc, err := calendar.NewService(ctx, option.WithHTTPClient(impersonatedClient))
	if err != nil {
		return fmt.Errorf("create calendar service: %w", err)
	}
	c.calendarService = calSvc

	docsSvc, err := docs.NewService(ctx, option.WithHTTPClient(httpClient))
	if err != nil {
		return fmt.Errorf("create docs service: %w", err)
	}
	c.docsService = docsSvc

	sheetsSvc, err := sheets.NewService(ctx, option.WithHTTPClient(httpClient))
	if err != nil {
		return fmt.Errorf("create sheets service: %w", err)
	}
	c.sheetsService = sheetsSvc

	slidesSvc, err := slides.NewService(ctx, option.WithHTTPClient(httpClient))
	if err != nil {
		return fmt.Errorf("create slides service: %w", err)
	}
	c.slidesService = slidesSvc

	gmailSvc, err := gmail.NewService(ctx, option.WithHTTPClient(impersonatedClient))
	if err != nil {
		return fmt.Errorf("create gmail service: %w", err)
	}
	c.gmailService = gmailSvc

	tasksSvc, err := tasks.NewService(ctx, option.WithHTTPClient(impersonatedClient))
	if err != nil {
		return fmt.Errorf("create tasks service: %w", err)
	}
	c.tasksService = tasksSvc

	return nil
}

// buildHTTPClient detects credential type (service account vs OAuth2) and returns an authenticated HTTP client.
func (c *Connector) buildHTTPClient(ctx context.Context, credBytes []byte) (*http.Client, error) {
	// Detect credential type by checking for "type" field
	var creds struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(credBytes, &creds); err != nil {
		return nil, fmt.Errorf("parse credentials type: %w", err)
	}

	if creds.Type == "service_account" {
		c.logger.Info("using service account credentials")
		jwtConfig, err := google.JWTConfigFromJSON(credBytes, c.scopes...)
		if err != nil {
			return nil, fmt.Errorf("parse service account key: %w", err)
		}
		return jwtConfig.Client(ctx), nil
	}

	// OAuth2 user credentials — use token file
	c.logger.Info("using OAuth2 user credentials")
	config, err := google.ConfigFromJSON(credBytes, c.scopes...)
	if err != nil {
		return nil, fmt.Errorf("parse OAuth2 credentials: %w", err)
	}

	token, err := c.loadToken()
	if err != nil {
		return nil, fmt.Errorf("load token: %w", err)
	}

	return config.Client(ctx, token), nil
}

// buildImpersonatedHTTPClient creates an HTTP client that impersonates a specific user.
// Used for Gmail API which requires domain-wide delegation with a specific user identity.
func (c *Connector) buildImpersonatedHTTPClient(ctx context.Context, credBytes []byte) (*http.Client, error) {
	var creds struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(credBytes, &creds); err != nil {
		return nil, fmt.Errorf("parse credentials type: %w", err)
	}

	if creds.Type != "service_account" {
		return nil, fmt.Errorf("impersonation requires service account credentials")
	}

	// Only request scopes that are authorized for domain-wide delegation.
	impersonationScopes := []string{
		"https://www.googleapis.com/auth/calendar",
		"https://www.googleapis.com/auth/tasks",
		"https://www.googleapis.com/auth/gmail.send",
	}
	jwtConfig, err := google.JWTConfigFromJSON(credBytes, impersonationScopes...)
	if err != nil {
		return nil, fmt.Errorf("parse service account key: %w", err)
	}
	jwtConfig.Subject = c.impersonateEmail
	c.logger.Info("created impersonated client", "email", c.impersonateEmail)
	return jwtConfig.Client(ctx), nil
}

// VerifyCalendarAccess checks and logs the access role for a given calendar.
// Returns the access role string ("reader", "writer", "owner", etc.) or error.
func (c *Connector) VerifyCalendarAccess(calendarID string) (string, error) {
	entry, err := c.calendarService.CalendarList.Get(calendarID).Do()
	if err != nil {
		return "", fmt.Errorf("get calendar %s: %w", calendarID, err)
	}
	return entry.AccessRole, nil
}

func (c *Connector) loadToken() (*oauth2.Token, error) {
	data, err := os.ReadFile(c.tokenFile)
	if err != nil {
		return nil, fmt.Errorf("read token file %s: %w", c.tokenFile, err)
	}
	var token oauth2.Token
	if err := json.Unmarshal(data, &token); err != nil {
		return nil, fmt.Errorf("parse token: %w", err)
	}
	return &token, nil
}
