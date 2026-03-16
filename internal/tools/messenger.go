package tools

import "context"

// MessagePoster abstracts sending messages to a channel. Implemented by
// SlackMessagePoster (Slack Block Kit) and DiscordMessagePoster.
type MessagePoster interface {
	PostMessage(ctx context.Context, channelID, text string, threadTS string) (timestamp string, err error)
}

// ChannelResolver abstracts channel name ↔ ID resolution.
type ChannelResolver interface {
	ResolveChannel(channel string) (channelID string, err error)
	ResolveChannelName(channelID string) string
}
