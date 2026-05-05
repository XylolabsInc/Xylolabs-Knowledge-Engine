package worker

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/xylolabsinc/xylolabs-kb/internal/storage"
)

type mockJobStore struct {
	jobs    []storage.ScheduledJob
	deleted []string
	updated map[string]time.Time
}

func (m *mockJobStore) GetDueJobs(now time.Time) ([]storage.ScheduledJob, error) {
	return m.jobs, nil
}

func (m *mockJobStore) DeleteScheduledJob(id string) error {
	m.deleted = append(m.deleted, id)
	return nil
}

func (m *mockJobStore) UpdateNextRun(id string, nextRun time.Time) error {
	if m.updated == nil {
		m.updated = make(map[string]time.Time)
	}
	m.updated[id] = nextRun
	return nil
}

type mockPoster struct {
	posted []postCall
}

type postCall struct {
	channelID string
	text      string
}

func (m *mockPoster) PostMessage(ctx context.Context, channelID, text string, threadTS string) (string, error) {
	m.posted = append(m.posted, postCall{channelID: channelID, text: text})
	return "1234.5678", nil
}

func TestJobSchedulerPollOnce(t *testing.T) {
	store := &mockJobStore{
		jobs: []storage.ScheduledJob{
			{ID: "j1", Type: "once", ChannelID: "C123", Message: "hello"},
		},
	}
	poster := &mockPoster{}
	js := NewJobScheduler(store, poster, time.UTC, slog.Default())

	js.poll()

	if len(poster.posted) != 1 {
		t.Fatalf("expected 1 post, got %d", len(poster.posted))
	}
	if poster.posted[0].text != "hello" {
		t.Errorf("expected message 'hello', got %q", poster.posted[0].text)
	}
	if len(store.deleted) != 1 || store.deleted[0] != "j1" {
		t.Errorf("expected j1 to be deleted, got %v", store.deleted)
	}
}

func TestJobSchedulerPollRecurring(t *testing.T) {
	store := &mockJobStore{
		jobs: []storage.ScheduledJob{
			{ID: "j2", Type: "recurring", ChannelID: "C456", Message: "repeat", CronExpr: "0 9 * * 1-5"},
		},
	}
	poster := &mockPoster{}
	js := NewJobScheduler(store, poster, time.UTC, slog.Default())

	js.poll()

	if len(poster.posted) != 1 {
		t.Fatalf("expected 1 post, got %d", len(poster.posted))
	}
	if len(store.deleted) != 0 {
		t.Errorf("recurring job should not be deleted, got %v", store.deleted)
	}
	if _, ok := store.updated["j2"]; !ok {
		t.Error("expected next_run to be updated for recurring job")
	}
}

func TestJobSchedulerComputeNextRun(t *testing.T) {
	js := NewJobScheduler(nil, nil, time.UTC, slog.Default())

	from := time.Date(2026, 4, 18, 10, 0, 0, 0, time.UTC) // Saturday
	next, err := js.computeNextRun("0 9 * * 1", from)
	if err != nil {
		t.Fatalf("computeNextRun failed: %v", err)
	}
	// Next Monday at 9am
	expected := time.Date(2026, 4, 20, 9, 0, 0, 0, time.UTC)
	if !next.Equal(expected) {
		t.Errorf("expected %v, got %v", expected, next)
	}
}

func TestJobSchedulerComputeNextRunInvalid(t *testing.T) {
	js := NewJobScheduler(nil, nil, time.UTC, slog.Default())
	_, err := js.computeNextRun("invalid-cron", time.Now())
	if err == nil {
		t.Error("expected error for invalid cron expression")
	}
}
