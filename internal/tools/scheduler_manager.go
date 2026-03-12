package tools

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/robfig/cron/v3"
	"github.com/slack-go/slack"

	"github.com/xylolabsinc/xylolabs-kb/internal/storage"
)

// SchedulerStore abstracts job persistence for the scheduler manager.
type SchedulerStore interface {
	CreateScheduledJob(job storage.ScheduledJob) error
	GetScheduledJob(id string) (*storage.ScheduledJob, error)
	ListScheduledJobs() ([]storage.ScheduledJob, error)
	ListScheduledJobsByCreator(userID string) ([]storage.ScheduledJob, error)
	DeleteScheduledJob(id string) error
}

// SchedulerManager handles scheduling operations for the tool executor.
type SchedulerManager struct {
	store    SchedulerStore
	slack    *slack.Client
	location *time.Location
	logger   *slog.Logger

	mu       sync.RWMutex
	channels map[string]string // channel name → channel ID cache
}

// NewSchedulerManager creates a new SchedulerManager.
func NewSchedulerManager(store SchedulerStore, slackClient *slack.Client, location *time.Location, logger *slog.Logger) *SchedulerManager {
	return &SchedulerManager{
		store:    store,
		slack:    slackClient,
		location: location,
		logger:   logger.With("component", "scheduler-manager"),
		channels: make(map[string]string),
	}
}

// ScheduleMessage creates a one-time scheduled message.
func (sm *SchedulerManager) ScheduleMessage(channel, message, sendAt, createdBy string) (map[string]any, error) {
	channelID, err := sm.resolveChannel(channel)
	if err != nil {
		return nil, fmt.Errorf("resolve channel %q: %w", channel, err)
	}

	runAt, err := sm.parseTime(sendAt)
	if err != nil {
		return nil, fmt.Errorf("parse send_at %q: %w", sendAt, err)
	}

	if runAt.Before(time.Now()) {
		return nil, fmt.Errorf("send_at must be in the future")
	}

	job := storage.ScheduledJob{
		ID:        uuid.New().String(),
		Type:      "once",
		ChannelID: channelID,
		Message:   message,
		RunAt:     runAt.UTC(),
		NextRun:   runAt.UTC(),
		CreatedBy: createdBy,
		Enabled:   true,
	}

	if err := sm.store.CreateScheduledJob(job); err != nil {
		return nil, err
	}

	return map[string]any{
		"job_id":     job.ID,
		"type":       "once",
		"channel_id": channelID,
		"send_at":    runAt.In(sm.location).Format(time.RFC3339),
		"status":     "scheduled",
	}, nil
}

// CreateRecurringJob creates a recurring cron job.
func (sm *SchedulerManager) CreateRecurringJob(channel, message, cronExpr, description, createdBy string) (map[string]any, error) {
	channelID, err := sm.resolveChannel(channel)
	if err != nil {
		return nil, fmt.Errorf("resolve channel %q: %w", channel, err)
	}

	// Validate cron expression
	sched, err := cron.ParseStandard(cronExpr)
	if err != nil {
		return nil, fmt.Errorf("invalid cron expression %q: %w", cronExpr, err)
	}

	now := time.Now().In(sm.location)
	nextRun := sched.Next(now)

	msg := message
	if description != "" {
		msg = message // description is metadata, message is what gets posted
	}

	job := storage.ScheduledJob{
		ID:        uuid.New().String(),
		Type:      "recurring",
		ChannelID: channelID,
		Message:   msg,
		CronExpr:  cronExpr,
		NextRun:   nextRun.UTC(),
		CreatedBy: createdBy,
		Enabled:   true,
	}

	if err := sm.store.CreateScheduledJob(job); err != nil {
		return nil, err
	}

	return map[string]any{
		"job_id":          job.ID,
		"type":            "recurring",
		"channel_id":      channelID,
		"cron_expression": cronExpr,
		"next_run":        nextRun.In(sm.location).Format(time.RFC3339),
		"status":          "scheduled",
	}, nil
}

// ListJobs returns all enabled scheduled jobs.
func (sm *SchedulerManager) ListJobs() (map[string]any, error) {
	jobs, err := sm.store.ListScheduledJobs()
	if err != nil {
		return nil, err
	}

	var results []map[string]any
	for _, job := range jobs {
		entry := map[string]any{
			"job_id":     job.ID,
			"type":       job.Type,
			"channel_id": job.ChannelID,
			"message":    job.Message,
			"enabled":    job.Enabled,
			"created_by": job.CreatedBy,
			"created_at": job.CreatedAt.In(sm.location).Format(time.RFC3339),
		}
		if job.Type == "recurring" {
			entry["cron_expression"] = job.CronExpr
		}
		if !job.NextRun.IsZero() {
			entry["next_run"] = job.NextRun.In(sm.location).Format(time.RFC3339)
		}
		results = append(results, entry)
	}

	return map[string]any{"jobs": results, "count": len(results)}, nil
}

// CancelJob deletes a scheduled job by ID.
func (sm *SchedulerManager) CancelJob(jobID string) (map[string]any, error) {
	job, err := sm.store.GetScheduledJob(jobID)
	if err != nil {
		return nil, err
	}
	if job == nil {
		return nil, fmt.Errorf("job %q not found", jobID)
	}

	if err := sm.store.DeleteScheduledJob(jobID); err != nil {
		return nil, err
	}

	return map[string]any{
		"job_id": jobID,
		"status": "cancelled",
	}, nil
}

// resolveChannel converts a channel name or ID to a channel ID.
func (sm *SchedulerManager) resolveChannel(channel string) (string, error) {
	// Already a channel ID
	if strings.HasPrefix(channel, "C") && len(channel) > 8 {
		return channel, nil
	}

	// Strip # prefix if present
	name := strings.TrimPrefix(channel, "#")

	// Check cache
	sm.mu.RLock()
	if id, ok := sm.channels[name]; ok {
		sm.mu.RUnlock()
		return id, nil
	}
	sm.mu.RUnlock()

	// Look up via Slack API
	channels, _, err := sm.slack.GetConversations(&slack.GetConversationsParameters{
		Types:           []string{"public_channel", "private_channel"},
		Limit:           1000,
		ExcludeArchived: true,
	})
	if err != nil {
		return "", fmt.Errorf("list channels: %w", err)
	}

	sm.mu.Lock()
	for _, ch := range channels {
		sm.channels[ch.Name] = ch.ID
	}
	sm.mu.Unlock()

	sm.mu.RLock()
	id, ok := sm.channels[name]
	sm.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("channel %q not found", name)
	}

	return id, nil
}

// parseTime parses a time string. Supports RFC3339 and relative expressions.
func (sm *SchedulerManager) parseTime(s string) (time.Time, error) {
	// Try RFC3339 first
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}

	// Try relative expressions: "in X minutes", "in X hours"
	s = strings.TrimSpace(strings.ToLower(s))
	now := time.Now().In(sm.location)

	if strings.HasPrefix(s, "in ") {
		parts := strings.Fields(s)
		if len(parts) >= 3 {
			var duration time.Duration
			var n int
			if _, err := fmt.Sscanf(parts[1], "%d", &n); err == nil {
				unit := parts[2]
				switch {
				case strings.HasPrefix(unit, "minute"), strings.HasPrefix(unit, "min"), strings.HasPrefix(unit, "분"):
					duration = time.Duration(n) * time.Minute
				case strings.HasPrefix(unit, "hour"), strings.HasPrefix(unit, "시간"):
					duration = time.Duration(n) * time.Hour
				case strings.HasPrefix(unit, "day"), strings.HasPrefix(unit, "일"):
					duration = time.Duration(n) * 24 * time.Hour
				case strings.HasPrefix(unit, "second"), strings.HasPrefix(unit, "sec"), strings.HasPrefix(unit, "초"):
					duration = time.Duration(n) * time.Second
				}
				if duration > 0 {
					return now.Add(duration), nil
				}
			}
		}
	}

	// Try common date-time formats
	formats := []string{
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
		"15:04",
		"3:04PM",
		"3:04pm",
	}
	for _, format := range formats {
		if t, err := time.ParseInLocation(format, s, sm.location); err == nil {
			// For time-only formats, set to today
			if format == "15:04" || format == "3:04PM" || format == "3:04pm" {
				t = time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, sm.location)
				// If the time has already passed today, schedule for tomorrow
				if t.Before(now) {
					t = t.Add(24 * time.Hour)
				}
			}
			return t, nil
		}
	}

	return time.Time{}, fmt.Errorf("unsupported time format: %s", s)
}
