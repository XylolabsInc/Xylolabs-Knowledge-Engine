package kb

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"time"
)

// Engine orchestrates indexing and search across the knowledge base.
type Engine struct {
	store  Storage
	logger *slog.Logger
}

// NewEngine creates a new KB engine.
func NewEngine(store Storage, logger *slog.Logger) *Engine {
	return &Engine{
		store:  store,
		logger: logger.With("component", "kb-engine"),
	}
}

// Index adds or updates a single document in the knowledge base.
func (e *Engine) Index(ctx context.Context, doc Document) error {
	if doc.ID == "" {
		doc.ID = generateID(doc.Source, doc.SourceID)
	}
	doc.IndexedAt = time.Now().UTC()
	if doc.UpdatedAt.IsZero() {
		doc.UpdatedAt = doc.Timestamp
	}

	if err := e.store.UpsertDocument(doc); err != nil {
		return fmt.Errorf("index document %s: %w", doc.ID, err)
	}

	for i := range doc.Attachments {
		doc.Attachments[i].DocumentID = doc.ID
		if doc.Attachments[i].ID == "" {
			doc.Attachments[i].ID = generateID(doc.Source, doc.Attachments[i].SourceURL)
		}
		if err := e.store.UpsertAttachment(doc.Attachments[i]); err != nil {
			e.logger.Warn("failed to index attachment",
				"document_id", doc.ID,
				"attachment", doc.Attachments[i].Filename,
				"error", err,
			)
		}
	}

	e.logger.Debug("indexed document",
		"id", doc.ID,
		"source", doc.Source,
		"content_type", doc.ContentType,
	)
	return nil
}

// IndexBatch adds or updates multiple documents.
func (e *Engine) IndexBatch(ctx context.Context, docs []Document) error {
	var errs []error
	for _, doc := range docs {
		if ctx.Err() != nil {
			return fmt.Errorf("index batch cancelled: %w", ctx.Err())
		}
		if err := e.Index(ctx, doc); err != nil {
			errs = append(errs, err)
			e.logger.Warn("failed to index document in batch",
				"source_id", doc.SourceID,
				"error", err,
			)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("batch indexing: %d of %d documents failed", len(errs), len(docs))
	}
	return nil
}

// Search queries the knowledge base.
func (e *Engine) Search(ctx context.Context, query SearchQuery) ([]SearchResult, error) {
	if query.Limit <= 0 {
		query.Limit = 20
	}
	results, err := e.store.Search(query)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	return results, nil
}

// GetDocument retrieves a document by its internal ID.
func (e *Engine) GetDocument(ctx context.Context, id string) (*Document, error) {
	doc, err := e.store.GetDocument(id)
	if err != nil {
		return nil, fmt.Errorf("get document %s: %w", id, err)
	}
	if doc != nil {
		atts, err := e.store.GetAttachments(id)
		if err != nil {
			e.logger.Warn("failed to fetch attachments", "document_id", id, "error", err)
		} else {
			doc.Attachments = atts
		}
	}
	return doc, nil
}

// GetDocumentBySourceID retrieves a document by its source and external ID.
func (e *Engine) GetDocumentBySourceID(ctx context.Context, source Source, sourceID string) (*Document, error) {
	doc, err := e.store.GetDocumentBySourceID(source, sourceID)
	if err != nil {
		return nil, fmt.Errorf("get document by source %s/%s: %w", source, sourceID, err)
	}
	return doc, nil
}

// DeleteDocument removes a document from the knowledge base.
func (e *Engine) DeleteDocument(ctx context.Context, id string) error {
	if err := e.store.DeleteDocument(id); err != nil {
		return fmt.Errorf("delete document %s: %w", id, err)
	}
	return nil
}

// GetStats returns aggregate statistics about the knowledge base.
func (e *Engine) GetStats(ctx context.Context) (*Stats, error) {
	stats, err := e.store.GetStats()
	if err != nil {
		return nil, fmt.Errorf("get stats: %w", err)
	}
	return stats, nil
}

// generateID creates a deterministic ID from source and source-specific identifier.
func generateID(source Source, sourceID string) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s:%s", source, sourceID)))
	return fmt.Sprintf("%x", h[:16])
}
