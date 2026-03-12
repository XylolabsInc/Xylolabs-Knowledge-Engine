package api

import (
	"encoding/json"
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
	writeJSON(w, http.StatusOK, map[string]string{
		"status": "ok",
		"time":   time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	queryText := q.Get("q")
	if queryText == "" {
		writeError(w, http.StatusBadRequest, "query parameter 'q' is required")
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

	items := make([]resultItem, 0, len(results))
	for _, r := range results {
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
		"total":   len(items),
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

	writeJSON(w, http.StatusOK, map[string]any{
		"total_documents":     stats.TotalDocuments,
		"documents_by_source": bySource,
		"documents_by_type":   stats.DocumentsByType,
		"total_attachments":   stats.TotalAttachments,
		"attachment_size":     stats.AttachmentSize,
		"last_sync_times":     syncTimes,
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
			source["cursor"]    = syncState.Cursor
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
	case kb.SourceSlack, kb.SourceGoogle, kb.SourceNotion:
		// valid
	default:
		writeError(w, http.StatusBadRequest, "unknown source: "+source)
		return
	}

	// Run sync asynchronously
	go func() {
		if err := s.syncManager.SyncSource(kbSource); err != nil {
			s.logger.Warn("manual sync failed", "source", source, "error", err)
		}
	}()

	writeJSON(w, http.StatusAccepted, map[string]string{
		"status":  "sync triggered",
		"source":  source,
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

	filepath.WalkDir(s.kbRepoDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(s.kbRepoDir, path)
		if rel == "." {
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
	})

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

	// Security: prevent path traversal
	cleaned := filepath.Clean(filePath)
	if strings.Contains(cleaned, "..") || filepath.IsAbs(cleaned) {
		writeError(w, http.StatusBadRequest, "invalid path")
		return
	}

	fullPath := filepath.Join(s.kbRepoDir, cleaned)

	// Verify the path is within the KB repo
	if !strings.HasPrefix(fullPath, s.kbRepoDir) {
		writeError(w, http.StatusBadRequest, "invalid path")
		return
	}

	data, err := os.ReadFile(fullPath)
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
		srcNode := &node{Name: src, Path: "db/" + src, IsDir: true}
		for ch, docs := range channels {
			chNode := &node{Name: ch, Path: "db/" + src + "/" + ch, IsDir: true}
			for _, doc := range docs {
				title := doc.Title
				if title == "" {
					title = doc.ID[:12]
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

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
