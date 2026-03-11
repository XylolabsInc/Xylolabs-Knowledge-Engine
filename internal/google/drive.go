package google

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"golang.org/x/time/rate"
	"google.golang.org/api/drive/v3"

	"github.com/xylolabsinc/xylolabs-kb/internal/extractor"
	"github.com/xylolabsinc/xylolabs-kb/internal/kb"
)

// DriveClient handles Google Drive file operations.
type DriveClient struct {
	service   *drive.Service
	logger    *slog.Logger
	limiter   *rate.Limiter
	extractor *extractor.Extractor // may be nil
	folderIDs []string             // optional folder IDs to restrict sync
}

func (d *DriveClient) waitRateLimit(ctx context.Context) error {
	return d.limiter.Wait(ctx)
}

// SyncAllFiles lists and indexes all files from Google Drive.
func (d *DriveClient) SyncAllFiles(ctx context.Context, engine *kb.Engine) (int, error) {
	var count int
	pageToken := ""

	for {
		query := "trashed = false"

		q := d.service.Files.List().
			Context(ctx).
			PageSize(100).
			Fields("nextPageToken, files(id, name, mimeType, modifiedTime, createdTime, webViewLink, owners, size, description, parents)").
			Q(query).
			OrderBy("modifiedTime desc").
			SupportsAllDrives(true).
			IncludeItemsFromAllDrives(true)

		// Shared drive IDs start with "0A" — use Corpora("drive") + DriveId
		// Regular folder IDs use parents-in query
		if len(d.folderIDs) == 1 && strings.HasPrefix(d.folderIDs[0], "0A") {
			q = q.Corpora("drive").DriveId(d.folderIDs[0])
		} else if len(d.folderIDs) > 0 {
			var parentClauses []string
			for _, fid := range d.folderIDs {
				parentClauses = append(parentClauses, fmt.Sprintf("'%s' in parents", fid))
			}
			q = q.Q(query + " and (" + strings.Join(parentClauses, " or ") + ")")
		}

		if pageToken != "" {
			q = q.PageToken(pageToken)
		}

		if err := d.waitRateLimit(ctx); err != nil {
			return count, fmt.Errorf("rate limit: %w", err)
		}
		result, err := q.Do()
		if err != nil {
			d.logger.Error("drive list files failed", "error", err, "folder_ids", d.folderIDs)
			return count, fmt.Errorf("list files: %w", err)
		}

		d.logger.Debug("drive list files result", "files_in_page", len(result.Files), "folder_ids", d.folderIDs)

		for _, f := range result.Files {
			doc, err := d.fileToDocument(ctx, f)
			if err != nil {
				d.logger.Warn("failed to convert file", "file_id", f.Id, "name", f.Name, "error", err)
				continue
			}
			if err := engine.Index(ctx, doc); err != nil {
				d.logger.Warn("failed to index file", "file_id", f.Id, "error", err)
				continue
			}
			count++
		}

		if result.NextPageToken == "" {
			break
		}
		pageToken = result.NextPageToken
	}

	return count, nil
}

// SyncChanges fetches changes since the given page token.
func (d *DriveClient) SyncChanges(ctx context.Context, pageToken string, engine *kb.Engine) (int, string, error) {
	var count int
	currentToken := pageToken

	for {
		if err := d.waitRateLimit(ctx); err != nil {
			return count, currentToken, fmt.Errorf("rate limit: %w", err)
		}
		changes, err := d.service.Changes.List(currentToken).
			Context(ctx).
			PageSize(100).
			Fields("nextPageToken, newStartPageToken, changes(fileId, removed, file(id, name, mimeType, modifiedTime, createdTime, webViewLink, owners, size, description))").
			IncludeRemoved(true).
			SupportsAllDrives(true).
			IncludeItemsFromAllDrives(true).
			Do()
		if err != nil {
			return count, currentToken, fmt.Errorf("list changes: %w", err)
		}

		for _, change := range changes.Changes {
			if change.Removed || change.File == nil {
				// File was deleted — could remove from index
				continue
			}

			doc, err := d.fileToDocument(ctx, change.File)
			if err != nil {
				d.logger.Warn("failed to convert changed file", "file_id", change.FileId, "error", err)
				continue
			}
			if err := engine.Index(ctx, doc); err != nil {
				d.logger.Warn("failed to index changed file", "file_id", change.FileId, "error", err)
				continue
			}
			count++
		}

		if changes.NewStartPageToken != "" {
			currentToken = changes.NewStartPageToken
			break
		}
		if changes.NextPageToken == "" {
			break
		}
		currentToken = changes.NextPageToken
	}

	return count, currentToken, nil
}

// GetStartPageToken retrieves the initial page token for future change tracking.
func (d *DriveClient) GetStartPageToken(ctx context.Context) (string, error) {
	if err := d.waitRateLimit(ctx); err != nil {
		return "", fmt.Errorf("rate limit: %w", err)
	}
	resp, err := d.service.Changes.GetStartPageToken().Context(ctx).SupportsAllDrives(true).Do()
	if err != nil {
		return "", fmt.Errorf("get start page token: %w", err)
	}
	return resp.StartPageToken, nil
}

func (d *DriveClient) fileToDocument(ctx context.Context, f *drive.File) (kb.Document, error) {
	content, err := d.extractContent(ctx, f)
	if err != nil {
		d.logger.Debug("could not extract content", "file_id", f.Id, "mime", f.MimeType, "error", err)
		content = f.Description // fallback to description
	}

	return ConvertDriveFile(f, content), nil
}

func (d *DriveClient) extractContent(ctx context.Context, f *drive.File) (string, error) {
	switch {
	// Google native formats - export as text
	case f.MimeType == "application/vnd.google-apps.document":
		return d.exportAsText(ctx, f.Id, "text/plain")
	case f.MimeType == "application/vnd.google-apps.spreadsheet":
		return d.exportAsText(ctx, f.Id, "text/csv")
	case f.MimeType == "application/vnd.google-apps.presentation":
		return d.exportAsText(ctx, f.Id, "text/plain")

	// Binary files that need extraction
	case f.MimeType == "application/pdf",
		strings.HasPrefix(f.MimeType, "image/"),
		f.MimeType == "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		f.MimeType == "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		f.MimeType == "application/vnd.openxmlformats-officedocument.presentationml.presentation":
		if d.extractor == nil {
			return "", fmt.Errorf("no extractor configured for %s", f.MimeType)
		}
		data, err := d.downloadBytes(ctx, f.Id)
		if err != nil {
			return "", fmt.Errorf("download file for extraction: %w", err)
		}
		result, err := d.extractor.ExtractFromBytes(ctx, data, f.MimeType, f.Name)
		if err != nil {
			return "", fmt.Errorf("extract content: %w", err)
		}
		return result.Text, nil

	// Plain text files
	case strings.HasPrefix(f.MimeType, "text/"):
		return d.downloadAsText(ctx, f.Id)

	default:
		return "", fmt.Errorf("unsupported mime type: %s", f.MimeType)
	}
}

func (d *DriveClient) exportAsText(ctx context.Context, fileID, mimeType string) (string, error) {
	if err := d.waitRateLimit(ctx); err != nil {
		return "", fmt.Errorf("rate limit: %w", err)
	}
	resp, err := d.service.Files.Export(fileID, mimeType).Context(ctx).Download()
	if err != nil {
		return "", fmt.Errorf("export file %s: %w", fileID, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024)) // 10MB limit
	if err != nil {
		return "", fmt.Errorf("read export: %w", err)
	}
	return string(data), nil
}

func (d *DriveClient) downloadAsText(ctx context.Context, fileID string) (string, error) {
	if err := d.waitRateLimit(ctx); err != nil {
		return "", fmt.Errorf("rate limit: %w", err)
	}
	resp, err := d.service.Files.Get(fileID).Context(ctx).Download()
	if err != nil {
		return "", fmt.Errorf("download file %s: %w", fileID, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024))
	if err != nil {
		return "", fmt.Errorf("read download: %w", err)
	}
	return string(data), nil
}

func (d *DriveClient) downloadBytes(ctx context.Context, fileID string) ([]byte, error) {
	if err := d.waitRateLimit(ctx); err != nil {
		return nil, fmt.Errorf("rate limit: %w", err)
	}
	resp, err := d.service.Files.Get(fileID).Context(ctx).Download()
	if err != nil {
		return nil, fmt.Errorf("download file %s: %w", fileID, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("read download: %w", err)
	}
	return data, nil
}
