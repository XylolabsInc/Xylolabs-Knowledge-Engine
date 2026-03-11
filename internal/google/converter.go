package google

import (
	"fmt"
	"strings"
	"time"

	"google.golang.org/api/drive/v3"

	"github.com/xylolabsinc/xylolabs-kb/internal/kb"
)

// ConvertDriveFile converts a Google Drive file to a KB document.
func ConvertDriveFile(f *drive.File, content string) kb.Document {
	contentType := mapMimeToContentType(f.MimeType)

	var author, authorEmail string
	if len(f.Owners) > 0 {
		author = f.Owners[0].DisplayName
		authorEmail = f.Owners[0].EmailAddress
	}

	modified := parseGoogleTime(f.ModifiedTime)
	created := parseGoogleTime(f.CreatedTime)

	return kb.Document{
		Source:      kb.SourceGoogle,
		SourceID:    f.Id,
		Title:       f.Name,
		Content:     content,
		ContentType: contentType,
		Author:      author,
		AuthorEmail: authorEmail,
		URL:         f.WebViewLink,
		Timestamp:   created,
		UpdatedAt:   modified,
		Metadata: map[string]string{
			"mime_type":   f.MimeType,
			"description": f.Description,
			"size":        formatSize(f.Size),
		},
	}
}

func mapMimeToContentType(mimeType string) string {
	switch {
	case mimeType == "application/vnd.google-apps.document":
		return "document"
	case mimeType == "application/vnd.google-apps.spreadsheet":
		return "spreadsheet"
	case mimeType == "application/vnd.google-apps.presentation":
		return "presentation"
	case mimeType == "application/vnd.google-apps.form":
		return "form"
	case mimeType == "application/pdf":
		return "pdf"
	case strings.HasPrefix(mimeType, "image/"):
		return "image"
	case strings.HasPrefix(mimeType, "video/"):
		return "video"
	case strings.HasPrefix(mimeType, "text/"):
		return "document"
	default:
		return "file"
	}
}

func parseGoogleTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

func formatSize(size int64) string {
	switch {
	case size >= 1<<30:
		return fmt.Sprintf("%.1fGB", float64(size)/float64(1<<30))
	case size >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(size)/float64(1<<20))
	case size >= 1<<10:
		return fmt.Sprintf("%.1fKB", float64(size)/float64(1<<10))
	default:
		return fmt.Sprintf("%dB", size)
	}
}
