package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	notionAPIBase       = "https://api.notion.com/v1"
	notionAPIVersion    = "2022-06-28"
	defaultNotionParent  = ""
)

// NotionWriter handles Notion write operations.
type NotionWriter struct {
	apiKey     string
	httpClient *http.Client
	logger     *slog.Logger
}

// NewNotionWriter creates a NotionWriter.
func NewNotionWriter(apiKey string, logger *slog.Logger) *NotionWriter {
	return &NotionWriter{
		apiKey: apiKey,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		logger: logger.With("component", "notion-writer"),
	}
}

// CreatePage creates a new Notion page with the given title and content.
// If parentPageID is empty, uses the default root page.
// Returns the URL of the created page.
func (w *NotionWriter) CreatePage(ctx context.Context, title, content, parentPageID string) (string, error) {
	if parentPageID == "" {
		parentPageID = defaultNotionParent
	}

	body := map[string]any{
		"parent": map[string]any{
			"page_id": parentPageID,
		},
		"properties": map[string]any{
			"title": map[string]any{
				"title": []map[string]any{
					{
						"text": map[string]any{
							"content": title,
						},
					},
				},
			},
		},
	}

	if content != "" {
		body["children"] = contentToBlocks(content)
	}

	resp, err := w.apiRequest(ctx, "POST", "/pages", body)
	if err != nil {
		return "", fmt.Errorf("create notion page: %w", err)
	}

	url, _ := resp["url"].(string)
	id, _ := resp["id"].(string)
	w.logger.Info("created notion page", "title", title, "id", id, "url", url)
	return url, nil
}

// AppendToPage appends content blocks to an existing Notion page.
// Returns the URL of the page.
func (w *NotionWriter) AppendToPage(ctx context.Context, pageID, content string) (string, error) {
	blocks := contentToBlocks(content)

	body := map[string]any{
		"children": blocks,
	}

	_, err := w.apiRequest(ctx, "PATCH", "/blocks/"+pageID+"/children", body)
	if err != nil {
		return "", fmt.Errorf("append to notion page: %w", err)
	}

	// Fetch page to get URL
	pageResp, err := w.apiRequest(ctx, "GET", "/pages/"+pageID, nil)
	if err != nil {
		// Non-fatal: content was appended successfully
		w.logger.Warn("appended content but failed to get page URL", "page_id", pageID, "error", err)
		return fmt.Sprintf("https://notion.so/%s", strings.ReplaceAll(pageID, "-", "")), nil
	}

	url, _ := pageResp["url"].(string)
	w.logger.Info("appended to notion page", "page_id", pageID, "url", url)
	return url, nil
}

var numberedListRe = regexp.MustCompile(`^\d+\.\s+`)

// isSeparatorRow returns true if a table row line contains only |, -, :, and spaces.
func isSeparatorRow(line string) bool {
	for _, ch := range line {
		if ch != '|' && ch != '-' && ch != ':' && ch != ' ' && ch != '\t' {
			return false
		}
	}
	return true
}

// parseTableCells splits a pipe-delimited table row into trimmed cell strings.
// Strips leading/trailing | before splitting.
func parseTableCells(line string) []string {
	line = strings.TrimPrefix(line, "|")
	line = strings.TrimSuffix(line, "|")
	parts := strings.Split(line, "|")
	cells := make([]string, 0, len(parts))
	for _, p := range parts {
		cells = append(cells, strings.TrimSpace(p))
	}
	return cells
}

// makeRichText creates a rich_text array from text, chunking at 2000 chars.
func makeRichText(text string) []map[string]any {
	var richText []map[string]any
	for len(text) > 0 {
		chunk := text
		if len(chunk) > 2000 {
			chunk = text[:2000]
			for len(chunk) > 0 && !utf8.RuneStart(chunk[len(chunk)-1]) {
				chunk = chunk[:len(chunk)-1]
			}
			text = text[len(chunk):]
		} else {
			text = ""
		}
		richText = append(richText, map[string]any{
			"type": "text",
			"text": map[string]any{"content": chunk},
		})
	}
	return richText
}

// emitTable converts collected table lines into a single Notion table block.
func emitTable(tableLines []string) map[string]any {
	// Filter and parse rows, skipping separator rows
	type row struct {
		cells []string
	}
	var rows []row
	for _, l := range tableLines {
		if isSeparatorRow(l) {
			continue
		}
		rows = append(rows, row{cells: parseTableCells(l)})
	}
	if len(rows) == 0 {
		return nil
	}

	// Determine column count from first row
	numCols := len(rows[0].cells)

	// Build table_row children
	var children []map[string]any
	for _, r := range rows {
		cells := make([][][]map[string]any, numCols)
		for i := 0; i < numCols; i++ {
			cellText := ""
			if i < len(r.cells) {
				cellText = r.cells[i]
			}
			cells[i] = [][]map[string]any{makeRichText(cellText)}
		}
		// Notion expects cells as [][]rich_text, represented as []interface{} in JSON
		cellsAny := make([]any, numCols)
		for i, c := range cells {
			cellsAny[i] = c[0]
		}
		children = append(children, map[string]any{
			"object": "block",
			"type":   "table_row",
			"table_row": map[string]any{
				"cells": cellsAny,
			},
		})
	}

	return map[string]any{
		"object": "block",
		"type":   "table",
		"table": map[string]any{
			"table_width":       numCols,
			"has_column_header": true,
			"has_row_header":    false,
			"children":          children,
		},
	}
}

// contentToBlocks converts Markdown content into Notion block objects.
// Handles headings, bullet/numbered lists, code blocks, blockquotes, dividers, tables, and paragraphs.
func contentToBlocks(content string) []map[string]any {
	lines := strings.Split(content, "\n")
	var blocks []map[string]any

	inCodeBlock := false
	var codeLines []string

	inTable := false
	var tableLines []string

	for _, line := range lines {
		line = strings.TrimRight(line, " \t\r")

		// --- Code block state machine ---
		if strings.HasPrefix(line, "```") {
			if inCodeBlock {
				// End of code block: emit
				inCodeBlock = false
				codeText := strings.Join(codeLines, "\n")
				blocks = append(blocks, map[string]any{
					"object": "block",
					"type":   "code",
					"code": map[string]any{
						"rich_text": makeRichText(codeText),
						"language":  "plain text",
					},
				})
				codeLines = nil
			} else {
				// Start of code block: flush any pending table first
				if inTable {
					inTable = false
					if b := emitTable(tableLines); b != nil {
						blocks = append(blocks, b)
					}
					tableLines = nil
				}
				inCodeBlock = true
			}
			continue
		}

		if inCodeBlock {
			codeLines = append(codeLines, line)
			continue
		}

		// --- Table state machine ---
		if strings.HasPrefix(line, "|") {
			if !inTable {
				inTable = true
				tableLines = nil
			}
			tableLines = append(tableLines, line)
			continue
		} else if inTable {
			// Non-pipe line ends the table
			inTable = false
			if b := emitTable(tableLines); b != nil {
				blocks = append(blocks, b)
			}
			tableLines = nil
		}

		// Skip blank lines outside special blocks
		if line == "" {
			continue
		}

		// --- Per-line block types ---
		switch {
		case strings.HasPrefix(line, "# "):
			blocks = append(blocks, map[string]any{
				"object": "block",
				"type":   "heading_1",
				"heading_1": map[string]any{
					"rich_text": makeRichText(strings.TrimPrefix(line, "# ")),
				},
			})

		case strings.HasPrefix(line, "## "):
			blocks = append(blocks, map[string]any{
				"object": "block",
				"type":   "heading_2",
				"heading_2": map[string]any{
					"rich_text": makeRichText(strings.TrimPrefix(line, "## ")),
				},
			})

		case strings.HasPrefix(line, "### "):
			blocks = append(blocks, map[string]any{
				"object": "block",
				"type":   "heading_3",
				"heading_3": map[string]any{
					"rich_text": makeRichText(strings.TrimPrefix(line, "### ")),
				},
			})

		case strings.HasPrefix(line, "- ") || strings.HasPrefix(line, "• "):
			blocks = append(blocks, map[string]any{
				"object": "block",
				"type":   "bulleted_list_item",
				"bulleted_list_item": map[string]any{
					"rich_text": makeRichText(line[2:]),
				},
			})

		case numberedListRe.MatchString(line):
			text := numberedListRe.ReplaceAllString(line, "")
			blocks = append(blocks, map[string]any{
				"object": "block",
				"type":   "numbered_list_item",
				"numbered_list_item": map[string]any{
					"rich_text": makeRichText(text),
				},
			})

		case strings.HasPrefix(line, "> "):
			blocks = append(blocks, map[string]any{
				"object": "block",
				"type":   "quote",
				"quote": map[string]any{
					"rich_text": makeRichText(strings.TrimPrefix(line, "> ")),
				},
			})

		case line == "---" || line == "***" || line == "___":
			blocks = append(blocks, map[string]any{
				"object":  "block",
				"type":    "divider",
				"divider": map[string]any{},
			})

		default:
			blocks = append(blocks, makeParagraphBlock(line))
		}
	}

	// Flush any open code block
	if inCodeBlock && len(codeLines) > 0 {
		codeText := strings.Join(codeLines, "\n")
		blocks = append(blocks, map[string]any{
			"object": "block",
			"type":   "code",
			"code": map[string]any{
				"rich_text": makeRichText(codeText),
				"language":  "plain text",
			},
		})
	}

	// Flush any open table
	if inTable && len(tableLines) > 0 {
		if b := emitTable(tableLines); b != nil {
			blocks = append(blocks, b)
		}
	}

	if len(blocks) == 0 {
		blocks = append(blocks, makeParagraphBlock(content))
	}

	return blocks
}

func makeParagraphBlock(text string) map[string]any {
	// Notion has a 2000 char limit per rich_text element
	var richText []map[string]any
	for len(text) > 0 {
		chunk := text
		if len(chunk) > 2000 {
			chunk = text[:2000]
			for len(chunk) > 0 && !utf8.RuneStart(chunk[len(chunk)-1]) {
				chunk = chunk[:len(chunk)-1]
			}
			text = text[len(chunk):]
		} else {
			text = ""
		}
		richText = append(richText, map[string]any{
			"type": "text",
			"text": map[string]any{"content": chunk},
		})
	}

	return map[string]any{
		"object": "block",
		"type":   "paragraph",
		"paragraph": map[string]any{
			"rich_text": richText,
		},
	}
}

func (w *NotionWriter) apiRequest(ctx context.Context, method, path string, body any) (map[string]any, error) {
	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request: %w", err)
		}
		reqBody = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, notionAPIBase+path, reqBody)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+w.apiKey)
	req.Header.Set("Notion-Version", notionAPIVersion)
	req.Header.Set("Content-Type", "application/json")

	resp, err := w.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	respData, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIResponseSize))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("notion API error %d: %s", resp.StatusCode, string(respData))
	}

	var result map[string]any
	if err := json.Unmarshal(respData, &result); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}

	return result, nil
}
