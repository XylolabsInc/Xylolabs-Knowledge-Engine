package slack

import (
	"bytes"
	"encoding/json"
	"log"
	"reflect"
	"strings"
	"testing"

	"github.com/slack-go/slack"

	"github.com/xylolabsinc/xylolabs-kb/internal/bot"
)

func parseCallback(t *testing.T, payload string) slack.InteractionCallback {
	t.Helper()
	var cb slack.InteractionCallback
	if err := json.Unmarshal([]byte(payload), &cb); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	return cb
}

func TestInteractionActions(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    []*bot.Action
	}{
		{
			name: "button in a channel thread",
			payload: `{
				"type": "block_actions",
				"trigger_id": "123.456.abc",
				"user": {"id": "U1", "username": "hong"},
				"channel": {"id": "C1", "name": "general"},
				"container": {"type": "message", "message_ts": "200.2", "channel_id": "C1", "thread_ts": "100.1"},
				"message": {"type": "message", "ts": "200.2", "thread_ts": "100.1", "text": "품의서 미리보기"},
				"actions": [{"type": "button", "action_id": "plugin_approve", "block_id": "plugin_buttons",
					"value": "action-1", "text": {"type": "plain_text", "text": "상신"}, "action_ts": "1.1"}]
			}`,
			want: []*bot.Action{{
				Platform: "slack", UserID: "U1", Channel: "C1", ThreadID: "100.1", MessageID: "200.2",
				ActionID: "plugin_approve", Value: "action-1", Trigger: "123.456.abc",
			}},
		},
		{
			name: "button in a DM, channel only in the container",
			payload: `{
				"type": "block_actions",
				"trigger_id": "t2",
				"user": {"id": "U2"},
				"container": {"type": "message", "message_ts": "300.3", "channel_id": "D9"},
				"actions": [{"type": "button", "action_id": "plugin_connect", "block_id": "plugin_buttons", "value": "daouoffice"}]
			}`,
			want: []*bot.Action{{
				Platform: "slack", UserID: "U2", Channel: "D9", MessageID: "300.3",
				ActionID: "plugin_connect", Value: "daouoffice", Trigger: "t2",
			}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := interactionActions(parseCallback(t, tt.payload))
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("interactionActions()\n got  %+v\n want %+v", deref(got), deref(tt.want))
			}
		})
	}
}

func deref(actions []*bot.Action) []bot.Action {
	out := make([]bot.Action, len(actions))
	for i, a := range actions {
		out[i] = *a
	}
	return out
}

func TestInteractionForm(t *testing.T) {
	payload := `{
		"type": "view_submission",
		"user": {"id": "U1"},
		"view": {
			"id": "V1", "type": "modal", "callback_id": "plugin_connect_form", "private_metadata": "daouoffice",
			"state": {"values": {
				"username": {"username": {"type": "plain_text_input", "value": "hong"}},
				"password": {"password": {"type": "plain_text_input", "value": "s3cret"}},
				"allowKick": {"allowKick": {"type": "checkboxes", "selected_options": [{"value": "true", "text": {"type": "plain_text", "text": "예"}}]}}
			}}
		}
	}`
	got := interactionForm(parseCallback(t, payload))
	want := &bot.FormSubmission{
		Platform: "slack", UserID: "U1", CallbackID: "plugin_connect_form", Metadata: "daouoffice",
		Values: map[string]string{"username": "hong", "password": "s3cret", "allowKick": "true"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("interactionForm() = %+v, want %+v", got, want)
	}

	unchecked := strings.Replace(payload, `"selected_options": [{"value": "true", "text": {"type": "plain_text", "text": "예"}}]`, `"selected_options": []`, 1)
	if got := interactionForm(parseCallback(t, unchecked)); got.Values["allowKick"] != "" {
		t.Errorf("unchecked box = %q, want empty", got.Values["allowKick"])
	}
	if got := interactionForm(parseCallback(t, `{"type": "view_submission", "user": {"id": "U1"}, "view": {}}`)); got != nil {
		t.Errorf("view without a callback ID = %+v, want nil", got)
	}
}

func TestRedactingLogWithholdsSubmissions(t *testing.T) {
	var buf bytes.Buffer
	l := redactingLog{log.New(&buf, "", 0)}
	_ = l.Output(1, `Received WebSocket message: {"type":"interactive","payload":{"type":"view_submission","view":{"state":{"values":{"password":{"password":{"value":"s3cret"}}}}}}}`)
	_ = l.Output(1, "WebSocket connection succeeded on try 1")
	out := buf.String()
	if strings.Contains(out, "s3cret") || !strings.Contains(out, "withheld") || !strings.Contains(out, "connection succeeded") {
		t.Errorf("log = %q", out)
	}
}
