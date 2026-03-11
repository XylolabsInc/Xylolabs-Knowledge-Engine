package api

import (
	"encoding/json"
	"net/http"
	"strconv"
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

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
