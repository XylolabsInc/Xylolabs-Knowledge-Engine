package slack

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
	"golang.org/x/time/rate"

	"github.com/xylolabsinc/xylolabs-kb/internal/bot"
	"github.com/xylolabsinc/xylolabs-kb/internal/kb"
	"github.com/xylolabsinc/xylolabs-kb/internal/storage"
)

// clickRecorder is a bot handler that notes when a click arrives.
type clickRecorder struct {
	mu     sync.Mutex
	clicks []time.Time
}

func (c *clickRecorder) HandleMention(context.Context, *bot.IncomingMessage)       {}
func (c *clickRecorder) HandleDirectMessage(context.Context, *bot.IncomingMessage) {}
func (c *clickRecorder) HandleFormSubmission(context.Context, *bot.FormSubmission) {}
func (c *clickRecorder) IsTrackedThread(string, string) bool                       { return false }
func (c *clickRecorder) HandleAction(context.Context, *bot.Action) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clicks = append(c.clicks, time.Now())
}

func (c *clickRecorder) first() (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.clicks) == 0 {
		return time.Time{}, false
	}
	return c.clicks[0], true
}

// A click must not wait behind slow message events (rate-limited user
// lookups, indexing): past 3 seconds Slack shows an error and the trigger
// for opening a form expires.
func TestInteractiveEventsSkipTheMessageQueue(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(1500 * time.Millisecond):
		case <-r.Context().Done():
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"user":{"id":"U1","name":"hong"},"channel":{"id":"C1","name":"general"}}`))
	}))
	defer api.Close()

	logger := slog.New(slog.DiscardHandler)
	store, err := storage.New(filepath.Join(t.TempDir(), "kb.db"), logger)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	client := slack.New("xoxb-test", slack.OptionAPIURL(api.URL+"/"))
	rec := &clickRecorder{}
	c := &Connector{
		client:        client,
		socketClient:  socketmode.New(client),
		engine:        kb.NewEngine(store, logger),
		store:         store,
		logger:        logger,
		userCache:     map[string]*slack.User{},
		channelCache:  map[string]string{},
		accessibleIMs: map[string]struct{}{},
		deniedIMs:     map[string]time.Time{},
		limiter:       rate.NewLimiter(rate.Limit(1), 1),
		botHandler:    rec,
		botUserID:     "UBOT",
	}

	ctx, cancel := context.WithCancel(context.Background())
	loopDone := make(chan struct{})
	go func() {
		c.handleEvents(ctx)
		close(loopDone)
	}()

	for i := range 6 {
		c.socketClient.Events <- socketmode.Event{
			Type:    socketmode.EventTypeEventsAPI,
			Request: &socketmode.Request{EnvelopeID: "env-msg"},
			Data: slackevents.EventsAPIEvent{
				Type: slackevents.CallbackEvent,
				InnerEvent: slackevents.EventsAPIInnerEvent{
					Type: "message",
					Data: &slackevents.MessageEvent{Type: "message", User: "U1", Channel: "C1", Text: "hello", TimeStamp: "1.00000" + string(rune('0'+i))},
				},
			},
		}
	}

	sent := time.Now()
	c.socketClient.Events <- socketmode.Event{
		Type:    socketmode.EventTypeInteractive,
		Request: &socketmode.Request{EnvelopeID: "env-click"},
		Data: slack.InteractionCallback{
			Type:    slack.InteractionTypeBlockActions,
			User:    slack.User{ID: "U1"},
			Channel: slack.Channel{GroupConversation: slack.GroupConversation{Conversation: slack.Conversation{ID: "C1"}}},
			ActionCallback: slack.ActionCallbacks{BlockActions: []*slack.BlockAction{
				{ActionID: "plugin_approve", BlockID: "plugin_buttons", Value: "a1"},
			}},
		},
	}

	deadline := time.Now().Add(5 * time.Second)
	var got time.Time
	for {
		if at, ok := rec.first(); ok {
			got = at
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the click never reached the bot")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if wait := got.Sub(sent); wait > 500*time.Millisecond {
		t.Errorf("the click waited %v behind message events", wait)
	}

	cancel()
	select {
	case <-loopDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the event loop did not stop after cancel")
	}
}
