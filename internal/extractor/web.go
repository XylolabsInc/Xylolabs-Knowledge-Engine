package extractor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	readability "github.com/go-shiori/go-readability"
	"golang.org/x/net/html/charset"
)

// extractWebURL fetches a URL and extracts its text content.
// For HTML responses it uses go-readability to extract the article body.
// For other content types it delegates to ExtractFromBytes.
func (e *Extractor) extractWebURL(ctx context.Context, rawURL string) (*ExtractResult, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("web: create request: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/145.0.0.0 Safari/537.36")
	req.Header.Set("Accept-Language", "ko-KR,ko;q=0.9,en-US;q=0.8,en;q=0.7")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("web: fetch url: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("web: fetch url: HTTP %d", resp.StatusCode)
	}

	// Limit response body to maxSize bytes.
	lr := &limitReader{r: resp.Body, limit: e.maxSize}
	body, err := io.ReadAll(lr)
	if err != nil {
		return nil, fmt.Errorf("web: read body: %w", err)
	}

	contentType := resp.Header.Get("Content-Type")

	// Detect and transcode non-UTF-8 charset (common with Korean sites using EUC-KR).
	if strings.Contains(strings.ToLower(contentType), "text/html") {
		reader, err := charset.NewReader(bytes.NewReader(body), contentType)
		if err == nil {
			if transcoded, err := io.ReadAll(reader); err == nil {
				body = transcoded
			}
		}
	}

	baseMIME := strings.SplitN(contentType, ";", 2)[0]
	baseMIME = strings.TrimSpace(baseMIME)

	if !strings.EqualFold(baseMIME, "text/html") {
		// Non-HTML content: delegate to bytes extractor.
		result, err := e.ExtractFromBytes(ctx, body, contentType, "")
		if err != nil {
			return nil, fmt.Errorf("web: extract non-html: %w", err)
		}
		return result, nil
	}

	// HTML: use go-readability to extract article text.
	parsedURL := resp.Request.URL // follow redirects
	article, err := readability.FromReader(strings.NewReader(string(body)), parsedURL)
	if err != nil {
		return nil, fmt.Errorf("web: readability parse: %w", err)
	}

	var sb strings.Builder
	if article.Title != "" {
		sb.WriteString(article.Title)
		sb.WriteString("\n\n")
	}
	sb.WriteString(article.TextContent)

	text := strings.TrimSpace(sb.String())
	if text == "" {
		// Fall back to raw body text if readability found nothing.
		e.logger.Debug("readability returned empty content, falling back to raw text", "url", rawURL)
		text = strings.TrimSpace(string(body))
	}

	e.logger.Debug("extracted web content", "url", rawURL, "length", len(text), "byline", article.Byline, "fetched_at", time.Now())

	return &ExtractResult{
		Text:     text,
		MimeType: "text/html",
	}, nil
}
