package storage

import (
	"database/sql"
	"fmt"
)

var migrations = []struct {
	version int
	sql     string
}{
	{
		version: 1,
		sql: `
CREATE TABLE IF NOT EXISTS documents (
	id TEXT PRIMARY KEY,
	source TEXT NOT NULL,
	source_id TEXT NOT NULL,
	parent_id TEXT DEFAULT '',
	title TEXT DEFAULT '',
	content TEXT DEFAULT '',
	content_type TEXT DEFAULT '',
	author TEXT DEFAULT '',
	author_email TEXT DEFAULT '',
	channel TEXT DEFAULT '',
	workspace TEXT DEFAULT '',
	url TEXT DEFAULT '',
	timestamp DATETIME,
	updated_at DATETIME,
	indexed_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_documents_source ON documents(source, source_id);
CREATE INDEX IF NOT EXISTS idx_documents_channel ON documents(channel);
CREATE INDEX IF NOT EXISTS idx_documents_author ON documents(author);
CREATE INDEX IF NOT EXISTS idx_documents_timestamp ON documents(timestamp);
CREATE INDEX IF NOT EXISTS idx_documents_content_type ON documents(content_type);

CREATE VIRTUAL TABLE IF NOT EXISTS fts_documents USING fts5(
	title,
	content,
	author,
	channel,
	content_id UNINDEXED,
	content='documents',
	content_rowid='rowid',
	tokenize='porter unicode61'
);

CREATE TRIGGER IF NOT EXISTS documents_ai AFTER INSERT ON documents BEGIN
	INSERT INTO fts_documents(rowid, title, content, author, channel, content_id)
	VALUES (new.rowid, new.title, new.content, new.author, new.channel, new.id);
END;

CREATE TRIGGER IF NOT EXISTS documents_ad AFTER DELETE ON documents BEGIN
	INSERT INTO fts_documents(fts_documents, rowid, title, content, author, channel, content_id)
	VALUES ('delete', old.rowid, old.title, old.content, old.author, old.channel, old.id);
END;

CREATE TRIGGER IF NOT EXISTS documents_au AFTER UPDATE ON documents BEGIN
	INSERT INTO fts_documents(fts_documents, rowid, title, content, author, channel, content_id)
	VALUES ('delete', old.rowid, old.title, old.content, old.author, old.channel, old.id);
	INSERT INTO fts_documents(rowid, title, content, author, channel, content_id)
	VALUES (new.rowid, new.title, new.content, new.author, new.channel, new.id);
END;

CREATE TABLE IF NOT EXISTS attachments (
	id TEXT PRIMARY KEY,
	document_id TEXT NOT NULL,
	filename TEXT DEFAULT '',
	mime_type TEXT DEFAULT '',
	size INTEGER DEFAULT 0,
	source_url TEXT DEFAULT '',
	local_path TEXT DEFAULT '',
	downloaded_at DATETIME,
	FOREIGN KEY (document_id) REFERENCES documents(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_attachments_document ON attachments(document_id);

CREATE TABLE IF NOT EXISTS sync_state (
	source TEXT PRIMARY KEY,
	last_sync_at DATETIME,
	cursor TEXT DEFAULT '',
	metadata TEXT DEFAULT '{}'
);

CREATE TABLE IF NOT EXISTS schema_migrations (
	version INTEGER PRIMARY KEY,
	applied_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
`,
	},
	{
		version: 2,
		sql:     `ALTER TABLE documents ADD COLUMN metadata TEXT DEFAULT '{}';`,
	},
	{
		version: 3,
		sql: `
CREATE TABLE IF NOT EXISTS scheduled_jobs (
	id TEXT PRIMARY KEY,
	type TEXT NOT NULL,
	channel_id TEXT NOT NULL,
	message TEXT NOT NULL,
	cron_expr TEXT DEFAULT '',
	run_at DATETIME,
	next_run DATETIME,
	created_by TEXT DEFAULT '',
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	enabled INTEGER NOT NULL DEFAULT 1
);
CREATE INDEX IF NOT EXISTS idx_scheduled_jobs_next_run ON scheduled_jobs(next_run);
CREATE INDEX IF NOT EXISTS idx_scheduled_jobs_enabled ON scheduled_jobs(enabled);
`,
	},
	{
		version: 4,
		sql: `
-- Recreate FTS table without content_id (which doesn't exist in documents table).
-- The content-sync FTS5 reads columns by name from the content table,
-- so all FTS columns must match columns in the documents table.
DROP TRIGGER IF EXISTS documents_ai;
DROP TRIGGER IF EXISTS documents_ad;
DROP TRIGGER IF EXISTS documents_au;
DROP TABLE IF EXISTS fts_documents;

CREATE VIRTUAL TABLE fts_documents USING fts5(
	title,
	content,
	author,
	channel,
	content='documents',
	content_rowid='rowid',
	tokenize='porter unicode61'
);

CREATE TRIGGER documents_ai AFTER INSERT ON documents BEGIN
	INSERT INTO fts_documents(rowid, title, content, author, channel)
	VALUES (new.rowid, new.title, new.content, new.author, new.channel);
END;

CREATE TRIGGER documents_ad AFTER DELETE ON documents BEGIN
	INSERT INTO fts_documents(fts_documents, rowid, title, content, author, channel)
	VALUES ('delete', old.rowid, old.title, old.content, old.author, old.channel);
END;

CREATE TRIGGER documents_au AFTER UPDATE ON documents BEGIN
	INSERT INTO fts_documents(fts_documents, rowid, title, content, author, channel)
	VALUES ('delete', old.rowid, old.title, old.content, old.author, old.channel);
	INSERT INTO fts_documents(rowid, title, content, author, channel)
	VALUES (new.rowid, new.title, new.content, new.author, new.channel);
END;

-- Rebuild FTS index from existing documents.
INSERT INTO fts_documents(fts_documents) VALUES('rebuild');
`,
	},
}

// runMigrations applies all pending migrations.
func runMigrations(db *sql.DB) error {
	// Ensure schema_migrations table exists.
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		applied_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`); err != nil {
		return fmt.Errorf("create migrations table: %w", err)
	}

	for _, m := range migrations {
		var count int
		err := db.QueryRow("SELECT COUNT(*) FROM schema_migrations WHERE version = ?", m.version).Scan(&count)
		if err != nil {
			return fmt.Errorf("check migration %d: %w", m.version, err)
		}
		if count > 0 {
			continue
		}

		if _, err := db.Exec(m.sql); err != nil {
			return fmt.Errorf("apply migration %d: %w", m.version, err)
		}

		if _, err := db.Exec("INSERT INTO schema_migrations (version) VALUES (?)", m.version); err != nil {
			return fmt.Errorf("record migration %d: %w", m.version, err)
		}
	}
	return nil
}
