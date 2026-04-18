package worker

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/xylolabsinc/xylolabs-kb/internal/storage"
)

// MessagePoster abstracts sending messages to a channel.
type MessagePoster interface {
	PostMessage(ctx context.Context, channelID, text string, threadTS string) (timestamp string, err error)
}

// JobStore abstracts scheduled job persistence for testability.
type JobStore interface {
	GetDueJobs(now time.Time) ([]storage.ScheduledJob, error)
	DeleteScheduledJob(id string) error
	UpdateNextRun(id string, nextRun time.Time) error
}

const jobPostTimeout = 30 * time.Second

// JobScheduler polls for due scheduled jobs and posts messages to Slack.
type JobScheduler struct {
	store    JobStore
	poster   MessagePoster
	location *time.Location
	logger   *slog.Logger
	done     chan struct{}
	wg       sync.WaitGroup
}

// NewJobScheduler creates a new job scheduler.
func NewJobScheduler(store JobStore, poster MessagePoster, location *time.Location, logger *slog.Logger) *JobScheduler {
	return &JobScheduler{
		store:    store,
		poster:   poster,
		location: location,
		logger:   logger.With("component", "job-scheduler"),
		done:     make(chan struct{}),
	}
}

// Start begins the polling loop in a goroutine.
func (js *JobScheduler) Start() {
	js.wg.Add(1)
	go js.run()
	js.logger.Info("job scheduler started", "poll_interval", "30s")
}

// Stop signals the polling loop to stop and waits for it to finish.
func (js *JobScheduler) Stop() {
	close(js.done)
	js.wg.Wait()
	js.logger.Info("job scheduler stopped")
}

func (js *JobScheduler) run() {
	defer js.wg.Done()

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-js.done:
			return
		case <-ticker.C:
			js.poll()
		}
	}
}

func (js *JobScheduler) poll() {
	now := time.Now().In(js.location)
	jobs, err := js.store.GetDueJobs(now)
	if err != nil {
		js.logger.Error("failed to get due jobs", "error", err)
		return
	}

	for _, job := range jobs {
		js.logger.Info("executing scheduled job", "id", job.ID, "type", job.Type, "channel", job.ChannelID)

		postCtx, postCancel := context.WithTimeout(context.Background(), jobPostTimeout)
			_, err := js.poster.PostMessage(postCtx, job.ChannelID, job.Message, "")
			postCancel()
		if err != nil {
			js.logger.Error("failed to post scheduled message", "id", job.ID, "channel", job.ChannelID, "error", err)
			continue
		}

		if job.Type == "once" {
			if err := js.store.DeleteScheduledJob(job.ID); err != nil {
				js.logger.Error("failed to delete one-time job", "id", job.ID, "error", err)
			}
		} else if job.Type == "recurring" {
			nextRun, err := js.computeNextRun(job.CronExpr, now)
			if err != nil {
				js.logger.Error("failed to compute next run", "id", job.ID, "cron", job.CronExpr, "error", err)
				continue
			}
			if err := js.store.UpdateNextRun(job.ID, nextRun); err != nil {
				js.logger.Error("failed to update next run", "id", job.ID, "error", err)
			}
		}
	}
}

func (js *JobScheduler) computeNextRun(cronExpr string, from time.Time) (time.Time, error) {
	sched, err := cron.ParseStandard(cronExpr)
	if err != nil {
		return time.Time{}, err
	}
	return sched.Next(from), nil
}
