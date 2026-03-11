package attachment

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/xylolabsinc/xylolabs-kb/internal/kb"
)

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
	if err := os.MkdirAll(basePath, 0o755); err != nil {
		logger.Warn("failed to create attachment directory", "path", basePath, "error", err)
	}

	return &Handler{
		basePath: basePath,
		httpClient: &http.Client{
			Timeout: 5 * time.Minute,
		},
		store:      store,
		logger:     logger.With("component", "attachment-handler"),
		maxRetries: 3,
	}
}

// Download fetches an attachment from its source URL and stores it locally.
func (h *Handler) Download(att kb.Attachment, authHeaders map[string]string) (*kb.Attachment, error) {
	if att.SourceURL == "" {
		return nil, fmt.Errorf("attachment %s has no source URL", att.ID)
	}

	// Build local path: basePath/source/YYYY-MM/filename
	now := time.Now().UTC()
	dir := filepath.Join(h.basePath, att.DocumentID[:8], now.Format("2006-01"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create directory %s: %w", dir, err)
	}

	localPath := filepath.Join(dir, sanitizeFilename(att.Filename))

	// Download with retry
	var lastErr error
	for attempt := range h.maxRetries {
		err := h.downloadFile(att.SourceURL, localPath, authHeaders)
		if err == nil {
			att.LocalPath = localPath
			att.DownloadedAt = time.Now().UTC()

			// Detect content type if not set
			if att.MimeType == "" {
				att.MimeType = detectMimeType(localPath)
			}

			// Get file size
			if info, err := os.Stat(localPath); err == nil {
				att.Size = info.Size()
			}

			// Update in storage
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
		time.Sleep(time.Duration(attempt+1) * time.Second)
	}

	return nil, fmt.Errorf("download %s after %d attempts: %w", att.Filename, h.maxRetries, lastErr)
}

func (h *Handler) downloadFile(url, destPath string, headers map[string]string) error {
	req, err := http.NewRequest("GET", url, nil)
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

	f, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("create file %s: %w", destPath, err)
	}
	defer f.Close()

	if _, err := io.Copy(f, resp.Body); err != nil {
		os.Remove(destPath)
		return fmt.Errorf("write file: %w", err)
	}

	return nil
}

func sanitizeFilename(name string) string {
	if name == "" {
		return "unnamed"
	}
	// Replace problematic characters
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

func detectMimeType(path string) string {
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
