package kbrepo

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Reader provides hierarchical access to the markdown-based knowledge base repository.
// It uses a two-stage approach:
//  1. Load index files (topics, keywords, people, weekly summaries, READMEs)
//  2. Based on query relevance, load only the specific detail files referenced by indexes
type Reader struct {
	repoDir    string
	logger     *slog.Logger
	mu         sync.Mutex
	lastPull   time.Time
	pullMinGap time.Duration

	indexCache    []fileEntry
	indexCacheAt  time.Time
	indexCacheTTL time.Duration

	urlMapCache   map[string]string
	urlMapCacheAt time.Time
}

// NewReader creates a KB repo reader.
func NewReader(repoDir string, logger *slog.Logger) *Reader {
	return &Reader{
		repoDir:       repoDir,
		logger:        logger.With("component", "kbrepo"),
		pullMinGap:    30 * time.Second,
		indexCacheTTL: 30 * time.Second,
	}
}

// Pull runs git pull on the knowledge repo to get latest changes.
func (r *Reader) Pull() {
	r.mu.Lock()
	defer r.mu.Unlock()

	if time.Since(r.lastPull) < r.pullMinGap {
		return
	}

	// #nosec G204 -- git pull is an intentional maintenance command scoped to the configured repo.
	cmd := exec.Command("git", "-C", r.repoDir, "pull", "--rebase", "origin", "main")
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.logger.Warn("git pull failed", "error", err, "output", string(out))
		// Don't update lastPull on failure — retry sooner
	} else {
		r.logger.Debug("git pull complete", "output", strings.TrimSpace(string(out)))
		r.lastPull = time.Now()
		r.indexCache = nil   // invalidate cache on successful pull
		r.urlMapCache = nil
	}
}

// BuildContext constructs a context string for the LLM by:
//  1. Loading all index/overview files (always included)
//  2. Finding detail files relevant to the query via keyword matching and link extraction
//  3. Loading only those detail files
//
// This keeps context small and focused even as the repo grows.
func (r *Reader) BuildContext(query string) (string, error) {
	r.Pull()

	// Stage 1: Load index layer.
	indexes, err := r.loadIndexFiles()
	if err != nil {
		return "", fmt.Errorf("load indexes: %w", err)
	}

	// Stage 2: Find relevant detail files.
	detailFiles := r.findRelevantFiles(query, indexes)

	// Stage 3: Load detail files.
	details, err := r.loadFiles(detailFiles)
	if err != nil {
		return "", fmt.Errorf("load details: %w", err)
	}

	// Build URL map to rewrite internal .md links → actual source URLs.
	urlMap := r.buildURLMap()

	// Build final context — use document titles, never expose internal file paths.
	var b strings.Builder

	b.WriteString("# Knowledge Base Indexes\n\n")
	for _, f := range indexes {
		title := extractTitle(f.content)
		if title == "" {
			title = filepath.Base(f.relPath)
		}
		b.WriteString(fmt.Sprintf("## %s\n", title))
		b.WriteString(f.content)
		b.WriteString("\n\n")
	}

	if len(details) > 0 {
		b.WriteString("# Relevant Detail Documents\n\n")
		for _, f := range details {
			title := extractTitle(f.content)
			if title == "" {
				title = filepath.Base(f.relPath)
			}
			b.WriteString(fmt.Sprintf("## %s\n", title))
			b.WriteString(f.content)
			b.WriteString("\n\n")
		}
	}

	// Rewrite internal .md links to actual Google Drive / Notion URLs.
	return rewriteInternalLinks(b.String(), urlMap), nil
}

type fileEntry struct {
	relPath string
	content string
}

// loadIndexFiles loads all index and overview files from the repo.
// These are always included: indexes/*, source READMEs, channel READMEs.
func (r *Reader) loadIndexFiles() ([]fileEntry, error) {
	if r.indexCache != nil && time.Since(r.indexCacheAt) < r.indexCacheTTL {
		return r.indexCache, nil
	}

	root, err := os.OpenRoot(r.repoDir)
	if err != nil {
		return nil, fmt.Errorf("open repo root: %w", err)
	}
	defer root.Close()

	var entries []fileEntry

	// Index files.
	if err := fs.WalkDir(root.FS(), "indexes", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		data, err := root.ReadFile(path)
		if err != nil {
			return nil
		}
		if content := strings.TrimSpace(string(data)); content != "" {
			entries = append(entries, fileEntry{relPath: path, content: content})
		}
		return nil
	}); err != nil && !errors.Is(err, fs.ErrNotExist) {
		r.logger.Warn("failed to walk indexes", "error", err)
	}

	// Source and channel README files.
	readmePatterns := []string{
		"slack/README.md",
		"google/README.md",
		"notion/README.md",
		"user-provided/README.md",
		"discord/README.md",
		"slack/channels/*/README.md",
		"google/*/README.md",
		"discord/channels/*/README.md",
	}
	for _, pattern := range readmePatterns {
		matches, _ := fs.Glob(root.FS(), pattern)
		for _, path := range matches {
			data, err := root.ReadFile(path)
			if err != nil {
				continue
			}
			if content := strings.TrimSpace(string(data)); content != "" {
				entries = append(entries, fileEntry{relPath: path, content: content})
			}
		}
	}

	r.indexCache = entries
	r.indexCacheAt = time.Now()

	return entries, nil
}

// extractURL extracts the url field from YAML frontmatter.
func extractURL(content string) string {
	if !strings.HasPrefix(content, "---") {
		return ""
	}
	end := strings.Index(content[3:], "---")
	if end < 0 {
		return ""
	}
	frontmatter := content[3 : 3+end]
	for _, line := range strings.Split(frontmatter, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "url:") {
			url := strings.TrimPrefix(line, "url:")
			url = strings.TrimSpace(url)
			url = strings.Trim(url, "\"'")
			return url
		}
	}
	return ""
}

// buildURLMap scans all markdown files and maps relative paths to their source URLs.
// Results are cached for indexCacheTTL duration and invalidated on git pull.
func (r *Reader) buildURLMap() map[string]string {
	if r.urlMapCache != nil && time.Since(r.urlMapCacheAt) < r.indexCacheTTL {
		return r.urlMapCache
	}
	urlMap := make(map[string]string)
	root, err := os.OpenRoot(r.repoDir)
	if err != nil {
		r.logger.Warn("failed to open repo root", "error", err)
		return urlMap
	}
	defer root.Close()

	if err := fs.WalkDir(root.FS(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		if base := filepath.Base(path); base == "README.md" || base == "CLAUDE.md" {
			return nil
		}
		data, err := root.ReadFile(path)
		if err != nil {
			return nil
		}
		if url := extractURL(string(data)); url != "" {
			urlMap[path] = url
		}
		return nil
	}); err != nil {
		r.logger.Warn("failed to build url map", "error", err)
	}
	r.urlMapCache = urlMap
	r.urlMapCacheAt = time.Now()
	return urlMap
}

// internalLinkPattern matches markdown links to .md files: [text](path.md)
var internalLinkPattern = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+\.md)\)`)

// rewriteInternalLinks replaces [text](internal-path.md) with [text](actual-url).
func rewriteInternalLinks(text string, urlMap map[string]string) string {
	return internalLinkPattern.ReplaceAllStringFunc(text, func(match string) string {
		sub := internalLinkPattern.FindStringSubmatch(match)
		if len(sub) < 3 {
			return match
		}
		linkText := sub[1]
		linkPath := sub[2]

		// Try exact match, then suffix match.
		for relPath, url := range urlMap {
			if relPath == linkPath || strings.HasSuffix(relPath, "/"+linkPath) {
				return fmt.Sprintf("[%s](%s)", linkText, url)
			}
		}
		return match
	})
}

// extractTitle extracts a document title from YAML frontmatter or first heading.
func extractTitle(content string) string {
	// Try YAML frontmatter title field.
	if strings.HasPrefix(content, "---") {
		end := strings.Index(content[3:], "---")
		if end > 0 {
			frontmatter := content[3 : 3+end]
			for _, line := range strings.Split(frontmatter, "\n") {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "title:") {
					title := strings.TrimPrefix(line, "title:")
					title = strings.TrimSpace(title)
					title = strings.Trim(title, "\"'")
					return title
				}
			}
		}
	}
	// Try first heading.
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, "# ") {
			return strings.TrimPrefix(line, "# ")
		}
	}
	return ""
}

// linkPattern matches markdown links like [text](../path/to/file.md)
var linkPattern = regexp.MustCompile(`\]\(([^)]+\.md)\)`)

// findRelevantFiles determines which detail files to load based on the query.
// It scores sections of index files by IDF-weighted keyword match with heading boosts,
// then extracts file references from the highest-scoring sections.
func (r *Reader) findRelevantFiles(query string, indexes []fileEntry) []string {
	keywords := tokenize(query)
	if len(keywords) == 0 {
		return nil
	}

	// Compute IDF: count how many sections each keyword appears in.
	allSections := []string{}
	for _, idx := range indexes {
		allSections = append(allSections, splitSections(idx.content)...)
	}
	allDetails := r.listDetailFiles()

	keywordDocFreq := make(map[string]int)
	totalSections := len(allSections) + len(allDetails)
	for _, section := range allSections {
		lower := strings.ToLower(section)
		seen := make(map[string]bool)
		for _, kw := range keywords {
			if !seen[kw] && strings.Contains(lower, kw) {
				keywordDocFreq[kw]++
				seen[kw] = true
			}
		}
	}

	// IDF weight: log(totalSections / (1 + docFreq))
	idfWeight := func(kw string) float64 {
		df := keywordDocFreq[kw]
		if df == 0 {
			return 1.0
		}
		w := float64(totalSections) / float64(1+df)
		if w < 1.0 {
			w = 1.0
		}
		// Use simple log approximation
		result := 1.0
		for w > 2.0 {
			result += 1.0
			w /= 2.0
		}
		return result
	}

	fileScores := make(map[string]float64)

	for _, idx := range indexes {
		sections := splitSections(idx.content)
		for _, section := range sections {
			lower := strings.ToLower(section)
			var score float64
			for _, kw := range keywords {
				count := strings.Count(lower, kw)
				if count == 0 {
					continue
				}
				weight := idfWeight(kw)

				// Title/heading boost: check if keyword appears in heading lines
				headingBoost := 1.0
				for _, line := range strings.Split(section, "\n") {
					if strings.HasPrefix(line, "#") && strings.Contains(strings.ToLower(line), kw) {
						headingBoost = 3.0
						break
					}
				}

				score += float64(count) * weight * headingBoost
			}
			if score == 0 {
				continue
			}

			matches := linkPattern.FindAllStringSubmatch(section, -1)
			for _, m := range matches {
				ref := m[1]
				resolved := resolveRef(idx.relPath, ref)
				if resolved != "" {
					fileScores[resolved] += score
				}
			}
		}
	}

	// Extract date patterns from query for date-based file matching.
	// Converts Korean month references like "2월" → "2026-02", "3월" → "2026-03".
	datePatterns := extractDatePatterns(query)

	// Direct keyword matching on detail file paths
	for _, path := range allDetails {
		lower := strings.ToLower(path)
		for _, kw := range keywords {
			if strings.Contains(lower, kw) {
				fileScores[path] += 2.0 * idfWeight(kw)
			}
		}
		// Date-based matching: boost files whose path contains the date pattern
		for _, dp := range datePatterns {
			if strings.Contains(lower, dp) {
				fileScores[path] += 3.0
			}
		}
	}

	// Apply recency boost: files with more recent dates in their path get a bonus.
	now := time.Now()
	dateRe := regexp.MustCompile(`(\d{4}-\d{2}-\d{2})`)
	for path, score := range fileScores {
		if m := dateRe.FindString(path); m != "" {
			if fileDate, err := time.Parse("2006-01-02", m); err == nil {
				daysSince := now.Sub(fileDate).Hours() / 24
				switch {
				case daysSince <= 7:
					fileScores[path] = score * 3.0
				case daysSince <= 30:
					fileScores[path] = score * 2.0
				case daysSince <= 90:
					fileScores[path] = score * 1.5
				}
			}
		}
	}

	// Sort by score and take top results
	type scored struct {
		path  string
		score float64
	}
	var ranked []scored
	for path, score := range fileScores {
		ranked = append(ranked, scored{path, score})
	}
	for i := 0; i < len(ranked); i++ {
		for j := i + 1; j < len(ranked); j++ {
			if ranked[j].score > ranked[i].score {
				ranked[i], ranked[j] = ranked[j], ranked[i]
			}
		}
	}

	maxFiles := 15
	if len(ranked) < maxFiles {
		maxFiles = len(ranked)
	}

	var result []string
	for i := 0; i < maxFiles; i++ {
		result = append(result, ranked[i].path)
	}

	return result
}

// listDetailFiles lists all non-index, non-README markdown files.
func (r *Reader) listDetailFiles() []string {
	root, err := os.OpenRoot(r.repoDir)
	if err != nil {
		r.logger.Warn("failed to open repo root", "error", err)
		return nil
	}
	defer root.Close()

	var files []string
	if err := fs.WalkDir(root.FS(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			base := filepath.Base(path)
			if strings.HasPrefix(base, ".") || base == "_meta" || base == "indexes" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		// Skip root-level meta files and README files.
		switch filepath.Base(path) {
		case "CLAUDE.md", "AGENTS.md", "README.md":
			return nil
		}
		files = append(files, path)
		return nil
	}); err != nil {
		r.logger.Warn("failed to list detail files", "error", err)
	}
	return files
}

// loadFiles reads the specified files from the repo.
func (r *Reader) loadFiles(relPaths []string) ([]fileEntry, error) {
	root, err := os.OpenRoot(r.repoDir)
	if err != nil {
		return nil, fmt.Errorf("open repo root: %w", err)
	}
	defer root.Close()

	var entries []fileEntry
	seen := make(map[string]bool)

	for _, rel := range relPaths {
		if seen[rel] {
			continue
		}
		seen[rel] = true

		data, err := root.ReadFile(rel)
		if err != nil {
			r.logger.Debug("skipping missing kb file", "path", rel, "error", err)
			continue
		}
		if content := strings.TrimSpace(string(data)); content != "" {
			entries = append(entries, fileEntry{relPath: rel, content: content})
		}
	}
	return entries, nil
}

// resolveRef resolves a relative markdown link against the index file's path.
// e.g., indexPath="indexes/topics.md", ref="../slack/channels/foo/bar.md"
// → "slack/channels/foo/bar.md"
func resolveRef(indexPath, ref string) string {
	dir := filepath.Dir(indexPath)
	joined := filepath.Join(dir, ref)
	cleaned := filepath.Clean(joined)
	// Ensure it doesn't escape the repo.
	if strings.HasPrefix(cleaned, "..") {
		return ""
	}
	return cleaned
}

// splitSections splits markdown content by ## headers.
func splitSections(content string) []string {
	lines := strings.Split(content, "\n")
	var sections []string
	var current strings.Builder

	for _, line := range lines {
		if strings.HasPrefix(line, "## ") && current.Len() > 0 {
			sections = append(sections, current.String())
			current.Reset()
		}
		current.WriteString(line)
		current.WriteByte('\n')
	}
	if current.Len() > 0 {
		sections = append(sections, current.String())
	}
	return sections
}

// SaveFact writes a new fact to the KB repo and commits it.
// Facts from user messages are marked as unconfirmed until verified by an official source.
func (r *Reader) SaveFact(topic, content, author string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Create a slug from the topic.
	slug := slugify(topic)
	if slug == "" {
		slug = "untitled"
	}
	date := time.Now().Format("2006-01-02")
	relPath := filepath.Join("user-provided", date+"-"+slug+".md")
	fullPath := filepath.Join(r.repoDir, relPath)

	// Ensure directory exists.
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o750); err != nil {
		return fmt.Errorf("create dir: %w", err)
	}

	// Build markdown with frontmatter.
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString(fmt.Sprintf("title: %q\n", topic))
	b.WriteString(fmt.Sprintf("date: %s\n", date))
	b.WriteString("source: user-provided\n")
	b.WriteString("confirmed: false\n")
	b.WriteString(fmt.Sprintf("provided_by: %q\n", author))
	b.WriteString("---\n\n")
	b.WriteString(content)
	b.WriteString("\n")

	// #nosec G304 -- fullPath is derived from a slugified topic beneath repoDir.
	if err := os.WriteFile(fullPath, []byte(b.String()), 0o600); err != nil {
		return fmt.Errorf("write file: %w", err)
	}

	// Update user-provided/README.md index.
	readmePath := filepath.Join(r.repoDir, "user-provided", "README.md")
	r.updateUserProvidedReadme(readmePath, topic, relPath, content)
	r.updateUserProvidedIndex(topic, relPath, content, author)

	// Git add, commit, push.
	cmds := [][]string{
		{"git", "-C", r.repoDir, "add", relPath},
		{"git", "-C", r.repoDir, "add", "user-provided/README.md"},
		{"git", "-C", r.repoDir, "add", "indexes/user-provided.md"},
		{"git", "-C", r.repoDir, "commit", "-m", fmt.Sprintf("fact(user): add unconfirmed fact — %s (by %s)", topic, author)},
		{"git", "-C", r.repoDir, "push"},
	}
	for _, args := range cmds {
		// #nosec G204 -- git commands and arguments are intentionally constructed from fixed subcommands plus sanitized relative paths.
		cmd := exec.Command(args[0], args[1:]...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			r.logger.Warn("git command failed", "cmd", args, "error", err, "output", string(out))
			return fmt.Errorf("git %s: %w", args[1], err)
		}
	}

	r.logger.Info("saved user-provided fact", "path", relPath, "topic", topic, "author", author)
	return nil
}

// updateUserProvidedReadme appends an entry to user-provided/README.md.
// Creates the file with a header if it doesn't exist.
// Includes a brief excerpt from the content for better keyword matching.
func (r *Reader) updateUserProvidedReadme(readmePath, topic, relPath, content string) {
	const header = "---\ntitle: \"User-Provided Knowledge\"\n---\n# User-Provided Knowledge\n\nFacts and information provided by team members via chat.\n\n"

	// Create file with header if it doesn't exist.
	if _, err := os.Stat(readmePath); os.IsNotExist(err) {
		// #nosec G304 -- readmePath stays within repoDir/user-provided.
		if err := os.WriteFile(readmePath, []byte(header), 0o600); err != nil {
			r.logger.Warn("failed to create user-provided README", "error", err)
			return
		}
	}

	// Append entry with excerpt.
	// #nosec G304 -- readmePath stays within repoDir/user-provided.
	f, err := os.OpenFile(readmePath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		r.logger.Warn("failed to open user-provided README", "error", err)
		return
	}
	defer f.Close()

	// Extract first meaningful line as excerpt (up to 100 chars).
	excerpt := extractExcerpt(content, 100)
	entry := fmt.Sprintf("- [%s](%s)", sanitizeMarkdownLinkText(topic), filepath.Base(relPath))
	if excerpt != "" {
		entry += fmt.Sprintf(" — %s", excerpt)
	}
	entry += "\n"
	if _, err := f.WriteString(entry); err != nil {
		r.logger.Warn("failed to append to user-provided README", "error", err)
	}
}

// extractExcerpt returns the first meaningful line of content, trimmed to maxLen.
func extractExcerpt(content string, maxLen int) string {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "---") || strings.HasPrefix(line, "#") {
			continue
		}
		if len(line) > maxLen {
			// Try to break at a word boundary.
			if idx := strings.LastIndex(line[:maxLen], " "); idx > maxLen/2 {
				return line[:idx] + "…"
			}
			return line[:maxLen] + "…"
		}
		return line
	}
	return ""
}

// updateUserProvidedIndex maintains indexes/user-provided.md for cross-referencing.
// This ensures user-provided facts appear in the primary index search path.
func (r *Reader) updateUserProvidedIndex(topic, relPath, content, author string) {
	indexDir := filepath.Join(r.repoDir, "indexes")
	if err := os.MkdirAll(indexDir, 0o750); err != nil {
		r.logger.Warn("failed to create indexes dir", "error", err)
		return
	}

	indexPath := filepath.Join(indexDir, "user-provided.md")
	const header = "---\ntitle: \"User-Provided Facts Index\"\n---\n# User-Provided Facts\n\nFacts and knowledge contributed by team members.\n\n"

	if _, err := os.Stat(indexPath); os.IsNotExist(err) {
		// #nosec G304 -- indexPath stays within repoDir/indexes.
		if err := os.WriteFile(indexPath, []byte(header), 0o600); err != nil {
			r.logger.Warn("failed to create user-provided index", "error", err)
			return
		}
	}

	// #nosec G304 -- indexPath stays within repoDir/indexes.
	f, err := os.OpenFile(indexPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		r.logger.Warn("failed to open user-provided index", "error", err)
		return
	}
	defer f.Close()

	// Write a section with topic, keywords from content, and file reference.
	excerpt := extractExcerpt(content, 200)
	date := time.Now().Format("2006-01-02")
	entry := fmt.Sprintf("## %s\n- Date: %s | By: %s\n- %s\n- Source: [%s](../%s)\n\n", sanitizeMarkdownLinkText(topic), date, author, excerpt, sanitizeMarkdownLinkText(topic), relPath)
	if _, err := f.WriteString(entry); err != nil {
		r.logger.Warn("failed to append to user-provided index", "error", err)
	}
}

// sanitizeMarkdownLinkText strips characters that break markdown link syntax: [ ] < >
func sanitizeMarkdownLinkText(s string) string {
	r := strings.NewReplacer("[", "", "]", "", "<", "", ">", "")
	return r.Replace(s)
}

// slugify converts a string to a URL/filesystem-safe slug.
// Uses the same normalization logic as kb.NormalizeChannel: lowercase,
// underscores/spaces → hyphens, preserves unicode letters, collapses hyphens.
func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	prevHyphen := false
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			prevHyphen = false
		} else if r == ' ' || r == '-' || r == '_' {
			if !prevHyphen && b.Len() > 0 {
				b.WriteByte('-')
				prevHyphen = true
			}
		}
	}
	slug := strings.Trim(b.String(), "-")
	if len(slug) > 80 {
		slug = slug[:80]
	}
	return slug
}

// synonyms maps common abbreviations and terms to their expansions.
var synonyms = map[string][]string{
	// Source type mappings (Korean ↔ English)
	"슬랙":      {"slack", "channels"},
	"slack":   {"슬랙", "channels"},
	"구글":      {"google", "docs"},
	"google":  {"구글", "docs"},
	"노션":      {"notion", "pages"},
	"notion":  {"노션", "pages"},
	"디스코드":    {"discord", "channels"},
	"discord": {"디스코드", "channels"},
	// Content type mappings
	"대화":   {"channels", "messages", "slack"},
	"대화내용": {"channels", "messages", "slack"},
	"메시지":  {"messages", "slack", "channels"},
	"문서":   {"docs", "google", "pages", "notion"},
	"회의":   {"meeting", "회의록", "주간회의"},
	"회의록":  {"meeting", "minutes", "주간회의"},
	// Tech abbreviations
	"ml":     {"machine", "learning", "머신러닝"},
	"ai":     {"artificial", "intelligence", "인공지능"},
	"dl":     {"deep", "learning", "딥러닝"},
	"llm":    {"large", "language", "model"},
	"api":    {"interface"},
	"db":     {"database", "데이터베이스"},
	"ui":     {"user", "interface", "사용자"},
	"ux":     {"user", "experience"},
	"devops": {"deploy", "infrastructure", "배포"},
	"cicd":   {"ci", "cd", "continuous", "integration", "delivery"},
	"k8s":    {"kubernetes"},
	"fe":     {"frontend", "프론트엔드"},
	"be":     {"backend", "백엔드"},
}

// tokenize splits a query into lowercase keyword tokens with synonym expansion.
func tokenize(s string) []string {
	s = strings.ToLower(s)
	var tokens []string
	var current strings.Builder

	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			current.WriteRune(r)
		} else {
			if current.Len() > 0 {
				tokens = append(tokens, current.String())
				current.Reset()
			}
		}
	}
	if current.Len() > 0 {
		tokens = append(tokens, current.String())
	}

	// Filter out very short Latin-only tokens
	var filtered []string
	for _, t := range tokens {
		runes := []rune(t)
		if len(runes) == 1 && runes[0] < 0x1100 {
			continue
		}
		filtered = append(filtered, t)
	}

	// Expand synonyms
	expanded := make([]string, 0, len(filtered)*2)
	seen := make(map[string]bool)
	for _, t := range filtered {
		if !seen[t] {
			expanded = append(expanded, t)
			seen[t] = true
		}
		if syns, ok := synonyms[t]; ok {
			for _, syn := range syns {
				if !seen[syn] {
					expanded = append(expanded, syn)
					seen[syn] = true
				}
			}
		}
	}

	return expanded
}

// koreanMonthPattern matches Korean month references like "1월", "2월", "12월".
var koreanMonthPattern = regexp.MustCompile(`(\d{1,2})월`)

// extractDatePatterns extracts date path patterns from a query string.
// "2월" → ["2026-02"], "2025년 3월" → ["2025-03"], "3월" → ["2026-03"].
func extractDatePatterns(query string) []string {
	var patterns []string

	// Extract year if present (e.g., "2025년")
	yearPattern := regexp.MustCompile(`(20\d{2})년?`)
	yearMatches := yearPattern.FindStringSubmatch(query)
	year := time.Now().Format("2006")
	if len(yearMatches) >= 2 {
		year = yearMatches[1]
	}

	// Extract months
	now := time.Now()
	currentMonth := int(now.Month())
	monthMatches := koreanMonthPattern.FindAllStringSubmatch(query, -1)
	for _, m := range monthMatches {
		if len(m) >= 2 {
			month, err := strconv.Atoi(m[1])
			if err == nil && month >= 1 && month <= 12 {
				patterns = append(patterns, fmt.Sprintf("%s-%02d", year, month))
				// When no explicit year and the month is in the future while
				// we're early in the year, also try the previous year.
				if len(yearMatches) < 2 && month > currentMonth && currentMonth <= 3 {
					patterns = append(patterns, fmt.Sprintf("%d-%02d", now.Year()-1, month))
				}
			}
		}
	}

	return patterns
}
