package storage

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/xylolabsinc/xylolabs-kb/internal/kb"

	_ "modernc.org/sqlite"
)

// SQLiteStore implements kb.Storage backed by SQLite with FTS5.
type SQLiteStore struct {
	db     *sql.DB
	logger *slog.Logger
}

// New opens (or creates) a SQLite database and runs migrations.
func New(dsn string, logger *slog.Logger) (*SQLiteStore, error) {
	db, err := sql.Open("sqlite", dsn+"?_pragma=journal_mode(wal)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", dsn, err)
	}

	db.SetMaxOpenConns(1) // SQLite is single-writer

	if err := runMigrations(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("run migrations: %w", err)
	}

	return &SQLiteStore{db: db, logger: logger.With("component", "sqlite")}, nil
}

// Ping checks database connectivity.
func (s *SQLiteStore) Ping() error {
	return s.db.Ping()
}

// Close closes the database connection.
func (s *SQLiteStore) Close() error {
	return s.db.Close()
}

// UpsertDocument inserts or updates a document.
func (s *SQLiteStore) UpsertDocument(doc kb.Document) error {
	metaJSON := "{}"
	if doc.Metadata != nil {
		b, err := json.Marshal(doc.Metadata)
		if err != nil {
			return fmt.Errorf("marshal document metadata: %w", err)
		}
		metaJSON = string(b)
	}
	_, err := s.db.Exec(`
		INSERT INTO documents (id, source, source_id, parent_id, title, content, content_type,
			author, author_email, channel, workspace, url, timestamp, updated_at, indexed_at, metadata)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			title=excluded.title,
			content=excluded.content,
			content_type=excluded.content_type,
			author=excluded.author,
			author_email=excluded.author_email,
			channel=excluded.channel,
			workspace=excluded.workspace,
			url=excluded.url,
			updated_at=excluded.updated_at,
			indexed_at=excluded.indexed_at,
			metadata=excluded.metadata`,
		doc.ID, string(doc.Source), doc.SourceID, doc.ParentID,
		doc.Title, doc.Content, doc.ContentType,
		doc.Author, doc.AuthorEmail, doc.Channel, doc.Workspace, doc.URL,
		doc.Timestamp.UTC(), doc.UpdatedAt.UTC(), doc.IndexedAt.UTC(), metaJSON,
	)
	if err != nil {
		return fmt.Errorf("upsert document %s: %w", doc.ID, err)
	}
	return nil
}

// GetDocument retrieves a document by internal ID.
func (s *SQLiteStore) GetDocument(id string) (*kb.Document, error) {
	row := s.db.QueryRow(`SELECT id, source, source_id, parent_id, title, content, content_type,
		author, author_email, channel, workspace, url, timestamp, updated_at, indexed_at, metadata
		FROM documents WHERE id = ?`, id)
	doc, err := scanDocument(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get document %s: %w", id, err)
	}
	return doc, nil
}

// GetDocumentBySourceID retrieves a document by source and source-specific ID.
func (s *SQLiteStore) GetDocumentBySourceID(source kb.Source, sourceID string) (*kb.Document, error) {
	row := s.db.QueryRow(`SELECT id, source, source_id, parent_id, title, content, content_type,
		author, author_email, channel, workspace, url, timestamp, updated_at, indexed_at, metadata
		FROM documents WHERE source = ? AND source_id = ?`, string(source), sourceID)
	doc, err := scanDocument(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get document by source %s/%s: %w", source, sourceID, err)
	}
	return doc, nil
}

// DeleteDocument removes a document by internal ID.
func (s *SQLiteStore) DeleteDocument(id string) error {
	_, err := s.db.Exec("DELETE FROM documents WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("delete document %s: %w", id, err)
	}
	return nil
}

// RenameChannel updates all documents with oldName to newName for a given source.
func (s *SQLiteStore) RenameChannel(source kb.Source, oldName, newName string) (int64, error) {
	result, err := s.db.Exec(
		"UPDATE documents SET channel = ? WHERE source = ? AND channel = ?",
		newName, string(source), oldName,
	)
	if err != nil {
		return 0, fmt.Errorf("rename channel %s -> %s: %w", oldName, newName, err)
	}
	return result.RowsAffected()
}

// UpsertAttachment inserts or updates an attachment.
func (s *SQLiteStore) UpsertAttachment(att kb.Attachment) error {
	_, err := s.db.Exec(`
		INSERT INTO attachments (id, document_id, filename, mime_type, size, source_url, local_path, downloaded_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			filename=excluded.filename,
			mime_type=excluded.mime_type,
			size=excluded.size,
			source_url=excluded.source_url,
			local_path=excluded.local_path,
			downloaded_at=excluded.downloaded_at`,
		att.ID, att.DocumentID, att.Filename, att.MimeType,
		att.Size, att.SourceURL, att.LocalPath, nullTime(att.DownloadedAt),
	)
	if err != nil {
		return fmt.Errorf("upsert attachment %s: %w", att.ID, err)
	}
	return nil
}

// GetAttachments returns all attachments for a document.
func (s *SQLiteStore) GetAttachments(documentID string) ([]kb.Attachment, error) {
	rows, err := s.db.Query(`SELECT id, document_id, filename, mime_type, size,
		source_url, local_path, downloaded_at
		FROM attachments WHERE document_id = ?`, documentID)
	if err != nil {
		return nil, fmt.Errorf("get attachments for %s: %w", documentID, err)
	}
	defer rows.Close()

	var atts []kb.Attachment
	for rows.Next() {
		var att kb.Attachment
		var downloadedAt sql.NullTime
		if err := rows.Scan(&att.ID, &att.DocumentID, &att.Filename, &att.MimeType,
			&att.Size, &att.SourceURL, &att.LocalPath, &downloadedAt); err != nil {
			return nil, fmt.Errorf("scan attachment: %w", err)
		}
		if downloadedAt.Valid {
			att.DownloadedAt = downloadedAt.Time
		}
		atts = append(atts, att)
	}
	return atts, rows.Err()
}

// Search performs a full-text search with optional filters.
func (s *SQLiteStore) Search(query kb.SearchQuery) ([]kb.SearchResult, error) {
	var conditions []string
	var args []any

	baseQuery := `SELECT d.id, d.source, d.source_id, d.parent_id, d.title, d.content,
		d.content_type, d.author, d.author_email, d.channel, d.workspace, d.url,
		d.timestamp, d.updated_at, d.indexed_at,
		snippet(fts_documents, 1, '<b>', '</b>', '...', 32) AS snippet,
		bm25(fts_documents, 5.0, 1.0, 2.0, 2.0) AS score
		FROM fts_documents fts
		JOIN documents d ON d.rowid = fts.rowid
		WHERE fts_documents MATCH ?`

	args = append(args, query.Query)

	if query.Source != "" {
		conditions = append(conditions, "d.source = ?")
		args = append(args, string(query.Source))
	}
	if query.Channel != "" {
		conditions = append(conditions, "d.channel = ?")
		args = append(args, kb.NormalizeChannel(query.Channel))
	}
	if query.Author != "" {
		conditions = append(conditions, "d.author = ?")
		args = append(args, query.Author)
	}
	if !query.DateFrom.IsZero() {
		conditions = append(conditions, "d.timestamp >= ?")
		args = append(args, query.DateFrom.UTC())
	}
	if !query.DateTo.IsZero() {
		conditions = append(conditions, "d.timestamp <= ?")
		args = append(args, query.DateTo.UTC())
	}

	if len(conditions) > 0 {
		baseQuery += " AND " + strings.Join(conditions, " AND ")
	}

	// Blend BM25 relevance with recency: newer documents get a boost.
	// bm25() returns negative scores (lower = more relevant), so we add a
	// recency bonus that decays over 90 days. The result is still negative;
	// ORDER BY ascending gives best results first.
	baseQuery += ` ORDER BY (score + CASE
		WHEN d.timestamp > datetime('now', '-7 days') THEN -5.0
		WHEN d.timestamp > datetime('now', '-30 days') THEN -3.0
		WHEN d.timestamp > datetime('now', '-90 days') THEN -1.0
		ELSE 0.0 END) LIMIT ? OFFSET ?`
	args = append(args, query.Limit, query.Offset)

	rows, err := s.db.Query(baseQuery, args...)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	defer rows.Close()

	var results []kb.SearchResult
	for rows.Next() {
		var r kb.SearchResult
		var src string
		var ts, updatedAt, indexedAt time.Time
		if err := rows.Scan(
			&r.Document.ID, &src, &r.Document.SourceID, &r.Document.ParentID,
			&r.Document.Title, &r.Document.Content, &r.Document.ContentType,
			&r.Document.Author, &r.Document.AuthorEmail, &r.Document.Channel,
			&r.Document.Workspace, &r.Document.URL,
			&ts, &updatedAt, &indexedAt,
			&r.Snippet, &r.Score,
		); err != nil {
			return nil, fmt.Errorf("scan search result: %w", err)
		}
		r.Document.Source = kb.Source(src)
		r.Document.Timestamp = ts
		r.Document.UpdatedAt = updatedAt
		r.Document.IndexedAt = indexedAt
		results = append(results, r)
	}
	return results, rows.Err()
}

// ListDocuments returns a paginated list of documents with optional filters.
func (s *SQLiteStore) ListDocuments(query kb.ListDocumentsQuery) (*kb.ListDocumentsResult, error) {
	var conditions []string
	var args []any

	if query.Source != "" {
		conditions = append(conditions, "source = ?")
		args = append(args, string(query.Source))
	}
	if !query.Since.IsZero() {
		conditions = append(conditions, "timestamp > ?")
		args = append(args, query.Since.UTC())
	}

	where := ""
	if len(conditions) > 0 {
		where = "WHERE " + strings.Join(conditions, " AND ")
	}

	// Count total matching documents
	var total int64
	countQuery := "SELECT COUNT(*) FROM documents " + where
	if err := s.db.QueryRow(countQuery, args...).Scan(&total); err != nil {
		return nil, fmt.Errorf("count documents: %w", err)
	}

	limit := query.Limit
	if limit <= 0 {
		limit = 500
	}
	if limit > 1000 {
		limit = 1000
	}

	// Fetch documents
	selectQuery := `SELECT id, source, source_id, parent_id, title, content, content_type,
		author, author_email, channel, workspace, url, timestamp, updated_at, indexed_at, metadata
		FROM documents ` + where + ` ORDER BY timestamp DESC LIMIT ? OFFSET ?`
	args = append(args, limit, query.Offset)

	rows, err := s.db.Query(selectQuery, args...)
	if err != nil {
		return nil, fmt.Errorf("list documents: %w", err)
	}
	defer rows.Close()

	docs := make([]kb.Document, 0)
	for rows.Next() {
		doc, err := scanDocument(rows)
		if err != nil {
			return nil, fmt.Errorf("scan document: %w", err)
		}
		docs = append(docs, *doc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate documents: %w", err)
	}

	return &kb.ListDocumentsResult{
		Documents: docs,
		Total:     total,
		HasMore:   int64(query.Offset+len(docs)) < total,
	}, nil
}

// GetSyncState retrieves sync state for a source.
func (s *SQLiteStore) GetSyncState(source kb.Source) (*kb.SyncState, error) {
	var state kb.SyncState
	var metaJSON string
	var lastSync sql.NullTime
	err := s.db.QueryRow("SELECT source, last_sync_at, cursor, metadata FROM sync_state WHERE source = ?",
		string(source)).Scan(&state.Source, &lastSync, &state.Cursor, &metaJSON)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get sync state %s: %w", source, err)
	}
	if lastSync.Valid {
		state.LastSyncAt = lastSync.Time
	}
	if metaJSON != "" {
		if err := json.Unmarshal([]byte(metaJSON), &state.Metadata); err != nil {
			s.logger.Warn("failed to parse sync metadata", "source", source, "error", err)
			state.Metadata = make(map[string]string)
		}
	}
	return &state, nil
}

// SetSyncState updates sync state for a source.
func (s *SQLiteStore) SetSyncState(state kb.SyncState) error {
	metaJSON := "{}"
	if state.Metadata != nil {
		b, err := json.Marshal(state.Metadata)
		if err != nil {
			return fmt.Errorf("marshal sync metadata: %w", err)
		}
		metaJSON = string(b)
	}
	_, err := s.db.Exec(`
		INSERT INTO sync_state (source, last_sync_at, cursor, metadata)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(source) DO UPDATE SET
			last_sync_at=excluded.last_sync_at,
			cursor=excluded.cursor,
			metadata=excluded.metadata`,
		string(state.Source), state.LastSyncAt.UTC(), state.Cursor, metaJSON,
	)
	if err != nil {
		return fmt.Errorf("set sync state %s: %w", state.Source, err)
	}
	return nil
}

// GetStats returns aggregate knowledge base statistics.
func (s *SQLiteStore) GetStats() (*kb.Stats, error) {
	stats := &kb.Stats{
		DocumentsBySource: make(map[kb.Source]int64),
		DocumentsByType:   make(map[string]int64),
		LastSyncTimes:     make(map[kb.Source]time.Time),
	}

	// Total documents
	if err := s.db.QueryRow("SELECT COUNT(*) FROM documents").Scan(&stats.TotalDocuments); err != nil {
		return nil, fmt.Errorf("count documents: %w", err)
	}

	// By source
	rows, err := s.db.Query("SELECT source, COUNT(*) FROM documents GROUP BY source")
	if err != nil {
		return nil, fmt.Errorf("count by source: %w", err)
	}
	for rows.Next() {
		var src string
		var count int64
		if err := rows.Scan(&src, &count); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan source count: %w", err)
		}
		stats.DocumentsBySource[kb.Source(src)] = count
	}
	rows.Close()

	// By type
	rows, err = s.db.Query("SELECT content_type, COUNT(*) FROM documents GROUP BY content_type")
	if err != nil {
		return nil, fmt.Errorf("count by type: %w", err)
	}
	for rows.Next() {
		var ct string
		var count int64
		if err := rows.Scan(&ct, &count); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan type count: %w", err)
		}
		stats.DocumentsByType[ct] = count
	}
	rows.Close()

	// Attachments
	if err := s.db.QueryRow("SELECT COUNT(*), COALESCE(SUM(size),0) FROM attachments").Scan(
		&stats.TotalAttachments, &stats.AttachmentSize); err != nil {
		return nil, fmt.Errorf("count attachments: %w", err)
	}

	// Sync times
	syncRows, err := s.db.Query("SELECT source, last_sync_at FROM sync_state WHERE last_sync_at IS NOT NULL")
	if err != nil {
		return nil, fmt.Errorf("get sync times: %w", err)
	}
	for syncRows.Next() {
		var src string
		var t time.Time
		if err := syncRows.Scan(&src, &t); err != nil {
			syncRows.Close()
			return nil, fmt.Errorf("scan sync time: %w", err)
		}
		stats.LastSyncTimes[kb.Source(src)] = t
	}
	syncRows.Close()

	return stats, nil
}

// ScheduledJob represents a scheduled or recurring message job.
type ScheduledJob struct {
	ID        string
	Type      string // "once" or "recurring"
	ChannelID string
	Message   string
	CronExpr  string
	RunAt     time.Time
	NextRun   time.Time
	CreatedBy string
	CreatedAt time.Time
	Enabled   bool
	Platform  string // "slack" or "discord"
}

// CreateScheduledJob inserts a new scheduled job.
func (s *SQLiteStore) CreateScheduledJob(job ScheduledJob) error {
	enabled := 0
	if job.Enabled {
		enabled = 1
	}
	platform := job.Platform
	if platform == "" {
		platform = "slack"
	}
	_, err := s.db.Exec(`
		INSERT INTO scheduled_jobs (id, type, channel_id, message, cron_expr, run_at, next_run, created_by, enabled, platform)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		job.ID, job.Type, job.ChannelID, job.Message, job.CronExpr,
		nullTime(job.RunAt), nullTime(job.NextRun), job.CreatedBy, enabled, platform,
	)
	if err != nil {
		return fmt.Errorf("create scheduled job %s: %w", job.ID, err)
	}
	return nil
}

// GetScheduledJob retrieves a scheduled job by ID.
func (s *SQLiteStore) GetScheduledJob(id string) (*ScheduledJob, error) {
	var job ScheduledJob
	var runAt, nextRun, createdAt sql.NullTime
	var enabled int
	err := s.db.QueryRow(`
		SELECT id, type, channel_id, message, cron_expr, run_at, next_run, created_by, created_at, enabled, platform
		FROM scheduled_jobs WHERE id = ?`, id).Scan(
		&job.ID, &job.Type, &job.ChannelID, &job.Message, &job.CronExpr,
		&runAt, &nextRun, &job.CreatedBy, &createdAt, &enabled, &job.Platform,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get scheduled job %s: %w", id, err)
	}
	if runAt.Valid {
		job.RunAt = runAt.Time
	}
	if nextRun.Valid {
		job.NextRun = nextRun.Time
	}
	if createdAt.Valid {
		job.CreatedAt = createdAt.Time
	}
	job.Enabled = enabled == 1
	return &job, nil
}

// ListScheduledJobs returns all enabled scheduled jobs.
func (s *SQLiteStore) ListScheduledJobs() ([]ScheduledJob, error) {
	rows, err := s.db.Query(`
		SELECT id, type, channel_id, message, cron_expr, run_at, next_run, created_by, created_at, enabled, platform
		FROM scheduled_jobs WHERE enabled = 1 ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list scheduled jobs: %w", err)
	}
	defer rows.Close()
	return scanScheduledJobs(rows)
}

// ListScheduledJobsByCreator returns all enabled jobs created by a specific user.
func (s *SQLiteStore) ListScheduledJobsByCreator(userID string) ([]ScheduledJob, error) {
	rows, err := s.db.Query(`
		SELECT id, type, channel_id, message, cron_expr, run_at, next_run, created_by, created_at, enabled, platform
		FROM scheduled_jobs WHERE enabled = 1 AND created_by = ? ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("list scheduled jobs by creator %s: %w", userID, err)
	}
	defer rows.Close()
	return scanScheduledJobs(rows)
}

// DeleteScheduledJob removes a scheduled job by ID.
func (s *SQLiteStore) DeleteScheduledJob(id string) error {
	_, err := s.db.Exec("DELETE FROM scheduled_jobs WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("delete scheduled job %s: %w", id, err)
	}
	return nil
}

// UpdateNextRun updates the next_run time for a recurring job.
func (s *SQLiteStore) UpdateNextRun(id string, nextRun time.Time) error {
	_, err := s.db.Exec("UPDATE scheduled_jobs SET next_run = ? WHERE id = ?", nextRun.UTC(), id)
	if err != nil {
		return fmt.Errorf("update next run for %s: %w", id, err)
	}
	return nil
}

// DisableScheduledJob sets enabled=0 for a job.
func (s *SQLiteStore) DisableScheduledJob(id string) error {
	_, err := s.db.Exec("UPDATE scheduled_jobs SET enabled = 0 WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("disable scheduled job %s: %w", id, err)
	}
	return nil
}

// GetDueJobs returns all enabled jobs whose next_run is at or before the given time.
func (s *SQLiteStore) GetDueJobs(now time.Time) ([]ScheduledJob, error) {
	rows, err := s.db.Query(`
		SELECT id, type, channel_id, message, cron_expr, run_at, next_run, created_by, created_at, enabled, platform
		FROM scheduled_jobs WHERE enabled = 1 AND next_run <= ? ORDER BY next_run ASC`, now.UTC())
	if err != nil {
		return nil, fmt.Errorf("get due jobs: %w", err)
	}
	defer rows.Close()
	return scanScheduledJobs(rows)
}

func scanScheduledJobs(rows *sql.Rows) ([]ScheduledJob, error) {
	var jobs []ScheduledJob
	for rows.Next() {
		var job ScheduledJob
		var runAt, nextRun, createdAt sql.NullTime
		var enabled int
		if err := rows.Scan(
			&job.ID, &job.Type, &job.ChannelID, &job.Message, &job.CronExpr,
			&runAt, &nextRun, &job.CreatedBy, &createdAt, &enabled, &job.Platform,
		); err != nil {
			return nil, fmt.Errorf("scan scheduled job: %w", err)
		}
		if runAt.Valid {
			job.RunAt = runAt.Time
		}
		if nextRun.Valid {
			job.NextRun = nextRun.Time
		}
		if createdAt.Valid {
			job.CreatedAt = createdAt.Time
		}
		job.Enabled = enabled == 1
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

// scanner abstracts sql.Row and sql.Rows for shared scanning logic.
type scanner interface {
	Scan(dest ...any) error
}

func scanDocument(row scanner) (*kb.Document, error) {
	var doc kb.Document
	var src string
	var ts, updatedAt, indexedAt time.Time
	var metaJSON string
	err := row.Scan(
		&doc.ID, &src, &doc.SourceID, &doc.ParentID,
		&doc.Title, &doc.Content, &doc.ContentType,
		&doc.Author, &doc.AuthorEmail, &doc.Channel, &doc.Workspace, &doc.URL,
		&ts, &updatedAt, &indexedAt, &metaJSON,
	)
	if err != nil {
		return nil, err
	}
	doc.Source = kb.Source(src)
	doc.Timestamp = ts
	doc.UpdatedAt = updatedAt
	doc.IndexedAt = indexedAt
	if metaJSON != "" {
		if err := json.Unmarshal([]byte(metaJSON), &doc.Metadata); err != nil {
			doc.Metadata = make(map[string]string)
		}
	}
	return &doc, nil
}

func nullTime(t time.Time) sql.NullTime {
	if t.IsZero() {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: t, Valid: true}
}
