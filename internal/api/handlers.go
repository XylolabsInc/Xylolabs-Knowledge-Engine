package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/xylolabsinc/xylolabs-kb/internal/kb"
)

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	status := "ok"
	checks := map[string]string{}

	// Check database connectivity
	if err := s.store.Ping(); err != nil {
		status = "degraded"
		checks["database"] = "error: " + err.Error()
	} else {
		checks["database"] = "ok"
	}

	// Check KB repo directory if configured
	if s.kbRepoDir != "" {
		if _, err := os.Stat(s.kbRepoDir); err != nil {
			status = "degraded"
			checks["kb_repo"] = "error: " + err.Error()
		} else {
			checks["kb_repo"] = "ok"
		}
	}

	httpStatus := http.StatusOK
	if status == "degraded" {
		httpStatus = http.StatusServiceUnavailable
	}

	writeJSON(w, httpStatus, map[string]any{
		"status": status,
		"time":   time.Now().UTC().Format(time.RFC3339),
		"checks": checks,
	})
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	queryText := q.Get("q")
	if queryText == "" {
		writeError(w, http.StatusBadRequest, "query parameter 'q' is required")
		return
	}
	if len(queryText) > 500 {
		writeError(w, http.StatusBadRequest, "query too long (max 500 characters)")
		return
	}

	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}

	offset, _ := strconv.Atoi(q.Get("offset"))

	query := kb.SearchQuery{
		Query:   queryText,
		Source:  kb.Source(q.Get("source")),
		Channel: q.Get("channel"),
		Author:  q.Get("author"),
		Limit:   limit,
		Offset:  offset,
	}

	if query.Source != "" {
		switch query.Source {
		case kb.SourceSlack, kb.SourceGoogle, kb.SourceNotion, kb.SourceManual, kb.SourceDiscord:
			// valid
		default:
			writeError(w, http.StatusBadRequest, "invalid source: must be slack, google, notion, discord, or manual")
			return
		}
	}

	if from := q.Get("from"); from != "" {
		if t, err := time.Parse(time.RFC3339, from); err == nil {
			query.DateFrom = t
		}
	}
	if to := q.Get("to"); to != "" {
		if t, err := time.Parse(time.RFC3339, to); err == nil {
			query.DateTo = t
		}
	}

	results, err := s.engine.Search(r.Context(), query)
	if err != nil {
		s.logger.Warn("search failed", "query", queryText, "error", err)
		writeError(w, http.StatusInternalServerError, "search failed")
		return
	}

	type resultItem struct {
		ID          string            `json:"id"`
		Source      string            `json:"source"`
		Title       string            `json:"title,omitempty"`
		Snippet     string            `json:"snippet"`
		ContentType string            `json:"content_type"`
		Author      string            `json:"author,omitempty"`
		Channel     string            `json:"channel,omitempty"`
		URL         string            `json:"url,omitempty"`
		Timestamp   time.Time         `json:"timestamp"`
		Score       float64           `json:"score"`
		Metadata    map[string]string `json:"metadata,omitempty"`
	}

	items := make([]resultItem, 0, len(results.Results))
	for _, r := range results.Results {
		items = append(items, resultItem{
			ID:          r.Document.ID,
			Source:      string(r.Document.Source),
			Title:       r.Document.Title,
			Snippet:     r.Snippet,
			ContentType: r.Document.ContentType,
			Author:      r.Document.Author,
			Channel:     r.Document.Channel,
			URL:         r.Document.URL,
			Timestamp:   r.Document.Timestamp,
			Score:       r.Score,
			Metadata:    r.Document.Metadata,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"query":   queryText,
		"results": items,
		"total":   results.Total,
		"limit":   limit,
		"offset":  offset,
	})
}

func (s *Server) handleListDocuments(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 {
		limit = 500
	}
	if limit > 1000 {
		limit = 1000
	}

	offset, _ := strconv.Atoi(q.Get("offset"))

	query := kb.ListDocumentsQuery{
		Source: kb.Source(q.Get("source")),
		Limit:  limit,
		Offset: offset,
	}

	if query.Source != "" {
		switch query.Source {
		case kb.SourceSlack, kb.SourceGoogle, kb.SourceNotion, kb.SourceManual, kb.SourceDiscord:
			// valid
		default:
			writeError(w, http.StatusBadRequest, "invalid source: must be slack, google, notion, discord, or manual")
			return
		}
	}

	if since := q.Get("since"); since != "" {
		if t, err := time.Parse(time.RFC3339, since); err == nil {
			query.Since = t
		} else {
			writeError(w, http.StatusBadRequest, "invalid 'since' parameter: must be RFC3339 format")
			return
		}
	}

	result, err := s.store.ListDocuments(query)
	if err != nil {
		s.logger.Warn("list documents failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list documents")
		return
	}

	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleGetDocument(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "document ID is required")
		return
	}
	if len(id) > 100 {
		writeError(w, http.StatusBadRequest, "invalid document ID")
		return
	}

	doc, err := s.engine.GetDocument(r.Context(), id)
	if err != nil {
		s.logger.Warn("get document failed", "id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to get document")
		return
	}
	if doc == nil {
		writeError(w, http.StatusNotFound, "document not found")
		return
	}

	writeJSON(w, http.StatusOK, doc)
}

func (s *Server) handleGetStats(w http.ResponseWriter, r *http.Request) {
	stats, err := s.engine.GetStats(r.Context())
	if err != nil {
		s.logger.Warn("get stats failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to get stats")
		return
	}

	// Convert to JSON-friendly format
	bySource := make(map[string]int64)
	for k, v := range stats.DocumentsBySource {
		bySource[string(k)] = v
	}
	syncTimes := make(map[string]string)
	for k, v := range stats.LastSyncTimes {
		syncTimes[string(k)] = v.Format(time.RFC3339)
	}

	// Count KB repo markdown files
	var kbFileCount int
	if s.kbRepoDir != "" {
		if err := filepath.WalkDir(s.kbRepoDir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if strings.HasSuffix(d.Name(), ".md") && !strings.HasPrefix(path, filepath.Join(s.kbRepoDir, ".git")) {
				kbFileCount++
			}
			return nil
		}); err != nil {
			s.logger.Warn("failed to count kb repo files", "error", err)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"total_documents":     stats.TotalDocuments,
		"documents_by_source": bySource,
		"documents_by_type":   stats.DocumentsByType,
		"total_attachments":   stats.TotalAttachments,
		"attachment_size":     stats.AttachmentSize,
		"last_sync_times":     syncTimes,
		"kb_file_count":       kbFileCount,
	})
}

func (s *Server) handleListSources(w http.ResponseWriter, r *http.Request) {
	jobs := s.scheduler.Status()

	sources := make([]map[string]any, 0, len(jobs))
	for _, job := range jobs {
		syncState, _ := s.store.GetSyncState(kb.Source(job.Name))
		source := map[string]any{
			"name":      job.Name,
			"interval":  job.Interval,
			"last_run":  job.LastRun,
			"run_count": job.RunCount,
			"err_count": job.ErrCount,
		}
		if job.LastError != "" {
			source["last_error"] = job.LastError
		}
		if syncState != nil {
			source["last_sync"] = syncState.LastSyncAt
			source["cursor"] = syncState.Cursor
		}
		sources = append(sources, source)
	}

	writeJSON(w, http.StatusOK, map[string]any{"sources": sources})
}

func (s *Server) handleTriggerSync(w http.ResponseWriter, r *http.Request) {
	source := r.PathValue("source")
	if source == "" {
		writeError(w, http.StatusBadRequest, "source is required")
		return
	}

	kbSource := kb.Source(source)
	switch kbSource {
	case kb.SourceSlack, kb.SourceGoogle, kb.SourceNotion, kb.SourceManual, kb.SourceDiscord:
		// valid
	default:
		writeError(w, http.StatusBadRequest, "unknown source: "+source)
		return
	}

	// Run sync asynchronously
	s.asyncWg.Add(1)
	go func() {
		defer s.asyncWg.Done()
		syncCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		if err := s.syncManager.SyncSource(syncCtx, kbSource); err != nil {
			s.logger.Warn("manual sync failed", "source", source, "error", err)
		}
	}()

	writeJSON(w, http.StatusAccepted, map[string]string{
		"status": "sync triggered",
		"source": source,
	})
}

func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	if s.jobLister == nil {
		writeJSON(w, http.StatusOK, map[string]any{"jobs": []any{}})
		return
	}

	jobs, err := s.jobLister.ListScheduledJobs()
	if err != nil {
		s.logger.Warn("list jobs failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list jobs")
		return
	}

	type jobItem struct {
		ID        string  `json:"id"`
		Type      string  `json:"type"`
		ChannelID string  `json:"channel_id"`
		Message   string  `json:"message"`
		CronExpr  string  `json:"cron_expr,omitempty"`
		RunAt     *string `json:"run_at,omitempty"`
		NextRun   *string `json:"next_run,omitempty"`
		CreatedBy string  `json:"created_by"`
		CreatedAt string  `json:"created_at"`
	}

	items := make([]jobItem, 0, len(jobs))
	for _, j := range jobs {
		item := jobItem{
			ID:        j.ID,
			Type:      j.Type,
			ChannelID: j.ChannelID,
			Message:   j.Message,
			CronExpr:  j.CronExpr,
			CreatedBy: j.CreatedBy,
			CreatedAt: j.CreatedAt.Format(time.RFC3339),
		}
		if !j.RunAt.IsZero() {
			s := j.RunAt.Format(time.RFC3339)
			item.RunAt = &s
		}
		if !j.NextRun.IsZero() {
			s := j.NextRun.Format(time.RFC3339)
			item.NextRun = &s
		}
		items = append(items, item)
	}

	writeJSON(w, http.StatusOK, map[string]any{"jobs": items})
}

func (s *Server) handleKBTree(w http.ResponseWriter, r *http.Request) {
	if s.kbRepoDir == "" {
		writeJSON(w, http.StatusOK, map[string]any{"tree": []any{}, "error": "KB_REPO_DIR not configured"})
		return
	}

	type fileNode struct {
		Name     string      `json:"name"`
		Path     string      `json:"path"`
		IsDir    bool        `json:"is_dir"`
		Children []*fileNode `json:"children,omitempty"`
		Size     int64       `json:"size,omitempty"`
	}

	root := &fileNode{Name: "/", Path: "", IsDir: true}
	nodeMap := map[string]*fileNode{"": root}

	if err := filepath.WalkDir(s.kbRepoDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(s.kbRepoDir, path)
		if rel == "." {
			return nil
		}

		// Skip symlinks to prevent directory traversal
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}

		// Skip hidden dirs and .git
		base := filepath.Base(rel)
		if strings.HasPrefix(base, ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		// Skip _meta directory
		if base == "_meta" && d.IsDir() {
			return filepath.SkipDir
		}

		// Only include .md files and directories
		if !d.IsDir() && !strings.HasSuffix(base, ".md") {
			return nil
		}

		node := &fileNode{
			Name:  base,
			Path:  rel,
			IsDir: d.IsDir(),
		}

		if !d.IsDir() {
			if info, err := d.Info(); err == nil {
				node.Size = info.Size()
			}
		}

		// Find parent
		parent := filepath.Dir(rel)
		if parent == "." {
			parent = ""
		}
		if parentNode, ok := nodeMap[parent]; ok {
			parentNode.Children = append(parentNode.Children, node)
		}
		if d.IsDir() {
			nodeMap[rel] = node
		}

		return nil
	}); err != nil {
		s.logger.Warn("failed to build kb tree", "error", err)
	}

	writeJSON(w, http.StatusOK, map[string]any{"tree": root.Children})
}

func (s *Server) handleKBFile(w http.ResponseWriter, r *http.Request) {
	if s.kbRepoDir == "" {
		writeError(w, http.StatusNotFound, "KB_REPO_DIR not configured")
		return
	}

	filePath := r.URL.Query().Get("path")
	if filePath == "" {
		writeError(w, http.StatusBadRequest, "path parameter required")
		return
	}
	if len(filePath) > 500 {
		writeError(w, http.StatusBadRequest, "path too long")
		return
	}

	// Security: prevent path traversal
	cleaned := filepath.Clean(filePath)
	if strings.Contains(cleaned, "..") || filepath.IsAbs(cleaned) {
		writeError(w, http.StatusBadRequest, "invalid path")
		return
	}
	if !isAllowedKBRepoPath(cleaned) {
		writeError(w, http.StatusBadRequest, "invalid path")
		return
	}

	root, err := os.OpenRoot(s.kbRepoDir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server configuration error")
		return
	}
	defer root.Close()

	info, err := root.Stat(cleaned)
	if err != nil {
		if os.IsNotExist(err) {
			writeError(w, http.StatusNotFound, "file not found")
		} else {
			writeError(w, http.StatusInternalServerError, "failed to stat file")
		}
		return
	}
	if info.IsDir() {
		writeError(w, http.StatusBadRequest, "path must point to a markdown file")
		return
	}

	data, err := root.ReadFile(cleaned)
	if err != nil {
		if os.IsNotExist(err) {
			writeError(w, http.StatusNotFound, "file not found")
		} else {
			writeError(w, http.StatusInternalServerError, "failed to read file")
		}
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"path":    cleaned,
		"name":    filepath.Base(cleaned),
		"content": string(data),
	})
}

func (s *Server) handleKBDocTree(w http.ResponseWriter, r *http.Request) {
	result, err := s.store.ListDocuments(kb.ListDocumentsQuery{Limit: 1000})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list documents")
		return
	}

	type node struct {
		Name     string  `json:"name"`
		Path     string  `json:"path"`
		IsDir    bool    `json:"is_dir"`
		Children []*node `json:"children,omitempty"`
		Size     int64   `json:"size,omitempty"`
	}

	// Group by source -> channel
	sourceMap := map[string]map[string][]kb.Document{}
	for _, doc := range result.Documents {
		src := string(doc.Source)
		ch := doc.Channel
		if ch == "" {
			ch = doc.ContentType
		}
		if ch == "" {
			ch = "(uncategorized)"
		}
		if sourceMap[src] == nil {
			sourceMap[src] = map[string][]kb.Document{}
		}
		sourceMap[src][ch] = append(sourceMap[src][ch], doc)
	}

	var tree []*node
	for src, channels := range sourceMap {
		srcNode := &node{Name: src, Path: src, IsDir: true}
		for ch, docs := range channels {
			chNode := &node{Name: ch, Path: src + "/" + ch, IsDir: true}
			for _, doc := range docs {
				title := doc.Title
				if title == "" {
					title = truncateID(doc.ID, 12)
				}
				docNode := &node{
					Name:  title + ".md",
					Path:  "db/" + doc.ID,
					IsDir: false,
					Size:  int64(len(doc.Content)),
				}
				chNode.Children = append(chNode.Children, docNode)
			}
			srcNode.Children = append(srcNode.Children, chNode)
		}
		tree = append(tree, srcNode)
	}

	writeJSON(w, http.StatusOK, map[string]any{"tree": tree})
}

func (s *Server) handleKBDocFile(w http.ResponseWriter, r *http.Request) {
	docID := r.URL.Query().Get("id")
	if docID == "" {
		writeError(w, http.StatusBadRequest, "id parameter required")
		return
	}
	if len(docID) > 100 {
		writeError(w, http.StatusBadRequest, "invalid document ID")
		return
	}

	doc, err := s.engine.GetDocument(r.Context(), docID)
	if err != nil || doc == nil {
		writeError(w, http.StatusNotFound, "document not found")
		return
	}

	// Format as markdown
	var md strings.Builder
	md.WriteString("# " + doc.Title + "\n\n")
	md.WriteString("- **Source**: " + string(doc.Source) + "\n")
	md.WriteString("- **Channel**: " + doc.Channel + "\n")
	if doc.Author != "" {
		md.WriteString("- **Author**: " + doc.Author + "\n")
	}
	if !doc.Timestamp.IsZero() {
		md.WriteString("- **Date**: " + doc.Timestamp.Format("2006-01-02 15:04") + "\n")
	}
	if doc.URL != "" {
		md.WriteString("- **URL**: " + doc.URL + "\n")
	}
	md.WriteString("\n---\n\n")
	md.WriteString(doc.Content)

	writeJSON(w, http.StatusOK, map[string]any{
		"path":    "db/" + doc.ID,
		"name":    doc.Title + ".md",
		"content": md.String(),
	})
}

func (s *Server) handleCreateDocument(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 12<<20)
	// #nosec G120 -- request body is hard-capped with MaxBytesReader immediately above.
	if err := r.ParseMultipartForm(10 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request: max 10MB")
		return
	}

	title := strings.TrimSpace(r.FormValue("title"))
	if title == "" {
		writeError(w, http.StatusBadRequest, "title is required")
		return
	}
	if len(title) > 500 {
		writeError(w, http.StatusBadRequest, "title too long (max 500 characters)")
		return
	}

	content := r.FormValue("content")
	contentType := r.FormValue("content_type")
	if contentType == "" {
		contentType = "text/markdown"
	}
	category := r.FormValue("category")
	author := r.FormValue("author")
	url := r.FormValue("url")

	metadata := map[string]string{}

	// Handle file upload
	file, header, err := r.FormFile("file")
	if err == nil {
		defer file.Close()
		if header.Size > 10<<20 {
			writeError(w, http.StatusBadRequest, "file too large (max 10MB)")
			return
		}
		data, err := io.ReadAll(io.LimitReader(file, 10<<20+1))
		if err != nil {
			writeError(w, http.StatusBadRequest, "failed to read file")
			return
		}
		if int64(len(data)) > 10<<20 {
			writeError(w, http.StatusBadRequest, "file too large (max 10MB)")
			return
		}

		// Detect MIME type
		mimeType := header.Header.Get("Content-Type")
		if mimeType == "" || mimeType == "application/octet-stream" {
			mimeType = http.DetectContentType(data)
		}

		// Extract content from file
		if s.extractor != nil {
			result, err := s.extractor.ExtractFromBytes(r.Context(), data, mimeType, header.Filename)
			if err == nil && result.Text != "" {
				content = result.Text
				contentType = result.MimeType
			} else if err != nil {
				s.logger.Warn("file extraction failed, using empty content", "filename", header.Filename, "error", err)
			}
		}

		metadata["original_filename"] = header.Filename
	}

	if content == "" && metadata["original_filename"] == "" {
		writeError(w, http.StatusBadRequest, "content or file is required")
		return
	}

	now := time.Now()
	doc := kb.Document{
		Source:      kb.SourceManual,
		SourceID:    fmt.Sprintf("manual-%d", now.UnixNano()),
		Title:       title,
		Content:     content,
		ContentType: contentType,
		Author:      author,
		Channel:     category,
		URL:         url,
		Timestamp:   now,
		UpdatedAt:   now,
		IndexedAt:   now,
		Metadata:    metadata,
	}

	if err := s.engine.Index(r.Context(), doc); err != nil {
		s.logger.Error("failed to create document", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to create document")
		return
	}

	writeJSON(w, http.StatusCreated, doc)
}

func (s *Server) handleUpdateDocument(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" || len(id) > 100 {
		writeError(w, http.StatusBadRequest, "invalid document ID")
		return
	}

	existing, err := s.engine.GetDocument(r.Context(), id)
	if err != nil {
		s.logger.Warn("get document failed", "id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to get document")
		return
	}
	if existing == nil {
		writeError(w, http.StatusNotFound, "document not found")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 12<<20)
	// #nosec G120 -- request body is hard-capped with MaxBytesReader immediately above.
	if err := r.ParseMultipartForm(10 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request: max 10MB")
		return
	}

	if title := strings.TrimSpace(r.FormValue("title")); title != "" {
		if len(title) > 500 {
			writeError(w, http.StatusBadRequest, "title too long (max 500 characters)")
			return
		}
		existing.Title = title
	}

	if category := r.FormValue("category"); category != "" {
		existing.Channel = category
	}
	if author := r.FormValue("author"); author != "" {
		existing.Author = author
	}
	if url := r.FormValue("url"); url != "" {
		existing.URL = url
	}
	if ct := r.FormValue("content_type"); ct != "" {
		existing.ContentType = ct
	}

	// Handle file upload
	file, header, err := r.FormFile("file")
	if err == nil {
		defer file.Close()
		if header.Size > 10<<20 {
			writeError(w, http.StatusBadRequest, "file too large (max 10MB)")
			return
		}
		data, err := io.ReadAll(io.LimitReader(file, 10<<20+1))
		if err != nil {
			writeError(w, http.StatusBadRequest, "failed to read file")
			return
		}
		if int64(len(data)) > 10<<20 {
			writeError(w, http.StatusBadRequest, "file too large (max 10MB)")
			return
		}
		mimeType := header.Header.Get("Content-Type")
		if mimeType == "" || mimeType == "application/octet-stream" {
			mimeType = http.DetectContentType(data)
		}
		if s.extractor != nil {
			result, err := s.extractor.ExtractFromBytes(r.Context(), data, mimeType, header.Filename)
			if err == nil && result.Text != "" {
				existing.Content = result.Text
				existing.ContentType = result.MimeType
			}
		}
		if existing.Metadata == nil {
			existing.Metadata = map[string]string{}
		}
		existing.Metadata["original_filename"] = header.Filename
	} else if content := r.FormValue("content"); content != "" {
		existing.Content = content
	}

	existing.UpdatedAt = time.Now()

	if err := s.engine.Index(r.Context(), *existing); err != nil {
		s.logger.Error("failed to update document", "id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to update document")
		return
	}

	writeJSON(w, http.StatusOK, existing)
}

func (s *Server) handleDeleteDocument(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" || len(id) > 100 {
		writeError(w, http.StatusBadRequest, "invalid document ID")
		return
	}

	if err := s.store.DeleteDocument(id); err != nil {
		s.logger.Error("failed to delete document", "id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to delete document")
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		// Response has already started; best effort only.
		return
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// truncateID returns id truncated to maxLen characters, or the full id if shorter.
const defaultIDTruncLen = 12

func truncateID(id string, maxLen int) string {
	if len(id) <= maxLen {
		return id
	}
	return id[:maxLen]
}
