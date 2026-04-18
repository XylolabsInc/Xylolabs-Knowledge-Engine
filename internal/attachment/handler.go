package attachment

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/xylolabsinc/xylolabs-kb/internal/extractor"
	"github.com/xylolabsinc/xylolabs-kb/internal/kb"
)

const maxAttachmentSize = 100 << 20 // 100 MB

// Handler manages downloading and storing attachments.
type Handler struct {
	basePath   string
	httpClient *http.Client
	store      kb.Storage
	logger     *slog.Logger
	maxRetries int
}

// NewHandler creates an attachment handler.
func NewHandler(basePath string, store kb.Storage, logger *slog.Logger) *Handler {
	if err := os.MkdirAll(basePath, 0o750); err != nil {
		logger.Warn("failed to create attachment directory", "path", basePath, "error", err)
	}

	return &Handler{
		basePath: basePath,
		httpClient: extractor.NewRestrictedHTTPClient(5 * time.Minute),
		store:      store,
		logger:     logger.With("component", "attachment-handler"),
		maxRetries: 3,
	}
}

// Download fetches an attachment from its source URL and stores it locally.
func (h *Handler) Download(ctx context.Context, att kb.Attachment, authHeaders map[string]string) (*kb.Attachment, error) {
	if att.SourceURL == "" {
		return nil, fmt.Errorf("attachment %s has no source URL", att.ID)
	}
	if err := validateSourceURL(att.SourceURL); err != nil {
		return nil, fmt.Errorf("attachment %s has invalid source URL: %w", att.ID, err)
	}

	// Build local path: basePath/source/YYYY-MM/filename
	now := time.Now().UTC()
	dir := filepath.Join(h.basePath, documentPrefix(att.DocumentID), now.Format("2006-01"))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create directory %s: %w", dir, err)
	}

	localPath := filepath.Join(dir, sanitizeFilename(att.Filename))

	var lastErr error
	for attempt := range h.maxRetries {
		err := h.downloadFile(ctx, att.SourceURL, localPath, authHeaders)
		if err == nil {
			att.LocalPath = localPath
			att.DownloadedAt = time.Now().UTC()

			if att.MimeType == "" {
				att.MimeType = detectMimeType(localPath)
			}

			if info, err := os.Stat(localPath); err == nil {
				att.Size = info.Size()
			}

			if err := h.store.UpsertAttachment(att); err != nil {
				h.logger.Warn("failed to update attachment record", "id", att.ID, "error", err)
			}

			h.logger.Debug("downloaded attachment",
				"id", att.ID,
				"filename", att.Filename,
				"path", localPath,
				"size", att.Size,
			)
			return &att, nil
		}
		lastErr = err
		h.logger.Debug("download attempt failed",
			"attempt", attempt+1,
			"url", att.SourceURL,
			"error", err,
		)
		delay := 500*time.Millisecond * time.Duration(1<<uint(attempt))
			if delay > 10*time.Second {
				delay = 10 * time.Second
			}
			jitter := time.Duration(rand.Int64N(int64(delay) / 2))
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay + jitter):
			}
	}

	return nil, fmt.Errorf("download %s after %d attempts: %w", att.Filename, h.maxRetries, lastErr)
}

func (h *Handler) downloadFile(ctx context.Context, url, destPath string, headers map[string]string) error {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := h.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d downloading %s", resp.StatusCode, url)
	}

	// #nosec G304 -- destPath is constructed from sanitized attachment metadata beneath basePath.
	f, err := os.OpenFile(destPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create file %s: %w", destPath, err)
	}
	defer f.Close()

	if _, err := io.Copy(f, io.LimitReader(resp.Body, maxAttachmentSize)); err != nil {
		if removeErr := os.Remove(destPath); removeErr != nil && !os.IsNotExist(removeErr) {
			h.logger.Warn("failed to remove partial attachment", "path", destPath, "error", removeErr)
		}
		return fmt.Errorf("write file: %w", err)
	}

	return nil
}

func sanitizeFilename(name string) string {
	if name == "" {
		return "unnamed"
	}
	safe := make([]byte, 0, len(name))
	for i := range len(name) {
		c := name[i]
		switch c {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|':
			safe = append(safe, '_')
		default:
			safe = append(safe, c)
		}
	}
	return string(safe)
}

func validateSourceURL(rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("parse url: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("unsupported scheme %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return fmt.Errorf("missing host")
	}
	return nil
}

func documentPrefix(documentID string) string {
	if documentID == "" {
		return "unknown"
	}
	documentID = sanitizeFilename(documentID)
	if len(documentID) < 8 {
		return documentID
	}
	return documentID[:8]
}

func detectMimeType(path string) string {
	// #nosec G304 -- path is an internally generated attachment path beneath basePath.
	f, err := os.Open(path)
	if err != nil {
		return "application/octet-stream"
	}
	defer f.Close()

	buf := make([]byte, 512)
	n, err := f.Read(buf)
	if err != nil {
		return "application/octet-stream"
	}

	return http.DetectContentType(buf[:n])
}
