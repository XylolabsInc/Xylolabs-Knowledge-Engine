package storage

import (
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/xylolabsinc/xylolabs-kb/internal/kb"
)

func newTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	tmpFile, err := os.CreateTemp("", "test-*.db")
	if err != nil {
		t.Fatal(err)
	}
	tmpFile.Close()
	t.Cleanup(func() { os.Remove(tmpFile.Name()) })

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	store, err := New(tmpFile.Name(), logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestPing(t *testing.T) {
	store := newTestStore(t)
	if err := store.Ping(); err != nil {
		t.Errorf("Ping() error = %v", err)
	}
}

func TestUpsertAndGetDocument(t *testing.T) {
	store := newTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	doc := kb.Document{
		ID:          "test-doc-1",
		Source:      kb.SourceSlack,
		SourceID:    "S123",
		Title:       "Test Document",
		Content:     "Hello world",
		ContentType: "message",
		Author:      "alice",
		Channel:     "general",
		Timestamp:   now,
		UpdatedAt:   now,
		IndexedAt:   now,
	}

	if err := store.UpsertDocument(doc); err != nil {
		t.Fatalf("UpsertDocument() error = %v", err)
	}

	got, err := store.GetDocument("test-doc-1")
	if err != nil {
		t.Fatalf("GetDocument() error = %v", err)
	}
	if got == nil {
		t.Fatal("GetDocument() returned nil")
	}
	if got.Title != "Test Document" {
		t.Errorf("Title = %q, want %q", got.Title, "Test Document")
	}
	if got.Source != kb.SourceSlack {
		t.Errorf("Source = %q, want %q", got.Source, kb.SourceSlack)
	}
}

func TestGetDocumentNotFound(t *testing.T) {
	store := newTestStore(t)
	got, err := store.GetDocument("nonexistent")
	if err != nil {
		t.Fatalf("GetDocument() error = %v", err)
	}
	if got != nil {
		t.Error("GetDocument() should return nil for nonexistent doc")
	}
}

func TestGetDocumentBySourceID(t *testing.T) {
	store := newTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	doc := kb.Document{
		ID:        "test-src-1",
		Source:    kb.SourceGoogle,
		SourceID:  "G456",
		Title:     "Google Doc",
		Content:   "content",
		Timestamp: now,
		UpdatedAt: now,
		IndexedAt: now,
	}
	if err := store.UpsertDocument(doc); err != nil {
		t.Fatalf("UpsertDocument() error = %v", err)
	}

	got, err := store.GetDocumentBySourceID(kb.SourceGoogle, "G456")
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if got == nil || got.ID != "test-src-1" {
		t.Error("expected to find document by source ID")
	}
}

func TestDeleteDocument(t *testing.T) {
	store := newTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	doc := kb.Document{
		ID: "del-1", Source: kb.SourceSlack, SourceID: "D1",
		Timestamp: now, UpdatedAt: now, IndexedAt: now,
	}
	if err := store.UpsertDocument(doc); err != nil {
		t.Fatalf("UpsertDocument() error = %v", err)
	}
	if err := store.DeleteDocument("del-1"); err != nil {
		t.Fatalf("DeleteDocument() error = %v", err)
	}
	got, _ := store.GetDocument("del-1")
	if got != nil {
		t.Error("document should be deleted")
	}
}

func TestListDocuments(t *testing.T) {
	store := newTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < 5; i++ {
		doc := kb.Document{
			ID:        fmt.Sprintf("list-%d", i),
			Source:    kb.SourceSlack,
			SourceID:  fmt.Sprintf("L%d", i),
			Content:   "content",
			Timestamp: now.Add(time.Duration(i) * time.Minute),
			UpdatedAt: now,
			IndexedAt: now,
		}
		if err := store.UpsertDocument(doc); err != nil {
			t.Fatalf("UpsertDocument() error = %v", err)
		}
	}

	result, err := store.ListDocuments(kb.ListDocumentsQuery{Limit: 3})
	if err != nil {
		t.Fatalf("ListDocuments() error = %v", err)
	}
	if len(result.Documents) != 3 {
		t.Errorf("got %d documents, want 3", len(result.Documents))
	}
	if result.Total != 5 {
		t.Errorf("total = %d, want 5", result.Total)
	}
	if !result.HasMore {
		t.Error("HasMore should be true")
	}
}

func TestListDocumentsWithSourceFilter(t *testing.T) {
	store := newTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	for _, src := range []kb.Source{kb.SourceSlack, kb.SourceGoogle, kb.SourceSlack} {
		doc := kb.Document{
			ID:        fmt.Sprintf("filter-%s-%d", src, now.UnixNano()),
			Source:    src,
			SourceID:  fmt.Sprintf("F%d", now.UnixNano()),
			Timestamp: now,
			UpdatedAt: now,
			IndexedAt: now,
		}
		if err := store.UpsertDocument(doc); err != nil {
			t.Fatalf("UpsertDocument() error = %v", err)
		}
		now = now.Add(time.Second)
	}

	result, err := store.ListDocuments(kb.ListDocumentsQuery{Source: kb.SourceSlack, Limit: 100})
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if result.Total != 2 {
		t.Errorf("total = %d, want 2", result.Total)
	}
}

func TestSyncState(t *testing.T) {
	store := newTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)

	state := kb.SyncState{
		Source:     kb.SourceSlack,
		LastSyncAt: now,
		Cursor:     "cursor-123",
		Metadata:   map[string]string{"key": "value"},
	}
	if err := store.SetSyncState(state); err != nil {
		t.Fatalf("SetSyncState() error = %v", err)
	}

	got, err := store.GetSyncState(kb.SourceSlack)
	if err != nil {
		t.Fatalf("GetSyncState() error = %v", err)
	}
	if got == nil {
		t.Fatal("GetSyncState() returned nil")
	}
	if got.Cursor != "cursor-123" {
		t.Errorf("Cursor = %q, want %q", got.Cursor, "cursor-123")
	}
}

func TestGetStats(t *testing.T) {
	store := newTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	doc := kb.Document{
		ID: "stats-1", Source: kb.SourceSlack, SourceID: "ST1",
		ContentType: "message", Timestamp: now, UpdatedAt: now, IndexedAt: now,
	}
	if err := store.UpsertDocument(doc); err != nil {
		t.Fatalf("UpsertDocument() error = %v", err)
	}

	stats, err := store.GetStats()
	if err != nil {
		t.Fatalf("GetStats() error = %v", err)
	}
	if stats.TotalDocuments != 1 {
		t.Errorf("TotalDocuments = %d, want 1", stats.TotalDocuments)
	}
}

func TestScheduledJobs(t *testing.T) {
	store := newTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)

	job := ScheduledJob{
		ID:        "job-1",
		Type:      "once",
		ChannelID: "C123",
		Message:   "hello",
		RunAt:     now.Add(time.Hour),
		NextRun:   now.Add(time.Hour),
		CreatedBy: "U123",
		Enabled:   true,
	}
	if err := store.CreateScheduledJob(job); err != nil {
		t.Fatalf("CreateScheduledJob() error = %v", err)
	}

	got, err := store.GetScheduledJob("job-1")
	if err != nil {
		t.Fatalf("GetScheduledJob() error = %v", err)
	}
	if got == nil || got.Message != "hello" {
		t.Error("expected to find job")
	}

	jobs, err := store.ListScheduledJobs()
	if err != nil {
		t.Fatalf("ListScheduledJobs() error = %v", err)
	}
	if len(jobs) != 1 {
		t.Errorf("got %d jobs, want 1", len(jobs))
	}
}

func TestUpsertDocumentUpdate(t *testing.T) {
	store := newTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	doc := kb.Document{
		ID: "upsert-1", Source: kb.SourceSlack, SourceID: "U1",
		Title: "Original", Content: "content",
		Timestamp: now, UpdatedAt: now, IndexedAt: now,
	}
	if err := store.UpsertDocument(doc); err != nil {
		t.Fatalf("UpsertDocument() error = %v", err)
	}

	doc.Title = "Updated"
	doc.UpdatedAt = now.Add(time.Hour)
	if err := store.UpsertDocument(doc); err != nil {
		t.Fatalf("UpsertDocument() update error = %v", err)
	}

	got, _ := store.GetDocument("upsert-1")
	if got.Title != "Updated" {
		t.Errorf("Title = %q after upsert, want %q", got.Title, "Updated")
	}
}
