package discord

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/xylolabsinc/xylolabs-kb/internal/extractor"
	"github.com/xylolabsinc/xylolabs-kb/internal/kb"
)

var (
	discordUserMentionRe = regexp.MustCompile(`<@!?(\d+)>`)
	discordChannelRe     = regexp.MustCompile(`<#(\d+)>`)
	discordURLRe         = regexp.MustCompile(`https?://[^\s<>"]+`)
	discordHTTPClient    = extractor.NewRestrictedHTTPClient(30 * time.Second)
)

// ConvertMessage converts a Discord message to a KB document.
func ConvertMessage(msg *discordgo.Message, channelName, guildID string) kb.Document {
	author := ""
	if msg.Author != nil {
		author = msg.Author.GlobalName
		if author == "" {
			author = msg.Author.Username
		}
	}

	content := cleanDiscordText(msg.Content)
	ts := time.Time(msg.Timestamp)

	sourceID := fmt.Sprintf("%s-%s", msg.ChannelID, msg.ID)
	url := fmt.Sprintf("https://discord.com/channels/%s/%s/%s", guildID, msg.ChannelID, msg.ID)

	doc := kb.Document{
		Source:      kb.SourceDiscord,
		SourceID:    sourceID,
		Content:     content,
		ContentType: "message",
		Author:      author,
		Channel:     channelName,
		URL:         url,
		Timestamp:   ts,
		UpdatedAt:   ts,
		Metadata: map[string]string{
			"channel_id": msg.ChannelID,
			"msg_id":     msg.ID,
			"guild_id":   guildID,
		},
	}

	// Set parent for replies
	if msg.MessageReference != nil {
		doc.ParentID = fmt.Sprintf("%s-%s", msg.MessageReference.ChannelID, msg.MessageReference.MessageID)
	}

	// Attachments
	for _, att := range msg.Attachments {
		doc.Attachments = append(doc.Attachments, kb.Attachment{
			Filename:  att.Filename,
			MimeType:  att.ContentType,
			Size:      int64(att.Size),
			SourceURL: att.URL,
		})
	}

	return doc
}

func cleanDiscordText(text string) string {
	text = discordUserMentionRe.ReplaceAllString(text, "@$1")
	text = discordChannelRe.ReplaceAllString(text, "#$1")
	return strings.TrimSpace(text)
}

// EnrichDocumentContent extracts text from file attachments and URLs in the message.
func EnrichDocumentContent(ctx context.Context, doc *kb.Document, attachments []*discordgo.MessageAttachment, ext *extractor.Extractor) {
	if ext == nil {
		return
	}

	for _, att := range attachments {
		if att.URL == "" {
			continue
		}

		data, err := downloadDiscordFile(ctx, discordHTTPClient, att.URL)
		if err != nil {
			continue
		}

		mimeType := att.ContentType
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}

		result, err := ext.ExtractFromBytes(ctx, data, mimeType, att.Filename)
		if err != nil {
			continue
		}

		if result.Text != "" {
			doc.Content += "\n\n---\nAttached: " + att.Filename + "\n" + result.Text
		}
	}

	urls := discordURLRe.FindAllString(doc.Content, -1)
	seen := make(map[string]bool)
	for _, u := range urls {
		if seen[u] {
			continue
		}
		seen[u] = true
		result, err := ext.ExtractFromURL(ctx, u)
		if err != nil {
			continue
		}
		if result.Text != "" {
			text := result.Text
			if len(text) > 5000 {
				text = text[:5000] + "..."
			}
			doc.Content += "\n\n---\nLink: " + u + "\n" + text
		}
	}
}

func downloadDiscordFile(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("download discord file: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download discord file: %w", err)
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024))
}
