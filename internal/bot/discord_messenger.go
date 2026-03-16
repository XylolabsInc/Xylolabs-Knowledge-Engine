package bot

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/bwmarrin/discordgo"
)

// DiscordMessagePoster implements tools.MessagePoster and tools.ChannelResolver for Discord.
type DiscordMessagePoster struct {
	session  *discordgo.Session
	guildID  string
	logger   *slog.Logger
	mu       sync.RWMutex
	channels map[string]string // channel name -> channel ID cache
}

// NewDiscordMessagePoster creates a DiscordMessagePoster.
func NewDiscordMessagePoster(session *discordgo.Session, guildID string, logger *slog.Logger) *DiscordMessagePoster {
	return &DiscordMessagePoster{
		session:  session,
		guildID:  guildID,
		logger:   logger,
		channels: make(map[string]string),
	}
}

// PostMessage sends a message to a Discord channel. Long messages are split at the 2000-char limit.
// Returns the message ID (snowflake) as the timestamp.
func (p *DiscordMessagePoster) PostMessage(ctx context.Context, channelID, text string, threadTS string) (string, error) {
	targetChannel := channelID
	if threadTS != "" {
		targetChannel = threadTS
	}

	chunks := splitDiscordMessage(text)
	var lastMsgID string
	for _, chunk := range chunks {
		msg, err := p.session.ChannelMessageSend(targetChannel, chunk)
		if err != nil {
			return "", fmt.Errorf("post message: %w", err)
		}
		lastMsgID = msg.ID
	}
	return lastMsgID, nil
}

// ResolveChannel converts a channel name or ID to a channel ID.
// If the input looks like a numeric snowflake ID, it is returned as-is.
func (p *DiscordMessagePoster) ResolveChannel(channel string) (string, error) {
	// Discord channel IDs are numeric snowflakes.
	if isNumericID(channel) {
		return channel, nil
	}

	name := strings.TrimPrefix(channel, "#")

	p.mu.RLock()
	if id, ok := p.channels[name]; ok {
		p.mu.RUnlock()
		return id, nil
	}
	p.mu.RUnlock()

	// Fetch guild channels and cache them.
	guildChannels, err := p.session.GuildChannels(p.guildID)
	if err != nil {
		return "", fmt.Errorf("list guild channels: %w", err)
	}

	p.mu.Lock()
	for _, ch := range guildChannels {
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

// ResolveChannelName performs a reverse lookup from channel ID to display name.
func (p *DiscordMessagePoster) ResolveChannelName(channelID string) string {
	p.mu.RLock()
	for name, id := range p.channels {
		if id == channelID {
			p.mu.RUnlock()
			return "#" + name
		}
	}
	p.mu.RUnlock()

	ch, err := p.session.Channel(channelID)
	if err != nil {
		return channelID
	}

	p.mu.Lock()
	p.channels[ch.Name] = channelID
	p.mu.Unlock()

	return "#" + ch.Name
}

// isNumericID returns true if s consists entirely of digits (a Discord snowflake).
func isNumericID(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
