package discord

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"golang.org/x/time/rate"

	"github.com/xylolabsinc/xylolabs-kb/internal/bot"
	"github.com/xylolabsinc/xylolabs-kb/internal/extractor"
	"github.com/xylolabsinc/xylolabs-kb/internal/kb"
)

// Connector handles real-time and historical Discord message ingestion.
type Connector struct {
	session  *discordgo.Session
	guildID  string
	engine   *kb.Engine
	store    kb.Storage
	logger   *slog.Logger

	userCache   map[string]*discordgo.Member
	userCacheMu sync.RWMutex

	limiter *rate.Limiter

	botHandler bot.BotHandler
	extractor  *extractor.Extractor
	botUserID  string
}

// NewConnector creates a Discord connector.
func NewConnector(token, guildID string, engine *kb.Engine, store kb.Storage, logger *slog.Logger) (*Connector, error) {
	session, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, fmt.Errorf("create discord session: %w", err)
	}

	session.Identify.Intents = discordgo.IntentsGuildMessages |
		discordgo.IntentsDirectMessages |
		discordgo.IntentsMessageContent |
		discordgo.IntentsGuilds

	return &Connector{
		session:   session,
		guildID:   guildID,
		engine:    engine,
		store:     store,
		logger:    logger.With("component", "discord-connector"),
		userCache: make(map[string]*discordgo.Member),
		limiter:   rate.NewLimiter(rate.Limit(1), 1),
	}, nil
}

// Session returns the underlying discordgo session for use by the platform layer.
func (c *Connector) Session() *discordgo.Session {
	return c.session
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

// snowflakeLess returns true if a < b numerically. Falls back to string
// comparison if either value cannot be parsed as a uint64.
func snowflakeLess(a, b string) bool {
	ai, errA := strconv.ParseUint(a, 10, 64)
	bi, errB := strconv.ParseUint(b, 10, 64)
	if errA != nil || errB != nil {
		return a < b
	}
	return ai < bi
}

// Name returns the source identifier.
func (c *Connector) Name() kb.Source {
	return kb.SourceDiscord
}

// Start begins listening for real-time Discord events via the gateway.
func (c *Connector) Start(done <-chan struct{}) error {
	c.session.AddHandler(c.handleMessageCreate)

	if err := c.session.Open(); err != nil {
		return fmt.Errorf("open discord gateway: %w", err)
	}
	c.logger.Info("discord gateway connected", "guild_id", c.guildID)

	<-done
	return c.session.Close()
}

// Stop gracefully shuts down the connector.
func (c *Connector) Stop() error {
	c.logger.Info("discord connector stopped")
	return nil
}

// Sync performs a historical backfill of all text channels.
func (c *Connector) Sync(ctx context.Context) error {
	c.logger.Info("starting discord sync")

	syncState, err := c.store.GetSyncState(kb.SourceDiscord)
	if err != nil {
		return fmt.Errorf("get discord sync state: %w", err)
	}

	var afterID string
	if syncState != nil && syncState.Cursor != "" {
		afterID = syncState.Cursor
	}

	channels, err := c.session.GuildChannels(c.guildID)
	if err != nil {
		return fmt.Errorf("list guild channels: %w", err)
	}

	c.logger.Info("syncing discord channels", "count", len(channels))

	var totalMessages int
	var latestMsgID string
	for _, ch := range channels {
		// Only sync text channels
		if ch.Type != discordgo.ChannelTypeGuildText {
			continue
		}

		count, lastID, err := c.syncChannel(ctx, ch, afterID)
		if err != nil {
			c.logger.Warn("failed to sync discord channel", "channel", ch.Name, "error", err)
			continue
		}
		totalMessages += count
		if snowflakeLess(latestMsgID, lastID) {
			latestMsgID = lastID
		}
	}

	if latestMsgID != "" {
		now := time.Now().UTC()
		newState := kb.SyncState{
			Source:     kb.SourceDiscord,
			LastSyncAt: now,
			Cursor:     latestMsgID,
			Metadata:   map[string]string{"channels_synced": fmt.Sprintf("%d", len(channels))},
		}
		if err := c.store.SetSyncState(newState); err != nil {
			return fmt.Errorf("set sync state: %w", err)
		}
	}

	c.logger.Info("discord sync complete", "messages", totalMessages)
	return nil
}

func (c *Connector) syncChannel(ctx context.Context, ch *discordgo.Channel, afterID string) (int, string, error) {
	var count int
	var lastID string
	cursor := afterID

	for {
		if err := c.waitRateLimit(ctx); err != nil {
			return count, lastID, fmt.Errorf("rate limit: %w", err)
		}

		msgs, err := c.session.ChannelMessages(ch.ID, 100, "", cursor, "")
		if err != nil {
			return count, lastID, fmt.Errorf("get messages for %s: %w", ch.Name, err)
		}

		if len(msgs) == 0 {
			break
		}

		for _, msg := range msgs {
			if msg.Author != nil && msg.Author.Bot {
				continue
			}

			doc := ConvertMessage(msg, ch.Name, c.guildID)
			if c.extractor != nil {
				EnrichDocumentContent(ctx, &doc, msg.Attachments, c.extractor)
			}
			if err := c.engine.Index(ctx, doc); err != nil {
				c.logger.Warn("failed to index discord message",
					"channel", ch.Name,
					"msg_id", msg.ID,
					"error", err,
				)
				continue
			}
			count++
			if snowflakeLess(lastID, msg.ID) {
				lastID = msg.ID
			}
		}

		// Discord returns newest first; last in slice is oldest
		cursor = msgs[len(msgs)-1].ID
		if len(msgs) < 100 {
			break
		}
	}

	return count, lastID, nil
}

func (c *Connector) handleMessageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	// Ignore bot messages
	if m.Author == nil || m.Author.Bot {
		return
	}

	ctx := context.Background()

	// Bot routing
	if c.botHandler != nil && c.botUserID != "" {
		isDM := m.GuildID == ""
		isMention := strings.Contains(m.Content, "<@"+c.botUserID+">") ||
			strings.Contains(m.Content, "<@!"+c.botUserID+">")

		if isDM {
			incoming := discordMsgToIncoming(m.Message, true)
			go c.botHandler.HandleDirectMessage(ctx, incoming)
			return // Don't index DMs
		}

		if isMention {
			incoming := discordMsgToIncoming(m.Message, false)
			go c.botHandler.HandleMention(ctx, incoming)
			// Fall through to index
		} else if m.MessageReference != nil && c.botHandler.IsTrackedThread(m.ChannelID, m.MessageReference.MessageID) {
			incoming := discordMsgToIncoming(m.Message, false)
			go c.botHandler.HandleDirectMessage(ctx, incoming)
			// Fall through to index
		}
	}

	// Verify this is from our guild
	if m.GuildID != c.guildID {
		return
	}

	// Get channel name
	channelName := ""
	ch, err := s.Channel(m.ChannelID)
	if err == nil {
		channelName = ch.Name
	}

	doc := ConvertMessage(m.Message, channelName, c.guildID)
	if c.extractor != nil {
		EnrichDocumentContent(ctx, &doc, m.Attachments, c.extractor)
	}
	if err := c.engine.Index(ctx, doc); err != nil {
		c.logger.Warn("failed to index real-time discord message",
			"channel", m.ChannelID,
			"error", err,
		)
	}
}

func discordMsgToIncoming(m *discordgo.Message, isDM bool) *bot.IncomingMessage {
	msg := &bot.IncomingMessage{
		Platform:  "discord",
		Channel:   m.ChannelID,
		MessageID: m.ID,
		Text:      m.Content,
		IsDM:      isDM,
	}
	if m.Author != nil {
		msg.UserID = m.Author.ID
	}
	// Set ThreadID from MessageReference (reply) or thread channel
	if m.MessageReference != nil {
		msg.ThreadID = m.MessageReference.MessageID
	}
	// Convert attachments
	for _, att := range m.Attachments {
		msg.Files = append(msg.Files, bot.FileAttachment{
			ID:       att.ID,
			Name:     att.Filename,
			MimeType: att.ContentType,
			URL:      att.URL,
			Size:     att.Size,
		})
	}
	return msg
}
