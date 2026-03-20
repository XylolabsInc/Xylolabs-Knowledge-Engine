package main

import (
	"bufio"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// digestInfo holds parsed metadata from a daily digest file.
type digestInfo struct {
	Date         string
	Participants []string
	Keywords     []string
	Topics       []string
}

// channelSummary aggregates info about a channel from its digest files.
type channelSummary struct {
	Name      string
	Digests   []digestInfo
	LastDate  string
	FileCount int
}

// docInfo holds parsed metadata from a google/notion document file.
type docInfo struct {
	Slug      string
	Title     string
	Author    string
	URL       string
	UpdatedAt string
}

// rebuildAllIndexes rebuilds index files for all sources.
func rebuildAllIndexes(kbDir string, dryRun bool, logger *slog.Logger) error {
	logger.Info("rebuilding all indexes", "kb_dir", kbDir)

	for _, source := range []string{"slack", "discord", "google", "notion"} {
		if err := rebuildSourceIndexes(kbDir, source, dryRun, logger); err != nil {
			logger.Warn("failed to rebuild indexes for source", "source", source, "error", err)
		}
	}

	logger.Info("index rebuild complete")
	return nil
}

// rebuildSourceIndexes rebuilds index files for a specific source.
func rebuildSourceIndexes(kbDir, source string, dryRun bool, logger *slog.Logger) error {
	switch source {
	case "slack", "discord":
		return rebuildChannelIndexes(kbDir, source, dryRun, logger)
	case "google":
		return rebuildDocSourceIndex(kbDir, "google", "docs", dryRun, logger)
	case "notion":
		return rebuildDocSourceIndex(kbDir, "notion", "pages", dryRun, logger)
	default:
		return nil
	}
}

// rebuildChannelIndexes scans channel directories and rebuilds per-channel
// README.md files and the master source README.md.
func rebuildChannelIndexes(kbDir, source string, dryRun bool, logger *slog.Logger) error {
	channelsDir := filepath.Join(kbDir, source, "channels")

	entries, err := os.ReadDir(channelsDir)
	if err != nil {
		if os.IsNotExist(err) {
			logger.Debug("no channels directory, skipping", "source", source)
			return nil
		}
		return fmt.Errorf("read channels dir %s: %w", channelsDir, err)
	}

	var channels []channelSummary

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		channelName := entry.Name()
		channelDir := filepath.Join(channelsDir, channelName)

		files, err := os.ReadDir(channelDir)
		if err != nil {
			logger.Warn("failed to read channel dir", "channel", channelName, "error", err)
			continue
		}

		var digests []digestInfo
		for _, f := range files {
			name := f.Name()
			if f.IsDir() || !strings.HasSuffix(name, ".md") || strings.EqualFold(name, "README.md") {
				continue
			}

			filePath := filepath.Join(channelDir, name)
			di := parseDigestFrontmatter(filePath)
			if di.Date == "" {
				// Fall back to filename (e.g., "2024-01-15.md" -> "2024-01-15")
				di.Date = strings.TrimSuffix(name, ".md")
			}
			digests = append(digests, di)
		}

		if len(digests) == 0 {
			continue
		}

		// Sort by date descending
		sort.Slice(digests, func(i, j int) bool {
			return digests[i].Date > digests[j].Date
		})

		ch := channelSummary{
			Name:      channelName,
			Digests:   digests,
			LastDate:  digests[0].Date,
			FileCount: len(digests),
		}
		channels = append(channels, ch)

		// Write per-channel README.md
		if err := generateChannelReadme(kbDir, source, ch, dryRun, logger); err != nil {
			logger.Warn("failed to write channel index", "channel", channelName, "error", err)
		}
	}

	// Sort channels by name
	sort.Slice(channels, func(i, j int) bool {
		return channels[i].Name < channels[j].Name
	})

	// Write master source README.md
	return generateSourceReadme(kbDir, source, channels, dryRun, logger)
}

// parseMarkdownSections splits a markdown file into sections by ## headers.
// Returns a slice of {header, body} pairs. Content before the first ## header
// is returned with an empty header string.
type markdownSection struct {
	Header string // e.g. "## Purpose" (empty for preamble)
	Body   string
}

func parseMarkdownSections(content string) []markdownSection {
	var sections []markdownSection
	lines := strings.Split(content, "\n")
	var currentHeader string
	var currentBody strings.Builder

	for _, line := range lines {
		if strings.HasPrefix(line, "## ") {
			// Save previous section
			sections = append(sections, markdownSection{
				Header: currentHeader,
				Body:   currentBody.String(),
			})
			currentHeader = line
			currentBody.Reset()
		} else {
			if currentBody.Len() > 0 || line != "" {
				if currentBody.Len() > 0 {
					currentBody.WriteString("\n")
				}
				currentBody.WriteString(line)
			}
		}
	}
	// Save last section
	sections = append(sections, markdownSection{
		Header: currentHeader,
		Body:   currentBody.String(),
	})

	return sections
}

// generateChannelReadme writes a README.md for a single channel.
// It preserves existing hand-written or Gemini-generated sections (e.g. Purpose,
// Recurring Topics) and only regenerates the frontmatter and Recent Activity table.
func generateChannelReadme(kbDir, source string, ch channelSummary, dryRun bool, logger *slog.Logger) error {
	relPath := filepath.Join(source, "channels", ch.Name, "README.md")
	fullPath := filepath.Join(kbDir, relPath)

	// Read existing file to preserve non-auto-generated sections
	var preservedSections []markdownSection
	if existing, err := os.ReadFile(fullPath); err == nil {
		sections := parseMarkdownSections(string(existing))
		for _, s := range sections {
			headerName := strings.TrimSpace(strings.TrimPrefix(s.Header, "##"))
			// Skip sections we regenerate: preamble (frontmatter+title), Recent Activity
			if s.Header == "" || strings.EqualFold(headerName, "Recent Activity") {
				continue
			}
			preservedSections = append(preservedSections, s)
		}
	}

	// Aggregate participants and keywords across all digests
	participantSet := make(map[string]struct{})
	keywordSet := make(map[string]struct{})
	for _, d := range ch.Digests {
		for _, p := range d.Participants {
			participantSet[p] = struct{}{}
		}
		for _, k := range d.Keywords {
			keywordSet[k] = struct{}{}
		}
	}

	participants := sortedMapKeys(participantSet)
	keywords := sortedMapKeys(keywordSet)

	var sb strings.Builder
	sb.WriteString("---\n")
	sb.WriteString(fmt.Sprintf("channel: \"%s\"\n", ch.Name))
	if len(participants) > 0 {
		sb.WriteString(fmt.Sprintf("key_members: [%s]\n", strings.Join(yamlQuotedList(participants), ", ")))
	}
	if len(keywords) > 0 {
		top := keywords
		if len(top) > 10 {
			top = top[:10]
		}
		sb.WriteString(fmt.Sprintf("recurring_topics: [%s]\n", strings.Join(yamlQuotedList(top), ", ")))
	}
	sb.WriteString(fmt.Sprintf("last_updated: \"%s\"\n", ch.LastDate))
	sb.WriteString(fmt.Sprintf("total_digests: %d\n", ch.FileCount))
	sb.WriteString("---\n\n")

	sb.WriteString(fmt.Sprintf("# #%s\n\n", ch.Name))

	// Write preserved sections (Purpose, Recurring Topics, etc.) before Recent Activity
	for _, s := range preservedSections {
		sb.WriteString(s.Header + "\n\n")
		body := strings.TrimSpace(s.Body)
		if body != "" {
			sb.WriteString(body + "\n\n")
		}
	}

	sb.WriteString("## Recent Activity\n\n")
	sb.WriteString("| Date | Participants | Keywords |\n")
	sb.WriteString("|------|-------------|----------|\n")

	limit := 30
	if len(ch.Digests) < limit {
		limit = len(ch.Digests)
	}
	for _, d := range ch.Digests[:limit] {
		pStr := strings.Join(d.Participants, ", ")
		kStr := strings.Join(d.Keywords, ", ")
		if len(pStr) > 60 {
			pStr = pStr[:57] + "..."
		}
		if len(kStr) > 60 {
			kStr = kStr[:57] + "..."
		}
		sb.WriteString(fmt.Sprintf("| [%s](./%s.md) | %s | %s |\n", d.Date, d.Date, pStr, kStr))
	}

	if len(ch.Digests) > limit {
		sb.WriteString(fmt.Sprintf("\n_...and %d more digests._\n", len(ch.Digests)-limit))
	}
	sb.WriteString("\n")

	content := sb.String()

	if dryRun {
		fmt.Printf("[dry-run] Would write index: %s (%d bytes)\n", relPath, len(content))
		return nil
	}

	logger.Info("wrote channel index", "path", relPath)
	return writeFile(fullPath, content)
}

// parseSourceTableRow extracts channel name and extra columns from a markdown table row.
// Returns channelName and a map of extra values keyed by column header.
func parseSourceTableRow(row string, headers []string) (string, map[string]string) {
	cells := strings.Split(strings.Trim(row, "|"), "|")
	extras := make(map[string]string)
	var channelName string

	for i, cell := range cells {
		cell = strings.TrimSpace(cell)
		if i == 0 {
			// Extract channel name from "[#name](./channels/name/)" or plain "#name"
			if idx := strings.Index(cell, "]("); idx > 0 {
				channelName = strings.TrimPrefix(cell[:idx], "[#")
			} else {
				channelName = strings.TrimPrefix(cell, "#")
			}
		} else if i < len(headers) {
			// Store extra columns (Purpose, Recent Topics, etc.) beyond the standard ones
			header := strings.TrimSpace(headers[i])
			if header != "Last Activity" && header != "Digests" {
				extras[header] = cell
			}
		}
	}
	return channelName, extras
}

// generateSourceReadme writes the master README.md for a source (e.g., slack/README.md).
// It preserves extra columns (Purpose, Recent Topics) and non-table sections from the existing file.
func generateSourceReadme(kbDir, source string, channels []channelSummary, dryRun bool, logger *slog.Logger) error {
	relPath := filepath.Join(source, "README.md")
	fullPath := filepath.Join(kbDir, relPath)

	// Read existing file to preserve extra table columns and non-Channels sections
	existingExtras := make(map[string]map[string]string) // channelName → {colHeader → value}
	var extraHeaders []string                             // extra column headers beyond standard ones
	var preservedSections []markdownSection

	if existing, err := os.ReadFile(fullPath); err == nil {
		sections := parseMarkdownSections(string(existing))
		for _, s := range sections {
			headerName := strings.TrimSpace(strings.TrimPrefix(s.Header, "##"))
			if strings.EqualFold(headerName, "Channels") {
				// Parse the table to extract extra columns
				lines := strings.Split(s.Body, "\n")
				var tableHeaders []string
				for _, line := range lines {
					line = strings.TrimSpace(line)
					if line == "" || strings.HasPrefix(line, "<!--") || line == "_No channels synced yet._" {
						continue
					}
					if strings.Contains(line, "---") && strings.HasPrefix(line, "|") {
						continue // separator row
					}
					if strings.HasPrefix(line, "|") {
						if len(tableHeaders) == 0 {
							// Parse header row
							cells := strings.Split(strings.Trim(line, "|"), "|")
							for _, c := range cells {
								tableHeaders = append(tableHeaders, strings.TrimSpace(c))
							}
							// Identify extra headers beyond Channel, Last Activity, Digests
							for _, h := range tableHeaders {
								if h != "Channel" && h != "Last Activity" && h != "Digests" && h != "" {
									extraHeaders = append(extraHeaders, h)
								}
							}
						} else {
							// Data row
							chName, extras := parseSourceTableRow(line, tableHeaders)
							if chName != "" && len(extras) > 0 {
								existingExtras[chName] = extras
							}
						}
					}
				}
			} else if s.Header != "" {
				// Preserve non-Channels sections
				preservedSections = append(preservedSections, s)
			}
		}
	}

	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("# %s\n\n", sourceDisplayName(source)))
	sb.WriteString("Message digests organized by channel and date.\n\n")

	// Write preserved sections before Channels
	for _, s := range preservedSections {
		sb.WriteString(s.Header + "\n\n")
		body := strings.TrimSpace(s.Body)
		if body != "" {
			sb.WriteString(body + "\n\n")
		}
	}

	sb.WriteString("## Channels\n\n")

	if len(channels) == 0 {
		sb.WriteString("_No channels synced yet._\n")
	} else {
		// Build header row with any extra columns
		headerRow := "| Channel | Last Activity | Digests |"
		sepRow := "|---------|--------------|--------|"
		for _, h := range extraHeaders {
			headerRow += fmt.Sprintf(" %s |", h)
			sepRow += strings.Repeat("-", len(h)+2) + "|"
		}
		sb.WriteString(headerRow + "\n")
		sb.WriteString(sepRow + "\n")

		for _, ch := range channels {
			row := fmt.Sprintf("| [#%s](./channels/%s/) | %s | %d |",
				ch.Name, ch.Name, ch.LastDate, ch.FileCount)
			// Carry forward extra column values from existing table
			if extras, ok := existingExtras[ch.Name]; ok {
				for _, h := range extraHeaders {
					row += fmt.Sprintf(" %s |", extras[h])
				}
			} else {
				for range extraHeaders {
					row += " |"
				}
			}
			sb.WriteString(row + "\n")
		}
	}

	sb.WriteString(fmt.Sprintf("\n<!-- Auto-generated by kb-gen at %s -->\n",
		time.Now().UTC().Format(time.RFC3339)))

	content := sb.String()

	if dryRun {
		fmt.Printf("[dry-run] Would write index: %s (%d bytes)\n", relPath, len(content))
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}

	logger.Info("wrote source index", "path", relPath, "channels", len(channels))
	return writeFile(fullPath, content)
}

// rebuildDocSourceIndex rebuilds the README.md for a document source (google/notion).
// It preserves extra table columns and non-Documents sections from the existing file.
func rebuildDocSourceIndex(kbDir, source, subDir string, dryRun bool, logger *slog.Logger) error {
	docsDir := filepath.Join(kbDir, source, subDir)

	entries, err := os.ReadDir(docsDir)
	if err != nil {
		if os.IsNotExist(err) {
			logger.Debug("no docs directory, skipping", "source", source)
			return nil
		}
		return fmt.Errorf("read docs dir %s: %w", docsDir, err)
	}

	var docs []docInfo
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".md") || strings.EqualFold(name, "README.md") {
			continue
		}

		filePath := filepath.Join(docsDir, name)
		doc := parseDocFrontmatter(filePath)
		doc.Slug = strings.TrimSuffix(name, ".md")
		docs = append(docs, doc)
	}

	// Sort by updated_at descending, then by title
	sort.Slice(docs, func(i, j int) bool {
		if docs[i].UpdatedAt != docs[j].UpdatedAt {
			return docs[i].UpdatedAt > docs[j].UpdatedAt
		}
		return docs[i].Title < docs[j].Title
	})

	relPath := filepath.Join(source, "README.md")
	fullPath := filepath.Join(kbDir, relPath)

	// Read existing file to preserve extra table columns and non-Documents sections
	existingDocExtras := make(map[string]map[string]string) // slug → {colHeader → value}
	var extraHeaders []string
	var preservedSections []markdownSection

	if existing, err := os.ReadFile(fullPath); err == nil {
		sections := parseMarkdownSections(string(existing))
		for _, s := range sections {
			headerName := strings.TrimSpace(strings.TrimPrefix(s.Header, "##"))
			if strings.EqualFold(headerName, "Documents") {
				// Parse table to extract extra columns
				lines := strings.Split(s.Body, "\n")
				var tableHeaders []string
				for _, line := range lines {
					line = strings.TrimSpace(line)
					if line == "" || strings.HasPrefix(line, "<!--") || line == "_No documents synced yet._" {
						continue
					}
					if strings.Contains(line, "---") && strings.HasPrefix(line, "|") {
						continue
					}
					if strings.HasPrefix(line, "|") {
						if len(tableHeaders) == 0 {
							cells := strings.Split(strings.Trim(line, "|"), "|")
							for _, c := range cells {
								tableHeaders = append(tableHeaders, strings.TrimSpace(c))
							}
							for _, h := range tableHeaders {
								if h != "Title" && h != "Author" && h != "Last Updated" && h != "" {
									extraHeaders = append(extraHeaders, h)
								}
							}
						} else {
							// Extract slug from link "[title](./subDir/slug.md)"
							cells := strings.Split(strings.Trim(line, "|"), "|")
							var slug string
							extras := make(map[string]string)
							for i, cell := range cells {
								cell = strings.TrimSpace(cell)
								if i == 0 {
									if start := strings.Index(cell, "](./"+subDir+"/"); start > 0 {
										rest := cell[start+len("](./"+subDir+"/"):]
										if end := strings.Index(rest, ".md)"); end > 0 {
											slug = rest[:end]
										}
									}
								} else if i < len(tableHeaders) {
									header := strings.TrimSpace(tableHeaders[i])
									if header != "Author" && header != "Last Updated" {
										extras[header] = cell
									}
								}
							}
							if slug != "" && len(extras) > 0 {
								existingDocExtras[slug] = extras
							}
						}
					}
				}
			} else if s.Header != "" {
				preservedSections = append(preservedSections, s)
			}
		}
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("# %s\n\n", sourceDisplayName(source)))
	switch source {
	case "google":
		sb.WriteString("Documents synced from Google Workspace.\n\n")
	case "notion":
		sb.WriteString("Pages synced from Notion.\n\n")
	}

	// Write preserved sections before Documents
	for _, s := range preservedSections {
		sb.WriteString(s.Header + "\n\n")
		body := strings.TrimSpace(s.Body)
		if body != "" {
			sb.WriteString(body + "\n\n")
		}
	}

	sb.WriteString("## Documents\n\n")

	if len(docs) == 0 {
		sb.WriteString("_No documents synced yet._\n")
	} else {
		headerRow := "| Title | Author | Last Updated |"
		sepRow := "|-------|--------|-------------|"
		for _, h := range extraHeaders {
			headerRow += fmt.Sprintf(" %s |", h)
			sepRow += strings.Repeat("-", len(h)+2) + "|"
		}
		sb.WriteString(headerRow + "\n")
		sb.WriteString(sepRow + "\n")

		for _, doc := range docs {
			title := doc.Title
			if title == "" {
				title = doc.Slug
			}
			row := fmt.Sprintf("| [%s](./%s/%s.md) | %s | %s |",
				title, subDir, doc.Slug, doc.Author, doc.UpdatedAt)
			if extras, ok := existingDocExtras[doc.Slug]; ok {
				for _, h := range extraHeaders {
					row += fmt.Sprintf(" %s |", extras[h])
				}
			} else {
				for range extraHeaders {
					row += " |"
				}
			}
			sb.WriteString(row + "\n")
		}
	}

	sb.WriteString(fmt.Sprintf("\n<!-- Auto-generated by kb-gen at %s -->\n",
		time.Now().UTC().Format(time.RFC3339)))

	content := sb.String()

	if dryRun {
		fmt.Printf("[dry-run] Would write index: %s (%d bytes)\n", relPath, len(content))
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}

	logger.Info("wrote doc source index", "path", relPath, "docs", len(docs))
	return writeFile(fullPath, content)
}

// --- Frontmatter Parsing ---

// parseDigestFrontmatter extracts metadata from a daily digest markdown file.
func parseDigestFrontmatter(path string) digestInfo {
	fm := readFrontmatterFields(path)
	return digestInfo{
		Date:         stripQuotes(fm["date"]),
		Participants: parseYAMLInlineList(fm["participants"]),
		Keywords:     parseYAMLInlineList(fm["keywords"]),
		Topics:       parseYAMLInlineList(fm["topics"]),
	}
}

// parseDocFrontmatter extracts metadata from a google/notion document markdown file.
func parseDocFrontmatter(path string) docInfo {
	fm := readFrontmatterFields(path)
	updatedAt := stripQuotes(fm["updated_at"])
	if updatedAt == "" {
		updatedAt = stripQuotes(fm["date"])
	}
	return docInfo{
		Title:     stripQuotes(fm["title"]),
		Author:    stripQuotes(fm["author"]),
		URL:       stripQuotes(fm["url"]),
		UpdatedAt: updatedAt,
	}
}

// readFrontmatterFields reads YAML frontmatter from a markdown file
// and returns a map of field name -> raw value string.
func readFrontmatterFields(path string) map[string]string {
	fields := make(map[string]string)

	f, err := os.Open(path)
	if err != nil {
		return fields
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	inFrontmatter := false

	for scanner.Scan() {
		line := scanner.Text()

		if strings.TrimSpace(line) == "---" {
			if !inFrontmatter {
				inFrontmatter = true
				continue
			}
			break // End of frontmatter
		}
		if !inFrontmatter {
			continue
		}

		// Parse "key: value" lines
		colonIdx := strings.Index(line, ":")
		if colonIdx < 0 {
			continue
		}

		key := strings.TrimSpace(line[:colonIdx])
		value := strings.TrimSpace(line[colonIdx+1:])

		fields[key] = value
	}

	return fields
}

// parseYAMLInlineList parses a YAML inline list like "[a, b, c]" or
// "['a', 'b']" into a string slice.
func parseYAMLInlineList(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" || s == "[]" {
		return nil
	}

	// Strip brackets
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")

	parts := strings.Split(s, ",")
	var result []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		p = stripQuotes(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}

// stripQuotes removes surrounding single or double quotes from a string.
func stripQuotes(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// sortedMapKeys returns the sorted keys of a map[string]struct{}.
func sortedMapKeys(m map[string]struct{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// yamlQuotedList wraps each string in quotes for YAML output.
func yamlQuotedList(items []string) []string {
	quoted := make([]string, len(items))
	for i, item := range items {
		escaped := strings.ReplaceAll(item, "\"", "\\\"")
		quoted[i] = fmt.Sprintf("\"%s\"", escaped)
	}
	return quoted
}

// sourceDisplayName returns the display name for a source.
func sourceDisplayName(source string) string {
	switch source {
	case "slack":
		return "Slack"
	case "discord":
		return "Discord"
	case "google":
		return "Google"
	case "notion":
		return "Notion"
	default:
		return source
	}
}
