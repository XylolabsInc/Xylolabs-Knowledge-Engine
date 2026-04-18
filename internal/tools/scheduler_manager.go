package tools

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/robfig/cron/v3"
	"github.com/slack-go/slack"

	"github.com/xylolabsinc/xylolabs-kb/internal/storage"
)

// Control block patterns that must be stripped from outgoing messages.
var (
	reControlReact = regexp.MustCompile(`===REACT:\s*\S+?===`)
	reControlLearn = regexp.MustCompile(`(?s)===LEARN:.*?===ENDLEARN===[ \t]*\r?\n?`)
	reControlSkip  = regexp.MustCompile(`===SKIP===`)
)

// stripControlBlocks removes ===REACT:...===, ===LEARN:...===ENDLEARN===, and ===SKIP===
// from a message before posting to a channel.
func stripControlBlocks(text string) string {
	text = reControlReact.ReplaceAllString(text, "")
	text = reControlLearn.ReplaceAllString(text, "")
	text = reControlSkip.ReplaceAllString(text, "")
	return strings.TrimSpace(text)
}

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
	poster   MessagePoster
	resolver ChannelResolver
	location *time.Location
	logger   *slog.Logger
}

// NewSchedulerManager creates a new SchedulerManager.
func NewSchedulerManager(store SchedulerStore, poster MessagePoster, resolver ChannelResolver, location *time.Location, logger *slog.Logger) *SchedulerManager {
	return &SchedulerManager{
		store:    store,
		poster:   poster,
		resolver: resolver,
		location: location,
		logger:   logger.With("component", "scheduler-manager"),
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
		Message:   stripControlBlocks(message),
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
	return sm.resolver.ResolveChannel(channel)
}

// resolveChannelName performs a reverse lookup from channel ID to name.
func (sm *SchedulerManager) resolveChannelName(channelID string) string {
	return sm.resolver.ResolveChannelName(channelID)
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

// SendMessage sends a message to a Slack channel immediately.
func (sm *SchedulerManager) SendMessage(channel, message, threadTS string) (map[string]any, error) {
	channelID, err := sm.resolveChannel(channel)
	if err != nil {
		return nil, fmt.Errorf("resolve channel %q: %w", channel, err)
	}

	// Strip bot control blocks that should never appear in outgoing messages.
	message = stripControlBlocks(message)

	ts, err := sm.poster.PostMessage(context.Background(), channelID, message, threadTS)
	if err != nil {
		return nil, fmt.Errorf("send message to %s: %w", channel, err)
	}

	channelName := sm.resolveChannelName(channelID)
	sm.logger.Info("sent message", "channel_id", channelID, "channel_name", channelName, "timestamp", ts)

	return map[string]any{
		"channel_id":   channelID,
		"channel_name": channelName,
		"timestamp":    ts,
		"status":       "sent",
	}, nil
}

// SlackMessagePoster implements MessagePoster and ChannelResolver for Slack.
type SlackMessagePoster struct {
	client   *slack.Client
	logger   *slog.Logger
	mu       sync.RWMutex
	channels map[string]string // channel name → channel ID cache
}

// NewSlackMessagePoster creates a SlackMessagePoster.
func NewSlackMessagePoster(client *slack.Client, logger *slog.Logger) *SlackMessagePoster {
	return &SlackMessagePoster{
		client:   client,
		logger:   logger,
		channels: make(map[string]string),
	}
}

// PostMessage sends a message to a Slack channel using Block Kit.
func (p *SlackMessagePoster) PostMessage(ctx context.Context, channelID, text string, threadTS string) (string, error) {
	const maxBlockTextLen = 3000
	var blocks []slack.Block
	remaining := text
	for len(remaining) > 0 {
		chunk := remaining
		if len(chunk) > maxBlockTextLen {
			cut := strings.LastIndex(chunk[:maxBlockTextLen], "\n\n")
			if cut < maxBlockTextLen/2 {
				cut = strings.LastIndex(chunk[:maxBlockTextLen], "\n")
			}
			if cut < maxBlockTextLen/2 {
				cut = maxBlockTextLen
			}
			chunk = remaining[:cut]
			remaining = strings.TrimLeft(remaining[cut:], "\n")
		} else {
			remaining = ""
		}
		blocks = append(blocks, slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", chunk, false, false),
			nil, nil,
		))
	}

	fallback := text
	if len(fallback) > 300 {
		fallback = fallback[:297] + "..."
	}

	opts := []slack.MsgOption{
		slack.MsgOptionText(fallback, false),
		slack.MsgOptionBlocks(blocks...),
	}
	if threadTS != "" {
		opts = append(opts, slack.MsgOptionTS(threadTS))
	}

	_, ts, err := p.client.PostMessageContext(ctx, channelID, opts...)
	if err != nil {
		return "", fmt.Errorf("post message: %w", err)
	}
	return ts, nil
}

// ResolveChannel converts a channel name or ID to a channel ID.
func (p *SlackMessagePoster) ResolveChannel(channel string) (string, error) {
	if strings.HasPrefix(channel, "C") && len(channel) > 8 {
		return channel, nil
	}

	name := strings.TrimPrefix(channel, "#")

	p.mu.RLock()
	if id, ok := p.channels[name]; ok {
		p.mu.RUnlock()
		return id, nil
	}
	p.mu.RUnlock()

	channels, _, err := p.client.GetConversations(&slack.GetConversationsParameters{
		Types:           []string{"public_channel", "private_channel"},
		Limit:           1000,
		ExcludeArchived: true,
	})
	if err != nil {
		return "", fmt.Errorf("list channels: %w", err)
	}

	p.mu.Lock()
	for _, ch := range channels {
		p.channels[ch.Name] = ch.ID
	}
	p.mu.Unlock()

	p.mu.RLock()
	id, ok := p.channels[name]
	p.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("channel %q not found", name)
	}
	return id, nil
}

// ResolveChannelName performs a reverse lookup from channel ID to name.
func (p *SlackMessagePoster) ResolveChannelName(channelID string) string {
	p.mu.RLock()
	for name, id := range p.channels {
		if id == channelID {
			p.mu.RUnlock()
			return "#" + name
		}
	}
	p.mu.RUnlock()

	info, err := p.client.GetConversationInfoContext(context.Background(), &slack.GetConversationInfoInput{
		ChannelID: channelID,
	})
	if err != nil {
		return channelID
	}

	p.mu.Lock()
	p.channels[info.Name] = channelID
	p.mu.Unlock()

	return "#" + info.Name
}
