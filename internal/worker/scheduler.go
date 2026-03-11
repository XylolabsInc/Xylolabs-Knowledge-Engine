package worker

import (
	"log/slog"
	"sync"
	"time"
)

// Job represents a scheduled sync task.
type Job struct {
	Name     string
	Interval time.Duration
	Fn       func() error

	// Metrics
	LastRun   time.Time
	LastError error
	Duration  time.Duration
	RunCount  int64
	ErrCount  int64
}

// JobStatus exposes metrics about a job.
type JobStatus struct {
	Name      string        `json:"name"`
	Interval  string        `json:"interval"`
	LastRun   time.Time     `json:"last_run,omitempty"`
	LastError string        `json:"last_error,omitempty"`
	Duration  string        `json:"last_duration,omitempty"`
	RunCount  int64         `json:"run_count"`
	ErrCount  int64         `json:"error_count"`
}

// Scheduler manages periodic sync jobs.
type Scheduler struct {
	jobs   []*Job
	logger *slog.Logger
	done   chan struct{}
	wg     sync.WaitGroup
	mu     sync.RWMutex
}

// NewScheduler creates a job scheduler.
func NewScheduler(logger *slog.Logger) *Scheduler {
	return &Scheduler{
		logger: logger.With("component", "scheduler"),
		done:   make(chan struct{}),
	}
}

// Register adds a sync job to the scheduler.
func (s *Scheduler) Register(name string, interval time.Duration, fn func() error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.jobs = append(s.jobs, &Job{
		Name:     name,
		Interval: interval,
		Fn:       fn,
	})
	s.logger.Info("registered job", "name", name, "interval", interval)
}

// Start begins running all registered jobs.
func (s *Scheduler) Start() {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, job := range s.jobs {
		s.wg.Add(1)
		go s.runJob(job)
	}
	s.logger.Info("scheduler started", "jobs", len(s.jobs))
}

// Stop signals all jobs to stop and waits for completion.
func (s *Scheduler) Stop() {
	close(s.done)
	s.wg.Wait()
	s.logger.Info("scheduler stopped")
}

// Status returns the current status of all jobs.
func (s *Scheduler) Status() []JobStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()

	statuses := make([]JobStatus, len(s.jobs))
	for i, job := range s.jobs {
		status := JobStatus{
			Name:     job.Name,
			Interval: job.Interval.String(),
			LastRun:  job.LastRun,
			Duration: job.Duration.String(),
			RunCount: job.RunCount,
			ErrCount: job.ErrCount,
		}
		if job.LastError != nil {
			status.LastError = job.LastError.Error()
		}
		statuses[i] = status
	}
	return statuses
}

// RunNow triggers an immediate execution of a named job.
func (s *Scheduler) RunNow(name string) error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, job := range s.jobs {
		if job.Name == name {
			return s.executeJob(job)
		}
	}
	return nil
}

func (s *Scheduler) runJob(job *Job) {
	defer s.wg.Done()

	// Run once immediately at startup
	s.executeJob(job)

	ticker := time.NewTicker(job.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			s.executeJob(job)
		}
	}
}

func (s *Scheduler) executeJob(job *Job) error {
	start := time.Now()
	s.logger.Debug("running job", "name", job.Name)

	err := job.Fn()

	s.mu.Lock()
	job.LastRun = start
	job.Duration = time.Since(start)
	job.RunCount++
	if err != nil {
		job.LastError = err
		job.ErrCount++
	} else {
		job.LastError = nil
	}
	s.mu.Unlock()

	if err != nil {
		s.logger.Warn("job failed", "name", job.Name, "duration", job.Duration, "error", err)
	} else {
		s.logger.Info("job completed", "name", job.Name, "duration", job.Duration)
	}

	return err
}
