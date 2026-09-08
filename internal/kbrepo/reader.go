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
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
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

	cacheMu sync.RWMutex

	indexCache    []fileEntry
	indexCacheAt  time.Time
	indexCacheTTL time.Duration

	urlMapCache   map[string]string
	urlMapCacheAt time.Time

	detailFilesCache   []string
	detailFilesCacheAt time.Time

	detailContentCache   map[string]string
	detailContentCacheAt time.Time

	maxContextBytes int
}

// NewReader creates a KB repo reader.
func NewReader(repoDir string, logger *slog.Logger) *Reader {
	return &Reader{
		repoDir:         repoDir,
		logger:          logger.With("component", "kbrepo"),
		pullMinGap:      30 * time.Second,
		indexCacheTTL:   30 * time.Second,
		maxContextBytes: defaultMaxContextBytes,
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
		r.cacheMu.Lock()
		r.indexCache = nil // invalidate cache on successful pull
		r.detailFilesCache = nil
		r.detailContentCache = nil
		r.urlMapCache = nil
		r.cacheMu.Unlock()
	}
}

// Context size budget. The caller imposes its own cap on the returned string;
// these keep the two layers inside it in the right order of priority.
const (
	// defaultMaxContextBytes is the total size BuildContext aims for.
	defaultMaxContextBytes = 400000

	// detailReservePercent is the share of the budget the query-relevant detail
	// documents may always claim. The index layer is "always included" and
	// grows with every channel added — it passed 200 KB on its own here, which
	// under a plain tail-truncation silently discarded every detail document,
	// the one part selected because it answers this question.
	detailReservePercent = 30

	// maxDetailFileBytes caps a single detail document, so one 500 KB day of a
	// busy channel cannot consume the whole detail reserve.
	maxDetailFileBytes = 24000
)

// renderSection appends a titled document to b, capped at limit bytes, and
// reports how many bytes it wrote.
func renderSection(b *strings.Builder, f fileEntry, limit int) int {
	title := extractTitle(f.content)
	if title == "" {
		title = filepath.Base(f.relPath)
	}
	section := fmt.Sprintf("## %s\n%s\n\n", title, f.content)
	if len(section) > limit {
		section = truncateUTF8(section, limit)
	}
	b.WriteString(section)
	return len(section)
}

// truncateUTF8 cuts s to at most limit bytes without splitting a rune.
func truncateUTF8(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	if limit <= 0 {
		return ""
	}
	for limit > 0 && !utf8.RuneStart(s[limit]) {
		limit--
	}
	return s[:limit]
}

// BuildContext constructs a context string for the LLM by:
//  1. Loading all index/overview files (always included)
//  2. Finding detail files relevant to the query via keyword matching and link extraction
//  3. Loading only those detail files
//
// Both layers are fitted into maxContextBytes with the detail layer given a
// reserved share, so the documents selected for this query survive even when
// the index layer alone would fill the budget.
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

	// Render the detail layer first to learn its true size, then give the index
	// layer whatever the budget has left. Output order is unchanged.
	var detailPart strings.Builder
	if len(details) > 0 {
		indexSize := 0
		for _, f := range indexes {
			indexSize += len(f.content)
		}
		reserve := r.maxContextBytes * detailReservePercent / 100
		detailBudget := r.maxContextBytes - indexSize
		if detailBudget < reserve {
			detailBudget = reserve
		}

		detailPart.WriteString("# Relevant Detail Documents\n\n")
		used := detailPart.Len()
		for _, f := range details {
			remaining := detailBudget - used
			if remaining <= 0 {
				break
			}
			limit := maxDetailFileBytes
			if remaining < limit {
				limit = remaining
			}
			used += renderSection(&detailPart, f, limit)
		}
	}

	indexBudget := r.maxContextBytes - detailPart.Len()

	// Build final context — use document titles, never expose internal file paths.
	var b strings.Builder
	b.WriteString("# Knowledge Base Indexes\n\n")
	used := b.Len()
	for _, f := range indexes {
		remaining := indexBudget - used
		if remaining <= 0 {
			break
		}
		used += renderSection(&b, f, remaining)
	}
	b.WriteString(detailPart.String())

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
	r.cacheMu.RLock()
	if r.indexCache != nil && time.Since(r.indexCacheAt) < r.indexCacheTTL {
		cache := r.indexCache
		r.cacheMu.RUnlock()
		return cache, nil
	}
	r.cacheMu.RUnlock()

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

	r.cacheMu.Lock()
	r.indexCache = entries
	r.indexCacheAt = time.Now()
	r.cacheMu.Unlock()

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
	r.cacheMu.RLock()
	if r.urlMapCache != nil && time.Since(r.urlMapCacheAt) < r.indexCacheTTL {
		cache := r.urlMapCache
		r.cacheMu.RUnlock()
		return cache
	}
	r.cacheMu.RUnlock()
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
	r.cacheMu.Lock()
	r.urlMapCache = urlMap
	r.urlMapCacheAt = time.Now()
	r.cacheMu.Unlock()
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

const (
	// maxContextFiles caps how many detail files BuildContext pulls into the
	// prompt.
	maxContextFiles = 15

	// contentMatchCap bounds how many occurrences of one term inside a single
	// detail file can contribute to its score, so a long transcript that
	// repeats a common word cannot outrank a short document that actually
	// answers the query.
	contentMatchCap = 8
)

// scoreFiles scores every detail file against the query. It scores index-file
// sections and detail-file bodies by IDF-weighted keyword match with heading
// boosts, and credits a detail file both for its own matches and for matches in
// the index sections that link to it.
func (r *Reader) scoreFiles(query string, indexes []fileEntry) map[string]float64 {
	terms := queryTerms(query)
	if len(terms) == 0 {
		return nil
	}

	// Compute IDF over both index sections and detail-file bodies, so a term's
	// weight reflects how rare it is across everything that gets searched.
	allSections := []string{}
	for _, idx := range indexes {
		allSections = append(allSections, splitSections(idx.content)...)
	}
	detailContents := r.detailFileContents()
	allDetails := r.listDetailFiles()

	keywordDocFreq := make(map[string]int)
	totalSections := len(allSections) + len(allDetails)
	countDocFreq := func(lower string) {
		seen := make(map[string]bool)
		for _, t := range terms {
			if !seen[t.text] && strings.Contains(lower, t.text) {
				keywordDocFreq[t.text]++
				seen[t.text] = true
			}
		}
	}
	for _, section := range allSections {
		countDocFreq(strings.ToLower(section))
	}
	for _, content := range detailContents {
		countDocFreq(content)
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
			for _, t := range terms {
				count := strings.Count(lower, t.text)
				if count == 0 {
					continue
				}
				weight := idfWeight(t.text)

				// Title/heading boost: check if keyword appears in heading lines
				headingBoost := 1.0
				for _, line := range strings.Split(section, "\n") {
					if strings.HasPrefix(line, "#") && strings.Contains(strings.ToLower(line), t.text) {
						headingBoost = 3.0
						break
					}
				}

				score += float64(count) * weight * headingBoost * t.weight
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
		for _, t := range terms {
			if strings.Contains(lower, t.text) {
				fileScores[path] += 2.0 * idfWeight(t.text) * t.weight
			}
		}
		// Date-based matching: boost files whose path contains the date pattern
		for _, dp := range datePatterns {
			if strings.Contains(lower, dp) {
				fileScores[path] += 3.0
			}
		}
	}

	// Direct keyword matching on detail file bodies. The index layer only ever
	// links a small fraction of the repo, so without this a fact that lives in
	// the body of a document nothing links to is unreachable no matter how
	// exactly the query names it.
	for path, content := range detailContents {
		var score float64
		for _, t := range terms {
			count := strings.Count(content, t.text)
			if count == 0 {
				continue
			}
			if count > contentMatchCap {
				count = contentMatchCap
			}
			score += float64(count) * idfWeight(t.text) * t.weight
		}
		if score > 0 {
			fileScores[path] += score
		}
	}

	// Apply recency boost: files with more recent dates in their path get a bonus.
	now := time.Now()
	for path, score := range fileScores {
		if m := datePathRe.FindString(path); m != "" {
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

	return fileScores
}

// findRelevantFiles ranks the scored files and returns the top maxContextFiles.
func (r *Reader) findRelevantFiles(query string, indexes []fileEntry) []string {
	fileScores := r.scoreFiles(query, indexes)

	// Sort by score descending and take top results
	type scored struct {
		path  string
		score float64
	}
	var ranked []scored
	for path, score := range fileScores {
		ranked = append(ranked, scored{path, score})
	}
	slices.SortFunc(ranked, func(a, b scored) int {
		if a.score > b.score {
			return -1
		}
		if a.score < b.score {
			return 1
		}
		return 0
	})

	maxFiles := maxContextFiles
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
	r.cacheMu.RLock()
	if r.detailFilesCache != nil && time.Since(r.detailFilesCacheAt) < r.indexCacheTTL {
		cache := r.detailFilesCache
		r.cacheMu.RUnlock()
		return cache
	}
	r.cacheMu.RUnlock()

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
			// The walk root is ".", whose Base is "." — without this guard the
			// hidden-directory check below skips the entire repo and the walk
			// yields nothing.
			if path == "." {
				return nil
			}
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
	r.cacheMu.Lock()
	r.detailFilesCache = files
	r.detailFilesCacheAt = time.Now()
	r.cacheMu.Unlock()
	return files
}

// detailFileContents returns the lowercased body of every detail file, keyed by
// relative path. Results are cached for indexCacheTTL and invalidated on git
// pull, so a query scans memory rather than the filesystem.
func (r *Reader) detailFileContents() map[string]string {
	r.cacheMu.RLock()
	if r.detailContentCache != nil && time.Since(r.detailContentCacheAt) < r.indexCacheTTL {
		cache := r.detailContentCache
		r.cacheMu.RUnlock()
		return cache
	}
	r.cacheMu.RUnlock()

	paths := r.listDetailFiles()

	root, err := os.OpenRoot(r.repoDir)
	if err != nil {
		r.logger.Warn("failed to open repo root", "error", err)
		return map[string]string{}
	}
	defer root.Close()

	contents := make(map[string]string, len(paths))
	for _, path := range paths {
		data, err := root.ReadFile(path)
		if err != nil {
			continue
		}
		contents[path] = strings.ToLower(string(data))
	}

	r.cacheMu.Lock()
	r.detailContentCache = contents
	r.detailContentCacheAt = time.Now()
	r.cacheMu.Unlock()
	return contents
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
		{"git", "-C", r.repoDir, "commit", "-m", fmt.Sprintf("fact(user): add unconfirmed fact — %s (by %s)", sanitizeCommitString(topic), sanitizeCommitString(author))},
		{"git", "-C", r.repoDir, "push"},
	}
	for _, args := range cmds {
		// #nosec G204 -- git commands and arguments are intentionally constructed from fixed subcommands plus sanitized relative paths.
		cmd := exec.Command(args[0], args[1:]...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			r.logger.Warn("git command failed", "cmd", args, "error", err, "output", string(out))
			// Clean up staged changes on commit/push failure to avoid orphaned index state.
			if args[1] == "commit" || args[1] == "push" {
				resetCmd := exec.Command("git", "-C", r.repoDir, "reset", "HEAD", relPath, "user-provided/README.md", "indexes/user-provided.md")
				if resetOut, resetErr := resetCmd.CombinedOutput(); resetErr != nil {
					r.logger.Warn("git reset failed", "error", resetErr, "output", string(resetOut))
				}
			}
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

// sanitizeCommitString strips newlines and carriage returns from strings
// used in git commit messages to prevent injection of arbitrary commit metadata.
func sanitizeCommitString(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", "")
	return s
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
	runes := []rune(slug)
	if len(runes) > 80 {
		slug = string(runes[:80])
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

// queryTerm is one search term derived from the user's query, together with how
// much a match on it counts. Surface tokens carry full weight; the sub-token
// fragments derived from them count for less.
type queryTerm struct {
	text   string
	weight float64
}

const (
	// Hangul sub-token fragments are cut at these lengths. Korean glues
	// particles onto nouns and compounds nouns without spaces, so a query
	// eojeol ("대표전화번호가") shares no whole token with the text that
	// answers it ("법인 ... 대표번호를") even though they are about the same
	// thing. Matching the query's fragments bridges both, in the one direction
	// that is safe: substrings of the query are still found by Contains inside
	// longer words in the documents.
	minHangulGram = 2
	maxHangulGram = 4

	// maxQueryTerms bounds the per-query scan: every term is swept across every
	// index section and detail body, so the term count sets the cost.
	maxQueryTerms = 48
)

// hasHangul reports whether s contains any Hangul syllable, jamo, or
// compatibility jamo.
func hasHangul(s string) bool {
	for _, r := range s {
		switch {
		case r >= 0xAC00 && r <= 0xD7A3, // syllables
			r >= 0x1100 && r <= 0x11FF, // jamo
			r >= 0x3130 && r <= 0x318F: // compatibility jamo
			return true
		}
	}
	return false
}

// hangulGrams returns the contiguous character n-grams of tok, longest first,
// excluding tok itself.
func hangulGrams(tok string) []string {
	runes := []rune(tok)
	var grams []string
	for n := maxHangulGram; n >= minHangulGram; n-- {
		if n >= len(runes) {
			continue
		}
		for i := 0; i+n <= len(runes); i++ {
			grams = append(grams, string(runes[i:i+n]))
		}
	}
	return grams
}

// queryTerms expands a query into weighted search terms: the tokens themselves
// at full weight, plus Hangul sub-token fragments at a reduced weight that rises
// with fragment length.
func queryTerms(query string) []queryTerm {
	tokens := tokenize(query)

	terms := make([]queryTerm, 0, len(tokens))
	seen := make(map[string]bool, len(tokens))
	for _, tok := range tokens {
		if seen[tok] {
			continue
		}
		seen[tok] = true
		terms = append(terms, queryTerm{text: tok, weight: 1.0})
	}

	for _, tok := range tokens {
		if !hasHangul(tok) {
			continue
		}
		for _, gram := range hangulGrams(tok) {
			if seen[gram] {
				continue
			}
			if len(terms) >= maxQueryTerms {
				return terms
			}
			seen[gram] = true
			// 2-gram → 0.15, 3-gram → 0.30, 4-gram → 0.45.
			terms = append(terms, queryTerm{text: gram, weight: 0.15 * float64(len([]rune(gram))-1)})
		}
	}

	return terms
}

// koreanMonthPattern matches Korean month references like "1월", "2월", "12월".
var koreanMonthPattern = regexp.MustCompile(`(\d{1,2})월`)
var datePathRe = regexp.MustCompile(`(\d{4}-\d{2}-\d{2})`)
var yearPattern = regexp.MustCompile(`(20\d{2})년?`)

// extractDatePatterns extracts date path patterns from a query string.
// "2월" → ["2026-02"], "2025년 3월" → ["2025-03"], "3월" → ["2026-03"].
func extractDatePatterns(query string) []string {
	var patterns []string

	// Extract year if present (e.g., "2025년")
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
