package kb

import "time"

// Source identifies the origin system of a document.
type Source string

const (
	SourceSlack   Source = "slack"
	SourceGoogle  Source = "google"
	SourceNotion  Source = "notion"
	SourceDiscord Source = "discord"
	SourceManual  Source = "manual"
)

// Document represents a single piece of content in the knowledge base.
type Document struct {
	ID          string            `json:"id"`
	Source      Source            `json:"source"`
	SourceID    string            `json:"source_id"`
	ParentID    string            `json:"parent_id"`
	Title       string            `json:"title"`
	Content     string            `json:"content"`
	ContentType string            `json:"content_type"`
	Author      string            `json:"author"`
	AuthorEmail string            `json:"author_email"`
	Channel     string            `json:"channel"`
	Workspace   string            `json:"workspace"`
	URL         string            `json:"url"`
	Timestamp   time.Time         `json:"timestamp"`
	UpdatedAt   time.Time         `json:"updated_at"`
	IndexedAt   time.Time         `json:"indexed_at"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	Attachments []Attachment      `json:"attachments,omitempty"`
}

// Attachment represents a file attached to a document.
type Attachment struct {
	ID           string    `json:"id"`
	DocumentID   string    `json:"document_id"`
	Filename     string    `json:"filename"`
	MimeType     string    `json:"mime_type"`
	Size         int64     `json:"size"`
	SourceURL    string    `json:"source_url"`
	LocalPath    string    `json:"local_path"`
	DownloadedAt time.Time `json:"downloaded_at"`
}

// SearchResult wraps a document with relevance scoring.
type SearchResult struct {
	Document Document `json:"document"`
	Score    float64  `json:"score"`
	Snippet  string  `json:"snippet"`
}

// SearchQuery specifies filters and parameters for a search.
type SearchQuery struct {
	Query    string
	Source   Source
	Channel  string
	Author   string
	DateFrom time.Time
	DateTo   time.Time
	Limit    int
	Offset   int
}

// SyncState tracks the synchronization cursor for a source.
type SyncState struct {
	Source     Source
	LastSyncAt time.Time
	Cursor     string
	Metadata   map[string]string
}

// Stats holds aggregate statistics about the knowledge base.
type Stats struct {
	TotalDocuments    int64                `json:"total_documents"`
	DocumentsBySource map[Source]int64     `json:"documents_by_source"`
	DocumentsByType   map[string]int64     `json:"documents_by_type"`
	TotalAttachments  int64                `json:"total_attachments"`
	AttachmentSize    int64                `json:"attachment_size"`
	LastSyncTimes     map[Source]time.Time `json:"last_sync_times"`
}

// ListDocumentsQuery specifies filters for listing documents.
type ListDocumentsQuery struct {
	Source Source
	Since  time.Time
	Limit  int
	Offset int
}

// ListDocumentsResult holds the paginated result of listing documents.
type ListDocumentsResult struct {
	Documents []Document `json:"documents"`
	Total     int64      `json:"total"`
	HasMore   bool       `json:"has_more"`
}

// Storage defines the persistence interface for the knowledge base.
type Storage interface {
	// Document operations
	UpsertDocument(doc Document) error
	GetDocument(id string) (*Document, error)
	GetDocumentBySourceID(source Source, sourceID string) (*Document, error)
	DeleteDocument(id string) error
	ListDocuments(query ListDocumentsQuery) (*ListDocumentsResult, error)

	// Attachment operations
	UpsertAttachment(att Attachment) error
	GetAttachments(documentID string) ([]Attachment, error)

	// Search
	Search(query SearchQuery) ([]SearchResult, error)

	// Sync state
	GetSyncState(source Source) (*SyncState, error)
	SetSyncState(state SyncState) error

	// Stats
	GetStats() (*Stats, error)

	// Health
	Ping() error

	// Lifecycle
	Close() error
}

// Connector defines the interface for external data source connectors.
type Connector interface {
	// Name returns the source identifier.
	Name() Source

	// Start begins real-time event listening (if supported).
	Start(done <-chan struct{}) error

	// Sync performs a synchronization pass, fetching new/updated content.
	Sync() error

	// Stop gracefully shuts down the connector.
	Stop() error
}
