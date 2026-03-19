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

// generateChannelReadme writes a README.md for a single channel.
func generateChannelReadme(kbDir, source string, ch channelSummary, dryRun bool, logger *slog.Logger) error {
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
	relPath := filepath.Join(source, "channels", ch.Name, "README.md")
	fullPath := filepath.Join(kbDir, relPath)

	if dryRun {
		fmt.Printf("[dry-run] Would write index: %s (%d bytes)\n", relPath, len(content))
		return nil
	}

	logger.Info("wrote channel index", "path", relPath)
	return writeFile(fullPath, content)
}

// generateSourceReadme writes the master README.md for a source (e.g., slack/README.md).
func generateSourceReadme(kbDir, source string, channels []channelSummary, dryRun bool, logger *slog.Logger) error {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("# %s\n\n", sourceDisplayName(source)))
	sb.WriteString("Message digests organized by channel and date.\n\n")
	sb.WriteString("## Channels\n\n")

	if len(channels) == 0 {
		sb.WriteString("_No channels synced yet._\n")
	} else {
		sb.WriteString("| Channel | Last Activity | Digests |\n")
		sb.WriteString("|---------|--------------|--------|\n")
		for _, ch := range channels {
			sb.WriteString(fmt.Sprintf("| [#%s](./channels/%s/) | %s | %d |\n",
				ch.Name, ch.Name, ch.LastDate, ch.FileCount))
		}
	}

	sb.WriteString(fmt.Sprintf("\n<!-- Auto-generated by kb-gen at %s -->\n",
		time.Now().UTC().Format(time.RFC3339)))

	content := sb.String()
	relPath := filepath.Join(source, "README.md")
	fullPath := filepath.Join(kbDir, relPath)

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

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("# %s\n\n", sourceDisplayName(source)))
	switch source {
	case "google":
		sb.WriteString("Documents synced from Google Workspace.\n\n")
	case "notion":
		sb.WriteString("Pages synced from Notion.\n\n")
	}
	sb.WriteString("## Documents\n\n")

	if len(docs) == 0 {
		sb.WriteString("_No documents synced yet._\n")
	} else {
		sb.WriteString("| Title | Author | Last Updated |\n")
		sb.WriteString("|-------|--------|-------------|\n")
		for _, doc := range docs {
			title := doc.Title
			if title == "" {
				title = doc.Slug
			}
			sb.WriteString(fmt.Sprintf("| [%s](./%s/%s.md) | %s | %s |\n",
				title, subDir, doc.Slug, doc.Author, doc.UpdatedAt))
		}
	}

	sb.WriteString(fmt.Sprintf("\n<!-- Auto-generated by kb-gen at %s -->\n",
		time.Now().UTC().Format(time.RFC3339)))

	content := sb.String()
	relPath := filepath.Join(source, "README.md")
	fullPath := filepath.Join(kbDir, relPath)

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
