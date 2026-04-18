package worker

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

func TestSchedulerRegister(t *testing.T) {
	s := NewScheduler(nilLogger())
	s.Register("test-job", 1*time.Hour, func(ctx context.Context) error { return nil })

	statuses := s.Status()
	if len(statuses) != 1 {
		t.Fatalf("expected 1 job, got %d", len(statuses))
	}
	if statuses[0].Name != "test-job" {
		t.Errorf("expected job name 'test-job', got %q", statuses[0].Name)
	}
	if statuses[0].RunCount != 0 {
		t.Errorf("expected run count 0, got %d", statuses[0].RunCount)
	}
}

func TestSchedulerRunNow(t *testing.T) {
	var runs atomic.Int64
	s := NewScheduler(nilLogger())
	s.Register("test-job", 1*time.Hour, func(ctx context.Context) error {
		runs.Add(1)
		return nil
	})

	if err := s.RunNow("test-job"); err != nil {
		t.Fatalf("RunNow failed: %v", err)
	}
	if runs.Load() != 1 {
		t.Errorf("expected 1 run, got %d", runs.Load())
	}

	statuses := s.Status()
	if statuses[0].RunCount != 1 {
		t.Errorf("expected run count 1, got %d", statuses[0].RunCount)
	}
}

func TestSchedulerRunNowNotFound(t *testing.T) {
	s := NewScheduler(nilLogger())
	err := s.RunNow("nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent job")
	}
}

func TestSchedulerRunNowError(t *testing.T) {
	s := NewScheduler(nilLogger())
	s.Register("failing-job", 1*time.Hour, func(ctx context.Context) error {
		return errors.New("job failed")
	})

	if err := s.RunNow("failing-job"); err == nil {
		t.Fatal("expected error from failing job")
	}

	statuses := s.Status()
	if statuses[0].ErrCount != 1 {
		t.Errorf("expected err count 1, got %d", statuses[0].ErrCount)
	}
}

func TestSchedulerStartStop(t *testing.T) {
	s := NewScheduler(nilLogger())
	var runs atomic.Int64
	s.Register("quick-job", 50*time.Millisecond, func(ctx context.Context) error {
		runs.Add(1)
		return nil
	})

	s.Start()
	time.Sleep(200 * time.Millisecond)
	s.Stop()

	if runs.Load() < 2 {
		t.Errorf("expected at least 2 runs (initial + periodic), got %d", runs.Load())
	}
}

func TestSchedulerRunNowConcurrentWithStatus(t *testing.T) {
	s := NewScheduler(nilLogger())
	s.Register("concurrent-job", 1*time.Hour, func(ctx context.Context) error {
		time.Sleep(10 * time.Millisecond)
		return nil
	})

	// This should not deadlock — the R-2 fix ensures RunNow releases RLock before executeJob
	errCh := make(chan error, 1)
	go func() {
		errCh <- s.RunNow("concurrent-job")
	}()

	// Call Status concurrently
	_ = s.Status()
	_ = s.Status()

	if err := <-errCh; err != nil {
		t.Fatalf("RunNow failed: %v", err)
	}
}

func nilLogger() *slog.Logger {
	return slog.Default()
}

func TestSchedulerMultipleJobs(t *testing.T) {
	s := NewScheduler(nilLogger())
	s.Register("job-a", 1*time.Hour, func(ctx context.Context) error { return nil })
	s.Register("job-b", 2*time.Hour, func(ctx context.Context) error { return nil })

	statuses := s.Status()
	if len(statuses) != 2 {
		t.Fatalf("expected 2 jobs, got %d", len(statuses))
	}
}
