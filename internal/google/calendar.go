package google

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"golang.org/x/time/rate"
	"google.golang.org/api/calendar/v3"

	"github.com/xylolabsinc/xylolabs-kb/internal/kb"
)

// CalendarClient handles Google Calendar event operations.
type CalendarClient struct {
	service *calendar.Service
	logger  *slog.Logger
	limiter *rate.Limiter
}

func (c *CalendarClient) waitRateLimit(ctx context.Context) error {
	return c.limiter.Wait(ctx)
}

// SyncEvents lists and indexes events from all accessible calendars.
func (c *CalendarClient) SyncEvents(ctx context.Context, engine *kb.Engine) (int, error) {
	// List all calendars
	if err := c.waitRateLimit(ctx); err != nil {
		return 0, fmt.Errorf("rate limit: %w", err)
	}
	calList, err := c.service.CalendarList.List().Context(ctx).Do()
	if err != nil {
		return 0, fmt.Errorf("list calendars: %w", err)
	}

	c.logger.Info("syncing google calendars", "count", len(calList.Items))

	var totalCount int
	// Sync events from the past 90 days and upcoming 90 days
	timeMin := time.Now().AddDate(0, 0, -90).Format(time.RFC3339)
	timeMax := time.Now().AddDate(0, 0, 90).Format(time.RFC3339)

	for _, cal := range calList.Items {
		count, err := c.syncCalendar(ctx, engine, cal, timeMin, timeMax)
		if err != nil {
			c.logger.Warn("failed to sync calendar", "calendar", cal.Summary, "error", err)
			continue
		}
		totalCount += count
	}

	c.logger.Info("calendar sync complete", "events", totalCount)
	return totalCount, nil
}

func (c *CalendarClient) syncCalendar(ctx context.Context, engine *kb.Engine, cal *calendar.CalendarListEntry, timeMin, timeMax string) (int, error) {
	var count int
	pageToken := ""

	for {
		if err := c.waitRateLimit(ctx); err != nil {
			return count, fmt.Errorf("rate limit: %w", err)
		}

		req := c.service.Events.List(cal.Id).
			Context(ctx).
			TimeMin(timeMin).
			TimeMax(timeMax).
			SingleEvents(true).
			OrderBy("startTime").
			MaxResults(250)

		if pageToken != "" {
			req = req.PageToken(pageToken)
		}

		events, err := req.Do()
		if err != nil {
			return count, fmt.Errorf("list events for %s: %w", cal.Summary, err)
		}

		for _, event := range events.Items {
			if event.Status == "cancelled" {
				continue
			}
			doc := convertCalendarEvent(event, cal.Summary)
			if err := engine.Index(ctx, doc); err != nil {
				c.logger.Warn("failed to index calendar event",
					"event", event.Summary,
					"error", err,
				)
				continue
			}
			count++
		}

		if events.NextPageToken == "" {
			break
		}
		pageToken = events.NextPageToken
	}

	return count, nil
}

func convertCalendarEvent(event *calendar.Event, calendarName string) kb.Document {
	var startTime, endTime time.Time
	if event.Start != nil {
		if event.Start.DateTime != "" {
			startTime, _ = time.Parse(time.RFC3339, event.Start.DateTime)
		} else if event.Start.Date != "" {
			startTime, _ = time.Parse("2006-01-02", event.Start.Date)
		}
	}
	if event.End != nil {
		if event.End.DateTime != "" {
			endTime, _ = time.Parse(time.RFC3339, event.End.DateTime)
		} else if event.End.Date != "" {
			endTime, _ = time.Parse("2006-01-02", event.End.Date)
		}
	}

	// Build content from event details
	var parts []string
	parts = append(parts, fmt.Sprintf("Event: %s", event.Summary))

	if !startTime.IsZero() {
		if !endTime.IsZero() {
			parts = append(parts, fmt.Sprintf("When: %s — %s", startTime.Format("2006-01-02 15:04"), endTime.Format("2006-01-02 15:04")))
		} else {
			parts = append(parts, fmt.Sprintf("When: %s", startTime.Format("2006-01-02 15:04")))
		}
	}

	if event.Location != "" {
		parts = append(parts, fmt.Sprintf("Location: %s", event.Location))
	}

	if len(event.Attendees) > 0 {
		var attendees []string
		for _, a := range event.Attendees {
			name := a.DisplayName
			if name == "" {
				name = a.Email
			}
			attendees = append(attendees, name)
		}
		parts = append(parts, fmt.Sprintf("Attendees: %s", strings.Join(attendees, ", ")))
	}

	if event.Description != "" {
		parts = append(parts, fmt.Sprintf("\n%s", event.Description))
	}

	content := strings.Join(parts, "\n")

	var author string
	if event.Creator != nil {
		author = event.Creator.DisplayName
		if author == "" {
			author = event.Creator.Email
		}
	}

	var authorEmail string
	if event.Organizer != nil {
		authorEmail = event.Organizer.Email
	}

	return kb.Document{
		Source:      kb.SourceGoogle,
		SourceID:    "cal-" + event.Id,
		Title:       event.Summary,
		Content:     content,
		ContentType: "calendar_event",
		Author:      author,
		AuthorEmail: authorEmail,
		Channel:     calendarName,
		URL:         event.HtmlLink,
		Timestamp:   startTime,
		UpdatedAt:   parseGoogleTime(event.Updated),
		Metadata: map[string]string{
			"calendar":   calendarName,
			"event_type": "calendar_event",
			"location":   event.Location,
			"status":     event.Status,
		},
	}
}
