package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/xylolabsinc/xylolabs-kb/internal/gemini"
)

// Document mirrors the API response structure.
type Document struct {
	ID          string            `json:"id"`
	Source      string            `json:"source"`
	SourceID    string            `json:"source_id"`
	Title       string            `json:"title"`
	Content     string            `json:"content"`
	ContentType string            `json:"content_type"`
	Author      string            `json:"author"`
	Channel     string            `json:"channel"`
	URL         string            `json:"url"`
	Timestamp   string            `json:"timestamp"`
	Metadata    map[string]string `json:"metadata"`
}

// APIResponse is the format returned by the Go worker API.
type APIResponse struct {
	Documents []Document `json:"documents"`
	Total     int64      `json:"total"`
}

// SyncState tracks last-processed timestamp per source.
type SyncState map[string]string

// DocumentMap tracks source_id -> file path mappings.
type DocumentMap map[string]string

// fileBlock represents a parsed file block from the Gemini response.
type fileBlock struct {
	Path    string
	Content string
}

// batch groups documents for a single Gemini call.
type batch struct {
	Key       string     // grouping key (e.g., "general/2024-01-15")
	Documents []Document
}

const (
	defaultModel         = "gemini-3.1-pro-preview"
	defaultThinkingLevel = "high"
	defaultMaxDocs       = 50
	maxDocContentChars   = 20000 // ~6.7k tokens; cap per document to limit batch payload
	fileBlockStart       = "===FILE: "
	fileBlockEnd         = "===ENDFILE==="
)

// parseRetryAfter extracts Retry-After seconds from an error message.
func parseRetryAfter(errMsg string) time.Duration {
	// Look for "retry-after: N" in the error message
	idx := strings.Index(errMsg, "retry-after: ")
	if idx < 0 {
		return 0
	}
	after := errMsg[idx+len("retry-after: "):]
	// Find the end (next ")" or end of string)
	if end := strings.IndexByte(after, ')'); end > 0 {
		after = after[:end]
	}
	if secs, err := strconv.Atoi(strings.TrimSpace(after)); err == nil {
		return time.Duration(secs) * time.Second
	}
	return 0
}

func main() {
	var (
		inputPath        string
		source           string
		kbDir            string
		apiKey           string
		model            string
		thinkingLevel    string
		maxDocs          int
		dryRun           bool
		force            bool
		fetchPeople      bool
		googleCredsFile  string
		impersonateEmail string
		domain           string
	)

	flag.StringVar(&inputPath, "input", "", "Path to raw documents JSON file (required)")
	flag.StringVar(&source, "source", "", "Source name: slack, google, notion (required)")
	flag.StringVar(&kbDir, "kb-dir", "", "Path to knowledge base repo directory (required)")
	flag.StringVar(&apiKey, "api-key", "", "Gemini API key (or GEMINI_API_KEY env var)")
	flag.StringVar(&model, "model", "", "Gemini model (default: gemini-3.1-pro-preview, or KB_GEN_MODEL env)")
	flag.StringVar(&thinkingLevel, "thinking", "", "Thinking level: none, low, medium, high (default: high, or KB_GEN_THINKING env)")
	flag.IntVar(&maxDocs, "max-docs", 0, "Max documents to process per batch (default: 50)")
	flag.BoolVar(&dryRun, "dry-run", false, "Print what would be written without writing files")
	flag.BoolVar(&force, "force", false, "Force reprocessing of all documents, ignoring document map")
	flag.BoolVar(&fetchPeople, "fetch-people", false, "Fetch Google Workspace directory and generate person knowledge files")
	flag.StringVar(&googleCredsFile, "google-creds", "", "Path to Google service account credentials JSON (or GOOGLE_CREDS_FILE env)")
	flag.StringVar(&impersonateEmail, "impersonate", "", "Email to impersonate for Admin SDK (or GOOGLE_IMPERSONATE_EMAIL env)")
	flag.StringVar(&domain, "domain", "", "Google Workspace domain (or GOOGLE_DOMAIN env, e.g. xylolabs.com)")
	flag.Parse()

	// Resolve defaults from env vars
	if apiKey == "" {
		apiKey = os.Getenv("GEMINI_API_KEY")
	}
	if model == "" {
		model = os.Getenv("KB_GEN_MODEL")
	}
	if model == "" {
		model = defaultModel
	}
	if thinkingLevel == "" {
		thinkingLevel = os.Getenv("KB_GEN_THINKING")
	}
	if thinkingLevel == "" {
		thinkingLevel = defaultThinkingLevel
	}
	if maxDocs == 0 {
		maxDocs = defaultMaxDocs
	}
	if googleCredsFile == "" {
		googleCredsFile = os.Getenv("GOOGLE_CREDS_FILE")
	}
	if impersonateEmail == "" {
		impersonateEmail = os.Getenv("GOOGLE_IMPERSONATE_EMAIL")
	}
	if domain == "" {
		domain = os.Getenv("GOOGLE_DOMAIN")
	}

	// Set up logger
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// People fetch mode — runs independently of normal document processing
	if fetchPeople {
		if googleCredsFile == "" || domain == "" {
			fmt.Fprintf(os.Stderr, "Error: --google-creds and --domain are required for --fetch-people\n")
			os.Exit(1)
		}
		if kbDir == "" {
			fmt.Fprintf(os.Stderr, "Error: --kb-dir is required\n")
			os.Exit(1)
		}
		if err := fetchAndWritePeople(googleCredsFile, impersonateEmail, domain, kbDir, dryRun, logger); err != nil {
			logger.Error("failed to fetch people", "error", err)
			os.Exit(1)
		}
		return
	}

	// Validate required flags for normal document processing
	if inputPath == "" || source == "" || kbDir == "" {
		fmt.Fprintf(os.Stderr, "Usage: kb-gen --input raw.json --source slack --kb-dir /opt/knowledge [options]\n\n")
		flag.PrintDefaults()
		os.Exit(1)
	}
	if apiKey == "" {
		fmt.Fprintf(os.Stderr, "Error: Gemini API key required (--api-key or GEMINI_API_KEY env var)\n")
		os.Exit(1)
	}

	logger.Info("kb-gen starting",
		"input", inputPath,
		"source", source,
		"kb_dir", kbDir,
		"model", model,
		"thinking", thinkingLevel,
		"max_docs", maxDocs,
		"dry_run", dryRun,
	)

	// Read and parse input
	apiResp, err := readInput(inputPath)
	if err != nil {
		logger.Error("failed to read input", "error", err)
		os.Exit(1)
	}

	if len(apiResp.Documents) == 0 {
		logger.Info("no documents to process")
		return
	}

	logger.Info("loaded documents", "count", len(apiResp.Documents), "total", apiResp.Total)

	// Filter out already-processed documents (skip if --force)
	if !force {
		existingMap := loadDocumentMap(kbDir)
		var newDocs []Document
		for _, doc := range apiResp.Documents {
			if doc.SourceID != "" {
				if _, exists := existingMap[doc.SourceID]; exists {
					continue
				}
			}
			newDocs = append(newDocs, doc)
		}
		skipped := len(apiResp.Documents) - len(newDocs)
		if skipped > 0 {
			logger.Info("skipped already-processed documents", "skipped", skipped, "remaining", len(newDocs))
		}
		apiResp.Documents = newDocs
		if len(apiResp.Documents) == 0 {
			logger.Info("all documents already processed, nothing to do")
			return
		}
	}

	// Read CLAUDE.md for curation instructions
	curatorInstructions := readCuratorInstructions(kbDir, logger)

	// Create Gemini client with extended timeout for KB generation
	client := gemini.NewClient(apiKey, model, logger)
	client.SetTimeout(10 * time.Minute) // KB generation with thinking needs longer than default 120s

	// Group documents into batches
	batches := groupDocuments(apiResp.Documents, source, maxDocs)
	logger.Info("grouped into batches", "batch_count", len(batches))

	// Process each batch
	var (
		totalFilesWritten int
		totalTokensUsed   int
		allDocMappings    = make(DocumentMap)
		latestTimestamp    string
	)

	for i, b := range batches {
		logger.Info("processing batch",
			"batch", i+1,
			"total_batches", len(batches),
			"key", b.Key,
			"doc_count", len(b.Documents),
		)

		// Rate limit: wait between batches to avoid quota exhaustion.
		if i > 0 {
			logger.Debug("rate limit pause between batches", "wait", "20s")
			time.Sleep(20 * time.Second)
		}

		// Retry with exponential backoff on 429/503 errors.
		var blocks []fileBlock
		var tokensUsed int
		var err error
		maxRetries := 5
		for attempt := 0; attempt < maxRetries; attempt++ {
			blocks, tokensUsed, err = processBatch(context.Background(), client, b, source, curatorInstructions, thinkingLevel, logger)
			if err == nil {
				break
			}
			errMsg := err.Error()
			isRetryable := strings.Contains(errMsg, "429") || strings.Contains(errMsg, "503")
			if !isRetryable || attempt >= maxRetries-1 {
				break
			}
			// Use Retry-After header if available, otherwise exponential backoff with jitter
			wait := parseRetryAfter(errMsg)
			if wait == 0 {
				baseWait := time.Duration(1<<uint(attempt)) * 15 * time.Second // 15s, 30s, 60s, 120s
				jitter := time.Duration(rand.IntN(int(baseWait / 4)))
				wait = baseWait + jitter
			}
			logger.Warn("rate limited, retrying", "batch", b.Key, "attempt", attempt+1, "wait", wait)
			time.Sleep(wait)
		}
		if err != nil {
			logger.Error("failed to process batch", "batch", b.Key, "error", err)
			continue
		}

		totalTokensUsed += tokensUsed

		batchWritesBefore := totalFilesWritten
		for _, block := range blocks {
			// Skip index/README files — these must be built separately to avoid
			// overwriting aggregated data with single-batch output.
			if isIndexFile(block.Path) {
				logger.Debug("skipping index file from batch output", "path", block.Path)
				continue
			}

			if dryRun {
				fmt.Printf("[dry-run] Would write: %s (%d bytes)\n", block.Path, len(block.Content))
				continue
			}

			fullPath := filepath.Join(kbDir, block.Path)
			if err := writeFile(fullPath, block.Content); err != nil {
				logger.Error("failed to write file", "path", block.Path, "error", err)
				continue
			}

			logger.Info("wrote file", "path", block.Path, "size", len(block.Content))
			totalFilesWritten++
		}

		// Map source_ids to generated file paths
		for _, doc := range b.Documents {
			if doc.SourceID != "" && len(blocks) > 0 {
				// Map each document to the first relevant file block
				allDocMappings[doc.SourceID] = blocks[0].Path
			}
		}

		// Track latest timestamp only when files were actually written successfully
		if totalFilesWritten > batchWritesBefore {
			for _, doc := range b.Documents {
				if doc.Timestamp > latestTimestamp {
					latestTimestamp = doc.Timestamp
				}
			}
		}
	}

	if !dryRun && latestTimestamp != "" {
		// Update sync state
		if err := updateSyncState(kbDir, source, latestTimestamp); err != nil {
			logger.Error("failed to update sync state", "error", err)
		}

		// Update document map
		if err := updateDocumentMap(kbDir, allDocMappings); err != nil {
			logger.Error("failed to update document map", "error", err)
		}
	}

	logger.Info("kb-gen complete",
		"files_written", totalFilesWritten,
		"tokens_used", totalTokensUsed,
		"latest_timestamp", latestTimestamp,
		"dry_run", dryRun,
	)
}

// readInput reads and parses the API response JSON file.
func readInput(path string) (*APIResponse, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read input file: %w", err)
	}

	var resp APIResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("parse input JSON: %w", err)
	}

	return &resp, nil
}

// readCuratorInstructions reads CLAUDE.md from the KB repo root if it exists.
func readCuratorInstructions(kbDir string, logger *slog.Logger) string {
	claudeMD := filepath.Join(kbDir, "CLAUDE.md")
	data, err := os.ReadFile(claudeMD)
	if err != nil {
		logger.Info("no CLAUDE.md found in KB repo, using default instructions")
		return ""
	}
	logger.Info("loaded CLAUDE.md", "size", len(data))
	return string(data)
}

// groupDocuments splits documents into batches appropriate for the source type.
func groupDocuments(docs []Document, source string, maxDocs int) []batch {
	switch source {
	case "slack", "discord":
		return groupSlackDocuments(docs, maxDocs)
	case "manual":
		return groupManualDocuments(docs, maxDocs)
	default:
		return groupGenericDocuments(docs, maxDocs)
	}
}

// groupSlackDocuments groups Slack messages by channel+date.
func groupSlackDocuments(docs []Document, maxDocs int) []batch {
	groups := make(map[string][]Document)

	for _, doc := range docs {
		channel := doc.Channel
		if channel == "" {
			channel = "general"
		}
		// Sanitize channel name for use as a key
		channel = sanitizeSlug(channel)

		date := "unknown"
		if doc.Timestamp != "" {
			if t, err := time.Parse(time.RFC3339, doc.Timestamp); err == nil {
				date = t.Format("2006-01-02")
			} else if t, err := time.Parse(time.RFC3339Nano, doc.Timestamp); err == nil {
				date = t.Format("2006-01-02")
			}
		}

		key := channel + "/" + date
		groups[key] = append(groups[key], doc)
	}

	// Sort keys for deterministic ordering
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var batches []batch
	for _, key := range keys {
		groupDocs := groups[key]
		// Split large groups into sub-batches
		for i := 0; i < len(groupDocs); i += maxDocs {
			end := i + maxDocs
			if end > len(groupDocs) {
				end = len(groupDocs)
			}
			batches = append(batches, batch{
				Key:       key,
				Documents: groupDocs[i:end],
			})
		}
	}

	return batches
}

// groupGenericDocuments groups documents in sequential chunks.
func groupGenericDocuments(docs []Document, maxDocs int) []batch {
	var batches []batch
	for i := 0; i < len(docs); i += maxDocs {
		end := i + maxDocs
		if end > len(docs) {
			end = len(docs)
		}
		key := fmt.Sprintf("batch-%d", i/maxDocs+1)
		batches = append(batches, batch{
			Key:       key,
			Documents: docs[i:end],
		})
	}
	return batches
}

// groupManualDocuments groups manual documents by category (channel field).
func groupManualDocuments(docs []Document, maxDocs int) []batch {
	groups := make(map[string][]Document)

	for _, doc := range docs {
		category := doc.Channel
		if category == "" {
			category = "uncategorized"
		}
		category = sanitizeSlug(category)
		groups[category] = append(groups[category], doc)
	}

	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var batches []batch
	for _, key := range keys {
		groupDocs := groups[key]
		for i := 0; i < len(groupDocs); i += maxDocs {
			end := i + maxDocs
			if end > len(groupDocs) {
				end = len(groupDocs)
			}
			batches = append(batches, batch{
				Key:       "manual/" + key,
				Documents: groupDocs[i:end],
			})
		}
	}

	return batches
}

// processBatch sends a batch of documents to Gemini and parses file blocks from the response.
func processBatch(
	ctx context.Context,
	client *gemini.Client,
	b batch,
	source string,
	curatorInstructions string,
	thinkingLevel string,
	logger *slog.Logger,
) ([]fileBlock, int, error) {
	systemPrompt := buildSystemPrompt(source, curatorInstructions)

	// Truncate large document contents to limit token usage
	docs := make([]Document, len(b.Documents))
	copy(docs, b.Documents)
	for i := range docs {
		if len(docs[i].Content) > maxDocContentChars {
			logger.Debug("truncated large document", "source_id", docs[i].SourceID, "original_len", len(b.Documents[i].Content))
			docs[i].Content = docs[i].Content[:maxDocContentChars] + "\n\n[... content truncated ...]"
		}
	}

	docsJSON, err := json.MarshalIndent(docs, "", "  ")
	if err != nil {
		return nil, 0, fmt.Errorf("marshal documents: %w", err)
	}

	userMessage := fmt.Sprintf(
		"Process the following %d %s documents (batch key: %s) into structured markdown files for the knowledge base.\n\n"+
			"Raw documents JSON:\n```json\n%s\n```",
		len(b.Documents), source, b.Key, string(docsJSON),
	)

	genReq := gemini.GenerateRequest{
		SystemPrompt:  systemPrompt,
		Messages:      []gemini.Message{{Role: "user", Content: userMessage}},
		ThinkingLevel: thinkingLevel,
	}

	resp, err := client.Generate(ctx, genReq)
	if err != nil {
		return nil, 0, fmt.Errorf("gemini generate: %w", err)
	}

	logger.Debug("gemini response received",
		"text_length", len(resp.Text),
		"thinking_length", len(resp.Thinking),
		"tokens_used", resp.TokensUsed,
	)

	blocks := parseFileBlocks(resp.Text)
	if len(blocks) == 0 {
		logger.Warn("no file blocks parsed from response", "batch", b.Key, "response_length", len(resp.Text))
	}

	return blocks, resp.TokensUsed, nil
}

// isIndexFile returns true if the path is an index or README file that should not
// be overwritten by per-batch output (these must be rebuilt from all data).
func isIndexFile(path string) bool {
	base := filepath.Base(path)
	if strings.EqualFold(base, "README.md") {
		return true
	}
	if strings.HasPrefix(path, "indexes/") || strings.HasPrefix(path, "indexes\\") {
		return true
	}
	return false
}

// loadDocumentMap reads the existing document map from _meta/document-map.json.
func loadDocumentMap(kbDir string) DocumentMap {
	mapFile := filepath.Join(kbDir, "_meta", "document-map.json")
	data, err := os.ReadFile(mapFile)
	if err != nil {
		return make(DocumentMap)
	}
	var m DocumentMap
	if err := json.Unmarshal(data, &m); err != nil {
		return make(DocumentMap)
	}
	return m
}

// buildSystemPrompt constructs the system prompt for Gemini.
func buildSystemPrompt(source, curatorInstructions string) string {
	var sb strings.Builder

	sb.WriteString(`You are a knowledge base curator. Process the provided raw documents and produce structured markdown files for a knowledge base repository.

`)

	if curatorInstructions != "" {
		sb.WriteString("## Knowledge Base Curation Instructions\n\n")
		sb.WriteString(curatorInstructions)
		sb.WriteString("\n\n")
	}

	sb.WriteString(`## IMPORTANT: Do NOT generate index or README files

Do NOT output any of the following files — they are maintained separately and will be overwritten:
- README.md (any directory)
- indexes/*.md (people.md, topics.md, keywords.md, cross-references.md, taxonomy.md, etc.)
- slack/README.md, google/README.md, notion/README.md

Only output the actual content files (daily digests, document summaries, etc.).

## Output Format

For each file you want to create or update, output it in this exact format:

===FILE: relative/path/to/file.md===
(complete file content with YAML frontmatter)
===ENDFILE===

You may output multiple file blocks. Each file must be complete and self-contained.

## File Naming Conventions

`)

	switch source {
	case "slack":
		sb.WriteString(`- Slack messages: slack/channels/{channel-name}/{YYYY-MM-DD}.md (daily digests per channel)
- Channel names should be lowercase, hyphenated (e.g., "engineering", "product-updates")
`)
	case "discord":
		sb.WriteString(`- Discord messages: discord/channels/{channel-name}/{YYYY-MM-DD}.md (daily digests per channel)
- Channel names should be lowercase, hyphenated (e.g., "general", "dev-chat")
`)
	case "google":
		sb.WriteString(`- Google docs: google/docs/{doc-slug}.md
- Doc slugs should be lowercase, hyphenated versions of the document title
`)
	case "notion":
		sb.WriteString(`- Notion pages: notion/pages/{page-slug}.md
- Page slugs should be lowercase, hyphenated versions of the page title
`)
	case "manual":
		sb.WriteString("- Manual documents: manual/{category}/{doc-slug}.md\n- Categories should be lowercase, hyphenated\n")
	default:
		sb.WriteString(fmt.Sprintf("- %s documents: %s/{doc-slug}.md\n", source, source))
	}

	sb.WriteString(`
## Content Structure

Each markdown file MUST have YAML frontmatter with:
- title: descriptive title
- date: ISO 8601 date (YYYY-MM-DD)
- source: the source system (slack, google, notion)
- channel: channel name (if applicable, for Slack)
- authors: list of authors/contributors

`)

	sb.WriteString(`## Attachments and Links

Documents may contain extracted content from file attachments and external links, delimited by:
- "---\nAttached: filename\n..." — text extracted from file attachments (PDF, DOCX, XLSX, PPTX, images)
- "---\nLink: url\n..." — text extracted from URLs found in the message

When processing these:
- Summarize attachment content naturally within the document (don't just copy raw extracted text)
- For images described by AI, include a brief description in context
- Preserve the original URL or link for reference using the "url" frontmatter field
- For important documents (contracts, certificates, financial docs), note the attachment name and key details
- If a message is primarily sharing a file/link, the curated entry should focus on what the file contains

`)

	if source == "slack" || source == "discord" {
		sb.WriteString(`For Slack daily digests:
- Create a chronological summary of the day's conversations
- Highlight key topics, decisions, and action items
- Group related messages into conversation threads
- Include a "Messages" section with a table: | Time | Author | Message |
- Include a "Key Topics" section summarizing main discussion points
- Include an "Action Items" section if any were identified
- When messages include file attachments or links, summarize the attachment content inline
`)
	}

	return sb.String()
}

// parseFileBlocks extracts ===FILE: path===...===ENDFILE=== blocks from the response text.
func parseFileBlocks(text string) []fileBlock {
	var blocks []fileBlock

	// Use regex to find file blocks
	re := regexp.MustCompile(`(?m)^===FILE:\s*(.+?)\s*===$`)
	matches := re.FindAllStringSubmatchIndex(text, -1)

	for i, match := range matches {
		if len(match) < 4 {
			continue
		}

		path := text[match[2]:match[3]]
		// Clean the path
		path = strings.TrimSpace(path)
		// Normalize underscores to hyphens to prevent duplicate folders
		path = normalizeFilePath(path)

		// Find the content between this FILE marker and the next ENDFILE
		contentStart := match[1] // end of the ===FILE: ...=== line
		// Skip the newline after the FILE marker
		if contentStart < len(text) && text[contentStart] == '\n' {
			contentStart++
		}

		// Find ENDFILE after this position
		endMarker := "===ENDFILE==="
		endIdx := strings.Index(text[contentStart:], endMarker)
		if endIdx == -1 {
			// If no ENDFILE found and this is the last block, take everything to end
			if i == len(matches)-1 {
				content := strings.TrimRight(text[contentStart:], "\n\r ")
				blocks = append(blocks, fileBlock{Path: path, Content: content})
			}
			continue
		}

		content := text[contentStart : contentStart+endIdx]
		// Trim trailing whitespace/newlines from content
		content = strings.TrimRight(content, "\n\r ")
		// Ensure file ends with a newline
		content += "\n"

		blocks = append(blocks, fileBlock{Path: path, Content: content})
	}

	return blocks
}

// writeFile creates necessary directories and writes the file content.
func writeFile(path, content string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create directory %s: %w", dir, err)
	}

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write file %s: %w", path, err)
	}

	return nil
}

// updateSyncState updates _meta/sync-state.json with the latest timestamp for the given source.
func updateSyncState(kbDir, source, timestamp string) error {
	metaDir := filepath.Join(kbDir, "_meta")
	if err := os.MkdirAll(metaDir, 0o755); err != nil {
		return fmt.Errorf("create _meta dir: %w", err)
	}

	syncFile := filepath.Join(metaDir, "sync-state.json")

	state := make(SyncState)
	if data, err := os.ReadFile(syncFile); err == nil {
		if err := json.Unmarshal(data, &state); err != nil {
			// If corrupt, start fresh
			state = make(SyncState)
		}
	}

	state[source] = timestamp

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal sync state: %w", err)
	}

	if err := os.WriteFile(syncFile, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write sync state: %w", err)
	}

	return nil
}

// updateDocumentMap merges new source_id -> path mappings into _meta/document-map.json.
func updateDocumentMap(kbDir string, newMappings DocumentMap) error {
	if len(newMappings) == 0 {
		return nil
	}

	metaDir := filepath.Join(kbDir, "_meta")
	if err := os.MkdirAll(metaDir, 0o755); err != nil {
		return fmt.Errorf("create _meta dir: %w", err)
	}

	mapFile := filepath.Join(metaDir, "document-map.json")

	existing := make(DocumentMap)
	if data, err := os.ReadFile(mapFile); err == nil {
		if err := json.Unmarshal(data, &existing); err != nil {
			existing = make(DocumentMap)
		}
	}

	// Merge new mappings
	for k, v := range newMappings {
		existing[k] = v
	}

	data, err := json.MarshalIndent(existing, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal document map: %w", err)
	}

	if err := os.WriteFile(mapFile, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write document map: %w", err)
	}

	return nil
}

// normalizeFilePath normalizes underscores to hyphens in path segments
// to prevent duplicate folders (e.g., "02_rnd" vs "02-rnd").
func normalizeFilePath(p string) string {
	parts := strings.Split(p, "/")
	for i, part := range parts {
		// Don't touch the filename (last segment) if it's a date-based .md file
		if i == len(parts)-1 {
			break
		}
		parts[i] = strings.ReplaceAll(part, "_", "-")
	}
	return strings.Join(parts, "/")
}

// sanitizeSlug converts a string to a filesystem-safe slug.
func sanitizeSlug(s string) string {
	s = strings.ToLower(s)
	s = strings.TrimPrefix(s, "#")
	s = strings.TrimSpace(s)
	// Replace non-alphanumeric characters with hyphens
	re := regexp.MustCompile(`[^a-z0-9-]+`)
	s = re.ReplaceAllString(s, "-")
	// Collapse multiple hyphens
	re = regexp.MustCompile(`-{2,}`)
	s = re.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		s = "general"
	}
	return s
}
