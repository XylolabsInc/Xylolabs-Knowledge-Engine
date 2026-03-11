package extractor

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"time"
)

const (
	defaultMaxSize    = 10 * 1024 * 1024 // 10MB
	fetchTimeout      = 15 * time.Second
)

// GeminiClient is the interface for AI-powered extraction (image description).
// This avoids a direct import cycle with the gemini package.
type GeminiClient interface {
	GenerateFromImage(ctx context.Context, prompt string, imageData []byte, mimeType string) (string, error)
}

// Extractor routes content to the appropriate extraction function based on MIME type.
type Extractor struct {
	gemini     GeminiClient
	httpClient *http.Client
	logger     *slog.Logger
	maxSize    int64
}

// ExtractResult holds the extracted text and detected MIME type.
type ExtractResult struct {
	Text     string
	MimeType string
}

// New creates an Extractor. gemini may be nil; image extraction will return a placeholder.
func New(gemini GeminiClient, logger *slog.Logger) *Extractor {
	return &Extractor{
		gemini: gemini,
		httpClient: &http.Client{
			Timeout: fetchTimeout,
		},
		logger:  logger.With("component", "extractor"),
		maxSize: defaultMaxSize,
	}
}

// ExtractFromBytes extracts text from raw bytes given a MIME type and filename hint.
func (e *Extractor) ExtractFromBytes(ctx context.Context, data []byte, mimeType string, filename string) (*ExtractResult, error) {
	if int64(len(data)) > e.maxSize {
		return nil, fmt.Errorf("extractor: extract from bytes: file size %d exceeds limit %d", len(data), e.maxSize)
	}

	// Normalise the MIME type (strip parameters like "; charset=utf-8").
	baseMIME := strings.SplitN(mimeType, ";", 2)[0]
	baseMIME = strings.TrimSpace(baseMIME)

	switch {
	case baseMIME == "application/pdf":
		text, err := extractPDF(data)
		if err != nil {
			return nil, fmt.Errorf("extractor: extract pdf: %w", err)
		}
		return &ExtractResult{Text: text, MimeType: baseMIME}, nil

	case strings.HasPrefix(baseMIME, "image/"):
		text, err := e.extractImage(ctx, data, baseMIME, filename)
		if err != nil {
			return nil, fmt.Errorf("extractor: extract image: %w", err)
		}
		return &ExtractResult{Text: text, MimeType: baseMIME}, nil

	case baseMIME == "application/vnd.openxmlformats-officedocument.wordprocessingml.document":
		text, err := extractDOCX(data)
		if err != nil {
			return nil, fmt.Errorf("extractor: extract docx: %w", err)
		}
		return &ExtractResult{Text: text, MimeType: baseMIME}, nil

	case baseMIME == "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":
		text, err := extractXLSX(data)
		if err != nil {
			return nil, fmt.Errorf("extractor: extract xlsx: %w", err)
		}
		return &ExtractResult{Text: text, MimeType: baseMIME}, nil

	case baseMIME == "application/vnd.openxmlformats-officedocument.presentationml.presentation":
		text, err := extractPPTX(data)
		if err != nil {
			return nil, fmt.Errorf("extractor: extract pptx: %w", err)
		}
		return &ExtractResult{Text: text, MimeType: baseMIME}, nil

	case baseMIME == "application/x-hwp" || baseMIME == "application/haansofthwp":
		text, err := extractHWP(data)
		if err != nil {
			return nil, fmt.Errorf("extractor: extract hwp: %w", err)
		}
		return &ExtractResult{Text: text, MimeType: baseMIME}, nil

	case baseMIME == "application/hwp+zip" || baseMIME == "application/x-hwpx":
		text, err := extractHWPX(data)
		if err != nil {
			return nil, fmt.Errorf("extractor: extract hwpx: %w", err)
		}
		return &ExtractResult{Text: text, MimeType: baseMIME}, nil

	case strings.HasPrefix(baseMIME, "text/"),
		baseMIME == "application/json",
		baseMIME == "application/xml",
		baseMIME == "application/csv",
		baseMIME == "application/javascript",
		baseMIME == "application/x-yaml",
		baseMIME == "application/x-sh":
		return &ExtractResult{Text: string(data), MimeType: baseMIME}, nil

	case strings.HasPrefix(baseMIME, "video/"),
		strings.HasPrefix(baseMIME, "audio/"):
		// Videos and audio can't be extracted as text; store metadata only.
		return &ExtractResult{
			Text:     fmt.Sprintf("[%s file: %s]", baseMIME, filename),
			MimeType: baseMIME,
		}, nil

	default:
		// Try to detect from filename extension.
		if filename != "" {
			detected := mimeFromFilename(filename)
			if detected != "" && detected != baseMIME {
				e.logger.Debug("falling back to extension-based MIME detection", "filename", filename, "detected", detected)
				return e.ExtractFromBytes(ctx, data, detected, "")
			}
		}
		// For truly unknown types, return a metadata placeholder rather than failing.
		e.logger.Debug("unsupported MIME type, storing metadata", "mime", baseMIME, "filename", filename)
		return &ExtractResult{
			Text:     fmt.Sprintf("[Attached file: %s (%s)]", filename, baseMIME),
			MimeType: baseMIME,
		}, nil
	}
}

// ExtractFromURL fetches a URL and extracts its text content.
func (e *Extractor) ExtractFromURL(ctx context.Context, url string) (*ExtractResult, error) {
	return e.extractWebURL(ctx, url)
}

// mimeFromFilename returns a MIME type guess based on the file extension.
func mimeFromFilename(filename string) string {
	ext := strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".pdf":
		return "application/pdf"
	case ".docx":
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	case ".xlsx":
		return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	case ".pptx":
		return "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	case ".txt", ".md", ".log", ".ini", ".cfg", ".conf":
		return "text/plain"
	case ".csv":
		return "application/csv"
	case ".json":
		return "application/json"
	case ".xml":
		return "application/xml"
	case ".yaml", ".yml":
		return "application/x-yaml"
	case ".html", ".htm":
		return "text/html"
	case ".js", ".ts":
		return "application/javascript"
	case ".sh", ".bash":
		return "application/x-sh"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".hwp":
		return "application/x-hwp"
	case ".hwpx":
		return "application/hwp+zip"
	default:
		return ""
	}
}

// limitReader wraps an io.Reader with a hard size limit, returning an error if exceeded.
type limitReader struct {
	r       io.Reader
	limit   int64
	read    int64
}

func (lr *limitReader) Read(p []byte) (int, error) {
	if lr.read >= lr.limit {
		return 0, fmt.Errorf("extractor: response body exceeds %d bytes", lr.limit)
	}
	remaining := lr.limit - lr.read
	if int64(len(p)) > remaining {
		p = p[:remaining]
	}
	n, err := lr.r.Read(p)
	lr.read += int64(n)
	return n, err
}
