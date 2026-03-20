package slack

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
	"golang.org/x/time/rate"

	"github.com/xylolabsinc/xylolabs-kb/internal/bot"
	"github.com/xylolabsinc/xylolabs-kb/internal/extractor"
	"github.com/xylolabsinc/xylolabs-kb/internal/kb"
)

// Connector handles real-time and historical Slack message ingestion.
type Connector struct {
	client       *slack.Client
	socketClient *socketmode.Client
	engine       *kb.Engine
	store        kb.Storage
	logger       *slog.Logger

	userCache   map[string]*slack.User
	userCacheMu sync.RWMutex

	channelCache   map[string]string // channel ID → channel name
	channelCacheMu sync.RWMutex

	botToken string
	appToken string

	limiter *rate.Limiter

	botHandler bot.BotHandler      // may be nil if Gemini not configured
	extractor *extractor.Extractor // may be nil
	botUserID string
}

// NewConnector creates a Slack connector.
func NewConnector(botToken, appToken string, engine *kb.Engine, store kb.Storage, logger *slog.Logger) *Connector {
	client := slack.New(
		botToken,
		slack.OptionAppLevelToken(appToken),
	)
	socketClient := socketmode.New(
		client,
		socketmode.OptionLog(slog.NewLogLogger(logger.Handler(), slog.LevelDebug)),
	)

	return &Connector{
		client:       client,
		socketClient: socketClient,
		engine:       engine,
		store:        store,
		logger:       logger.With("component", "slack-connector"),
		userCache:    make(map[string]*slack.User),
		channelCache: make(map[string]string),
		botToken:     botToken,
		appToken:     appToken,
		limiter:      rate.NewLimiter(rate.Limit(1), 1),
	}
}

// SetBot sets the bot handler for mention/DM processing.
func (c *Connector) SetBot(handler bot.BotHandler, botUserID string) {
	c.botHandler = handler
	c.botUserID = botUserID
}

// SetExtractor sets the content extractor for file processing.
func (c *Connector) SetExtractor(ext *extractor.Extractor) {
	c.extractor = ext
}

func (c *Connector) waitRateLimit(ctx context.Context) error {
	return c.limiter.Wait(ctx)
}

// Name returns the source identifier.
func (c *Connector) Name() kb.Source {
	return kb.SourceSlack
}

// Start begins listening for real-time Slack events via Socket Mode.
func (c *Connector) Start(done <-chan struct{}) error {
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		<-done
		cancel()
	}()

	go c.handleEvents(ctx)

	c.logger.Info("starting slack socket mode")
	return c.socketClient.RunContext(ctx)
}

// Stop gracefully shuts down the connector.
func (c *Connector) Stop() error {
	c.logger.Info("slack connector stopped")
	return nil
}

// Sync performs a historical backfill of all channels.
func (c *Connector) Sync() error {
	ctx := context.Background()
	c.logger.Info("starting slack sync")

	syncState, err := c.store.GetSyncState(kb.SourceSlack)
	if err != nil {
		return fmt.Errorf("get slack sync state: %w", err)
	}

	var oldest string
	if syncState != nil && syncState.Cursor != "" {
		oldest = syncState.Cursor
	}

	channels, err := c.listAllChannels(ctx)
	if err != nil {
		return fmt.Errorf("list channels: %w", err)
	}

	// Auto-join all public channels the bot isn't in yet
	c.joinAllChannels(ctx, channels)

	c.logger.Info("syncing slack channels", "count", len(channels))

	// Pre-populate channel name cache from sync data
	c.channelCacheMu.Lock()
	for _, ch := range channels {
		c.channelCache[ch.ID] = ch.Name
	}
	c.channelCacheMu.Unlock()

	var totalMessages int
	for _, ch := range channels {
		count, err := c.syncChannel(ctx, ch, oldest)
		if err != nil {
			c.logger.Warn("failed to sync channel", "channel", ch.Name, "error", err)
			continue
		}
		totalMessages += count
	}

	// Update sync state
	now := time.Now().UTC()
	newState := kb.SyncState{
		Source:     kb.SourceSlack,
		LastSyncAt: now,
		Cursor:     fmt.Sprintf("%d", now.Unix()),
		Metadata:   map[string]string{"channels_synced": fmt.Sprintf("%d", len(channels))},
	}
	if err := c.store.SetSyncState(newState); err != nil {
		return fmt.Errorf("set sync state: %w", err)
	}

	c.logger.Info("slack sync complete", "messages", totalMessages, "channels", len(channels))
	return nil
}

func (c *Connector) handleEvents(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-c.socketClient.Events:
			if !ok {
				return
			}
			c.processEvent(ctx, evt)
		}
	}
}

func (c *Connector) processEvent(ctx context.Context, evt socketmode.Event) {
	c.logger.Debug("socket event received", "type", evt.Type)

	switch evt.Type {
	case socketmode.EventTypeEventsAPI:
		eventsAPIEvent, ok := evt.Data.(slackevents.EventsAPIEvent)
		if !ok {
			c.logger.Warn("failed to cast events API event")
			return
		}
		c.socketClient.Ack(*evt.Request)
		c.logger.Debug("events API event", "event_type", eventsAPIEvent.Type, "inner_type", eventsAPIEvent.InnerEvent.Type)
		c.handleEventsAPI(ctx, eventsAPIEvent)
	case socketmode.EventTypeConnecting:
		c.logger.Debug("slack connecting")
	case socketmode.EventTypeConnected:
		c.logger.Info("slack connected")
	case socketmode.EventTypeConnectionError:
		c.logger.Warn("slack connection error")
	default:
		c.logger.Debug("unhandled socket event", "type", evt.Type)
		if evt.Request != nil && evt.Request.EnvelopeID != "" {
			c.socketClient.Ack(*evt.Request)
		}
	}
}

func (c *Connector) handleEventsAPI(ctx context.Context, event slackevents.EventsAPIEvent) {
	switch event.Type {
	case slackevents.CallbackEvent:
		innerEvent := event.InnerEvent
		switch ev := innerEvent.Data.(type) {
		case *slackevents.ChannelCreatedEvent:
			// Auto-join newly created public channels
			c.logger.Info("new channel created, auto-joining", "channel", ev.Channel.Name, "channel_id", ev.Channel.ID)
			go func() {
				if err := c.waitRateLimit(ctx); err != nil {
					return
				}
				if _, _, _, err := c.client.JoinConversationContext(ctx, ev.Channel.ID); err != nil {
					c.logger.Warn("failed to auto-join new channel", "channel", ev.Channel.Name, "error", err)
				} else {
					c.logger.Info("auto-joined new channel", "channel", ev.Channel.Name)
				}
			}()
		case *slackevents.MessageEvent:
			if ev.BotID != "" || ev.SubType == "bot_message" {
				return
			}

			// Bot routing: check for mentions, DMs, and tracked thread replies
			if c.botHandler != nil {
				// Skip bot's own messages
				if ev.User == c.botUserID {
					return
				}
				// Direct messages (DM channels start with "D")
				if strings.HasPrefix(ev.Channel, "D") {
					incoming := slackEventToIncomingMessage(ev)
					go c.botHandler.HandleDirectMessage(ctx, incoming)
					return // Don't index DMs to the bot
				}
				// @mentions
				if strings.Contains(ev.Text, "<@"+c.botUserID+">") {
					incoming := slackEventToIncomingMessage(ev)
					go c.botHandler.HandleMention(ctx, incoming)
					// Fall through to still index the message
				} else if ev.ThreadTimeStamp != "" && c.botHandler.IsTrackedThread(ev.Channel, ev.ThreadTimeStamp) {
					// Thread reply in a conversation the bot is participating in
					incoming := slackEventToIncomingMessage(ev)
					go c.botHandler.HandleDirectMessage(ctx, incoming)
					// Fall through to still index the message
				}
			}

			user := c.resolveUser(ctx, ev.User)
			channelName := c.resolveChannelName(ctx, ev.Channel)
			doc := ConvertMessage(ev, user, channelName)
			// Extract content from file attachments and URLs
			if c.extractor != nil && ev.Message != nil {
				EnrichDocumentContent(ctx, &doc, ev.Message.Files, c.extractor, c.botToken)
			}
			if err := c.engine.Index(ctx, doc); err != nil {
				c.logger.Warn("failed to index real-time message",
					"channel", ev.Channel,
					"error", err,
				)
			}
		}
	}
}

// slackEventToIncomingMessage converts a Slack event to a platform-agnostic IncomingMessage.
func slackEventToIncomingMessage(ev *slackevents.MessageEvent) *bot.IncomingMessage {
	msg := &bot.IncomingMessage{
		Platform:  "slack",
		Channel:   ev.Channel,
		MessageID: ev.TimeStamp,
		UserID:    ev.User,
		Text:      ev.Text,
		IsDM:      strings.HasPrefix(ev.Channel, "D"),
	}
	// Set ThreadID
	if ev.ThreadTimeStamp != "" {
		msg.ThreadID = ev.ThreadTimeStamp
	}
	// Convert file attachments
	if ev.Message != nil {
		for _, f := range ev.Message.Files {
			url := f.URLPrivateDownload
			if url == "" {
				url = f.URLPrivate
			}
			msg.Files = append(msg.Files, bot.FileAttachment{
				ID:       f.ID,
				Name:     f.Name,
				MimeType: f.Mimetype,
				URL:      url,
				Size:     f.Size,
			})
		}
	}
	return msg
}

func (c *Connector) listAllChannels(ctx context.Context) ([]slack.Channel, error) {
	var allChannels []slack.Channel
	cursor := ""
	for {
		params := &slack.GetConversationsParameters{
			Types:           []string{"public_channel", "private_channel"},
			Limit:           200,
			Cursor:          cursor,
			ExcludeArchived: true,
		}
		if err := c.waitRateLimit(ctx); err != nil {
			return nil, fmt.Errorf("rate limit: %w", err)
		}
		channels, nextCursor, err := c.client.GetConversationsContext(ctx, params)
		if err != nil {
			return nil, fmt.Errorf("get conversations: %w", err)
		}
		allChannels = append(allChannels, channels...)
		if nextCursor == "" {
			break
		}
		cursor = nextCursor
	}
	return allChannels, nil
}

func (c *Connector) syncChannel(ctx context.Context, ch slack.Channel, oldest string) (int, error) {
	var count int
	cursor := ""

	for {
		params := &slack.GetConversationHistoryParameters{
			ChannelID: ch.ID,
			Limit:     200,
			Oldest:    oldest,
			Cursor:    cursor,
		}
		if err := c.waitRateLimit(ctx); err != nil {
			return count, fmt.Errorf("rate limit: %w", err)
		}
		history, err := c.client.GetConversationHistoryContext(ctx, params)
		if err != nil {
			return count, fmt.Errorf("get history for %s: %w", ch.Name, err)
		}

		for _, msg := range history.Messages {
			if msg.BotID != "" || msg.SubType == "bot_message" {
				continue
			}
			user := c.resolveUser(ctx, msg.User)
			doc := ConvertHistoryMessage(&msg, user, ch.ID, ch.Name)
			// Extract content from file attachments and URLs
			if c.extractor != nil {
				EnrichDocumentContent(ctx, &doc, msg.Files, c.extractor, c.botToken)
			}
			if err := c.engine.Index(ctx, doc); err != nil {
				c.logger.Warn("failed to index message",
					"channel", ch.Name,
					"ts", msg.Timestamp,
					"error", err,
				)
				continue
			}
			count++

			// Fetch thread replies if any
			if msg.ReplyCount > 0 {
				threadCount, err := c.syncThread(ctx, ch, msg.Timestamp)
				if err != nil {
					c.logger.Warn("failed to sync thread",
						"channel", ch.Name,
						"thread_ts", msg.Timestamp,
						"error", err,
					)
				}
				count += threadCount
			}
		}

		if !history.HasMore {
			break
		}
		cursor = history.ResponseMetaData.NextCursor
	}

	return count, nil
}

func (c *Connector) syncThread(ctx context.Context, ch slack.Channel, threadTS string) (int, error) {
	var count int
	cursor := ""

	for {
		if err := c.waitRateLimit(ctx); err != nil {
			return count, fmt.Errorf("rate limit: %w", err)
		}
		msgs, hasMore, nextCursor, err := c.client.GetConversationRepliesContext(
			ctx,
			&slack.GetConversationRepliesParameters{
				ChannelID: ch.ID,
				Timestamp: threadTS,
				Limit:     200,
				Cursor:    cursor,
			},
		)
		if err != nil {
			return count, fmt.Errorf("get replies: %w", err)
		}

		for _, msg := range msgs {
			if msg.Timestamp == threadTS {
				continue // skip parent
			}
			user := c.resolveUser(ctx, msg.User)
			doc := ConvertThreadMessage(&msg, user, ch.ID, ch.Name, threadTS)
			// Extract content from file attachments and URLs
			if c.extractor != nil {
				EnrichDocumentContent(ctx, &doc, msg.Files, c.extractor, c.botToken)
			}
			if err := c.engine.Index(ctx, doc); err != nil {
				c.logger.Warn("failed to index thread reply", "error", err)
				continue
			}
			count++
		}

		if !hasMore {
			break
		}
		cursor = nextCursor
	}

	return count, nil
}

// joinAllChannels joins all public channels the bot is not already a member of.
func (c *Connector) joinAllChannels(ctx context.Context, channels []slack.Channel) {
	var joined int
	for _, ch := range channels {
		if ch.IsMember {
			continue
		}
		// Only join public channels (skip private)
		if ch.IsPrivate {
			continue
		}
		if err := c.waitRateLimit(ctx); err != nil {
			c.logger.Warn("rate limit during channel join", "error", err)
			return
		}
		if _, _, _, err := c.client.JoinConversationContext(ctx, ch.ID); err != nil {
			c.logger.Debug("failed to join channel", "channel", ch.Name, "error", err)
			continue
		}
		joined++
		c.logger.Info("auto-joined channel", "channel", ch.Name)
	}
	if joined > 0 {
		c.logger.Info("auto-joined public channels", "count", joined)
	}
}

func (c *Connector) resolveUser(ctx context.Context, userID string) *slack.User {
	if userID == "" {
		return nil
	}

	c.userCacheMu.RLock()
	user, ok := c.userCache[userID]
	c.userCacheMu.RUnlock()
	if ok {
		return user
	}

	if err := c.waitRateLimit(ctx); err != nil {
		c.logger.Debug("rate limit wait failed", "user_id", userID, "error", err)
		return nil
	}
	info, err := c.client.GetUserInfoContext(ctx, userID)
	if err != nil {
		c.logger.Debug("failed to resolve user", "user_id", userID, "error", err)
		return nil
	}

	c.userCacheMu.Lock()
	c.userCache[userID] = info
	c.userCacheMu.Unlock()

	return info
}

// resolveChannelName resolves a channel ID to its name, using a cache.
func (c *Connector) resolveChannelName(ctx context.Context, channelID string) string {
	if channelID == "" {
		return ""
	}

	c.channelCacheMu.RLock()
	name, ok := c.channelCache[channelID]
	c.channelCacheMu.RUnlock()
	if ok {
		return name
	}

	if err := c.waitRateLimit(ctx); err != nil {
		c.logger.Debug("rate limit wait failed", "channel_id", channelID, "error", err)
		return channelID
	}
	info, err := c.client.GetConversationInfoContext(ctx, &slack.GetConversationInfoInput{
		ChannelID: channelID,
	})
	if err != nil {
		c.logger.Debug("failed to resolve channel name", "channel_id", channelID, "error", err)
		return channelID
	}

	c.channelCacheMu.Lock()
	c.channelCache[channelID] = info.Name
	c.channelCacheMu.Unlock()

	return info.Name
}
