package slack

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	goslack "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"

	"github.com/xylolabsinc/xylolabs-kb/internal/extractor"
	"github.com/xylolabsinc/xylolabs-kb/internal/kb"
)

var (
	userMentionRe = regexp.MustCompile(`<@([A-Z0-9]+)>`)
	channelRe     = regexp.MustCompile(`<#([A-Z0-9]+)\|([^>]+)>`)
	urlRe         = regexp.MustCompile(`<(https?://[^|>]+)(?:\|([^>]+))?>`)
)

// ConvertMessage converts a real-time Slack event message to a KB document.
func ConvertMessage(ev *slackevents.MessageEvent, user *goslack.User, channelName string) kb.Document {
	author, authorEmail := extractUserInfo(user, ev.User)
	content := cleanMessageText(ev.Text)
	ts := parseSlackTimestamp(ev.TimeStamp)

	sourceID := fmt.Sprintf("%s-%s", ev.Channel, ev.TimeStamp)

	doc := kb.Document{
		Source:      kb.SourceSlack,
		SourceID:    sourceID,
		Content:     content,
		ContentType: "message",
		Author:      author,
		AuthorEmail: authorEmail,
		Channel:     channelName,
		Timestamp:   ts,
		UpdatedAt:   ts,
		Metadata: map[string]string{
			"channel_id": ev.Channel,
			"ts":         ev.TimeStamp,
		},
	}

	if ev.ThreadTimeStamp != "" && ev.ThreadTimeStamp != ev.TimeStamp {
		doc.ParentID = fmt.Sprintf("%s-%s", ev.Channel, ev.ThreadTimeStamp)
	}

	// File attachments are available via ev.Message (populated by custom unmarshaller).
	if ev.Message != nil {
		for _, f := range ev.Message.Files {
			url := f.URLPrivateDownload
			if url == "" {
				url = f.URLPrivate
			}
			doc.Attachments = append(doc.Attachments, kb.Attachment{
				Filename:  f.Name,
				MimeType:  f.Mimetype,
				Size:      int64(f.Size),
				SourceURL: url,
			})
		}
	}

	return doc
}

// ConvertHistoryMessage converts a historical Slack message to a KB document.
func ConvertHistoryMessage(msg *goslack.Message, user *goslack.User, channelID, channelName string) kb.Document {
	author, authorEmail := extractUserInfo(user, msg.User)
	content := cleanMessageText(msg.Text)
	ts := parseSlackTimestamp(msg.Timestamp)

	sourceID := fmt.Sprintf("%s-%s", channelID, msg.Timestamp)

	doc := kb.Document{
		Source:      kb.SourceSlack,
		SourceID:    sourceID,
		Content:     content,
		ContentType: "message",
		Author:      author,
		AuthorEmail: authorEmail,
		Channel:     channelName,
		URL:         fmt.Sprintf("https://slack.com/archives/%s/p%s", channelID, strings.Replace(msg.Timestamp, ".", "", 1)),
		Timestamp:   ts,
		UpdatedAt:   ts,
		Metadata: map[string]string{
			"channel_id": channelID,
			"ts":         msg.Timestamp,
		},
	}

	for _, f := range msg.Files {
		url := f.URLPrivateDownload
		if url == "" {
			url = f.URLPrivate
		}
		doc.Attachments = append(doc.Attachments, kb.Attachment{
			Filename:  f.Name,
			MimeType:  f.Mimetype,
			Size:      int64(f.Size),
			SourceURL: url,
		})
	}

	return doc
}

// ConvertThreadMessage converts a thread reply to a KB document.
func ConvertThreadMessage(msg *goslack.Message, user *goslack.User, channelID, channelName, threadTS string) kb.Document {
	doc := ConvertHistoryMessage(msg, user, channelID, channelName)
	doc.ParentID = fmt.Sprintf("%s-%s", channelID, threadTS)
	doc.Metadata["thread_ts"] = threadTS
	return doc
}

func extractUserInfo(user *goslack.User, fallbackID string) (name, email string) {
	if user != nil {
		name = user.RealName
		if name == "" {
			name = user.Name
		}
		email = user.Profile.Email
		return name, email
	}
	return fallbackID, ""
}

func cleanMessageText(text string) string {
	// Replace user mentions with readable format
	text = userMentionRe.ReplaceAllString(text, "@$1")

	// Replace channel references
	text = channelRe.ReplaceAllString(text, "#$2")

	// Replace URLs — keep label if present, otherwise keep URL
	text = urlRe.ReplaceAllStringFunc(text, func(match string) string {
		parts := urlRe.FindStringSubmatch(match)
		if len(parts) >= 3 && parts[2] != "" {
			return parts[2]
		}
		if len(parts) >= 2 {
			return parts[1]
		}
		return match
	})

	return strings.TrimSpace(text)
}

func parseSlackTimestamp(ts string) time.Time {
	parts := strings.SplitN(ts, ".", 2)
	if len(parts) == 0 {
		return time.Time{}
	}
	sec, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(sec, 0).UTC()
}

// EnrichDocumentContent extracts text from file attachments and URLs in the message,
// appending extracted content to the document.
func EnrichDocumentContent(ctx context.Context, doc *kb.Document, files []goslack.File, ext *extractor.Extractor, token string) {
	if ext == nil {
		return
	}

	httpClient := extractor.NewRestrictedHTTPClient(30 * time.Second)

	// Extract content from file attachments
	for _, f := range files {
		fileURL := f.URLPrivateDownload
		if fileURL == "" {
			fileURL = f.URLPrivate
		}
		if fileURL == "" {
			continue
		}

		data, err := downloadFile(ctx, httpClient, fileURL, token)
		if err != nil {
			continue // skip files we can't download
		}

		result, err := ext.ExtractFromBytes(ctx, data, f.Mimetype, f.Name)
		if err != nil {
			continue
		}

		if result.Text != "" {
			doc.Content += "\n\n---\nAttached: " + f.Name + "\n" + result.Text
		}
	}

	// Extract content from URLs in message text
	urls := extractURLsFromContent(doc.Content)
	for _, url := range urls {
		result, err := ext.ExtractFromURL(ctx, url)
		if err != nil {
			continue
		}
		if result.Text != "" {
			text := result.Text
			if len(text) > 5000 {
				text = text[:5000] + "..."
			}
			doc.Content += "\n\n---\nLink: " + url + "\n" + text
		}
	}
}

func downloadFile(ctx context.Context, client *http.Client, url, token string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("download file: new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download file: do request: %w", err)
	}
	defer resp.Body.Close()

	return io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024))
}

var plainURLRe = regexp.MustCompile(`https?://[^\s<>"]+`)

func extractURLsFromContent(text string) []string {
	matches := plainURLRe.FindAllString(text, -1)
	var urls []string
	seen := make(map[string]bool)
	for _, u := range matches {
		if !seen[u] {
			seen[u] = true
			urls = append(urls, u)
		}
	}
	return urls
}
