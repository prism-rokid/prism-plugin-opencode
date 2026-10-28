package adapter

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	pluginbridge "github.com/Rokid-Prism/prism-plugin-sdk"

	"github.com/Rokid-Prism/prism-plugin-opencode/internal/opencode"
)

const subscribeSession = "ses_1"

func TestPluginWideWatcherDiscoversNativeSession(t *testing.T) {
	client := &mockClient{
		getSession: func(_ context.Context, id string) (*opencode.Session, error) {
			return &opencode.Session{ID: id, Title: "Native CLI session", Directory: "/project"}, nil
		},
		streamEvents: func(ctx context.Context, fn func(opencode.Event) error) error {
			if err := fn(eventWithProps(t, "session.created", map[string]any{"sessionID": "ses_new"})); err != nil {
				return err
			}
			return fn(eventWithProps(t, "session.created", map[string]any{"info": map[string]any{"id": "ses_legacy"}}))
		},
	}
	a := New(Config{Client: client, Endpoint: "http://localhost:4096"})
	events, err := a.SubscribePlugin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	event, ok := <-events
	if !ok {
		t.Fatal("native creation was not published")
	}
	native, ok := event.Payload["native_session"].(map[string]any)
	if !ok || native["native_session_id"] != "ses_new" || native["cwd"] != "/project" {
		t.Fatalf("incorrect native identity: %#v", event.Payload)
	}
	hint, ok := event.Payload["session_hint"].(map[string]any)
	if !ok || hint["title"] != "Native CLI session" {
		t.Fatalf("missing session index hint: %#v", event.Payload)
	}
	legacy, ok := <-events
	if !ok || legacy.Payload["native_session"].(map[string]any)["native_session_id"] != "ses_legacy" {
		t.Fatalf("legacy session.created identity not discovered: %#v", legacy.Payload)
	}
}

// TestSubscribeTranslatesNativeEvents is the table-driven SSE->PluginEvent
// contract: each native event yields the listed PluginEvent (or none).
func TestSubscribeTranslatesNativeEvents(t *testing.T) {
	tests := []struct {
		name           string
		event          opencode.Event
		wantOK         bool
		wantType       string
		wantStatus     string
		wantSummarySub string
		wantPayload    map[string]any // subset match
	}{
		{
			name:   "server.connected ignored",
			event:  eventWithProps(t, "server.connected", map[string]any{"serverInfo": map[string]any{"version": "1"}}),
			wantOK: false,
		},
		{
			name:   "other session filtered",
			event:  eventWithProps(t, "message.updated", map[string]any{"sessionID": "ses_other", "message": map[string]any{"id": "m1", "role": "assistant"}}),
			wantOK: false,
		},
		{
			name:           "session.updated carries title as summary",
			event:          eventWithProps(t, "session.updated", map[string]any{"sessionID": subscribeSession, "session": map[string]any{"id": subscribeSession, "title": "Refactor adapter"}}),
			wantOK:         true,
			wantType:       "session.updated",
			wantStatus:     "idle",
			wantSummarySub: "Refactor adapter",
			wantPayload:    map[string]any{"session_id": subscribeSession, "title": "Refactor adapter"},
		},
		{
			// Real 1.19.4 wire shape: the session object nests under "info".
			name:           "session.updated via real info key carries title",
			event:          eventWithProps(t, "session.updated", map[string]any{"sessionID": subscribeSession, "info": map[string]any{"id": subscribeSession, "title": "Real wire shape"}}),
			wantOK:         true,
			wantType:       "session.updated",
			wantStatus:     "idle",
			wantSummarySub: "Real wire shape",
			wantPayload:    map[string]any{"session_id": subscribeSession, "title": "Real wire shape"},
		},
		{
			// Real 1.19.4 wire shape: message info nests under "info" and the
			// streamed info carries no parts (text arrives via part events).
			name:        "assistant message.updated via real info key streams as running",
			event:       eventWithProps(t, "message.updated", map[string]any{"sessionID": subscribeSession, "info": map[string]any{"id": "msg_real", "role": "assistant"}}),
			wantOK:      true,
			wantType:    "message.assistant",
			wantStatus:  "running",
			wantPayload: map[string]any{"message_id": "msg_real", "role": "assistant", "streaming": true},
		},
		{
			name:           "assistant message.updated streams as running",
			event:          eventWithProps(t, "message.updated", map[string]any{"sessionID": subscribeSession, "messageID": "msg_a", "message": map[string]any{"id": "msg_a", "role": "assistant", "parts": []any{map[string]any{"type": "text", "text": "working on it"}}}}),
			wantOK:         true,
			wantType:       "message.assistant",
			wantStatus:     "running",
			wantSummarySub: "working on it",
			wantPayload:    map[string]any{"message_id": "msg_a", "role": "assistant", "streaming": true},
		},
		{
			name:           "user message echo is completed",
			event:          eventWithProps(t, "message.updated", map[string]any{"sessionID": subscribeSession, "messageID": "msg_u", "message": map[string]any{"id": "msg_u", "role": "user", "parts": []any{map[string]any{"type": "text", "text": "do it"}}}}),
			wantOK:         true,
			wantType:       "message.user",
			wantStatus:     "completed",
			wantSummarySub: "do it",
			wantPayload:    map[string]any{"role": "user", "streaming": false},
		},
		{
			name:           "text part update carries output stream text",
			event:          eventWithProps(t, "message.part.updated", map[string]any{"sessionID": subscribeSession, "messageID": "msg_a", "part": map[string]any{"type": "text", "text": "partial answer"}}),
			wantOK:         true,
			wantType:       "message.part.updated",
			wantStatus:     "running",
			wantSummarySub: "partial answer",
			wantPayload:    map[string]any{"part_type": "text", "text": "partial answer"},
		},
		{
			name:        "non-text part update has no text payload",
			event:       eventWithProps(t, "message.part.updated", map[string]any{"sessionID": subscribeSession, "messageID": "msg_a", "part": map[string]any{"type": "tool", "state": map[string]any{"status": "running"}}}),
			wantOK:      true,
			wantType:    "message.part.updated",
			wantStatus:  "running",
			wantPayload: map[string]any{"part_type": "tool"},
		},
		{
			name:           "permission asked surfaces approval panel",
			event:          eventWithProps(t, "permission.asked", map[string]any{"sessionID": subscribeSession, "permissionID": "perm_1", "permission": map[string]any{"type": "bash", "title": "运行 rm -rf", "pattern": "rm *"}}),
			wantOK:         true,
			wantType:       "approval.required",
			wantStatus:     "waiting_approval",
			wantSummarySub: "运行 rm -rf",
			wantPayload:    map[string]any{"approval_request_id": "perm_1", "resolvable": false},
		},
		{
			name:       "permission responded resolves panel",
			event:      eventWithProps(t, "permission.responded", map[string]any{"sessionID": subscribeSession, "permissionID": "perm_1"}),
			wantOK:     true,
			wantType:   "approval.resolve",
			wantStatus: "completed",
		},
		{
			name:       "session.deleted reported",
			event:      eventWithProps(t, "session.deleted", map[string]any{"sessionID": subscribeSession}),
			wantOK:     true,
			wantType:   "session.deleted",
			wantStatus: "completed",
		},
		{
			name:   "unmapped event type dropped",
			event:  eventWithProps(t, "installation.updated", map[string]any{"sessionID": subscribeSession, "version": "1.2"}),
			wantOK: false,
		},
		{
			// Real 1.19.4 wire shape: run lifecycle is session.status with a
			// nested status object; busy means "run started", not terminal.
			name:   "session.status busy not forwarded",
			event:  eventWithProps(t, "session.status", map[string]any{"sessionID": subscribeSession, "status": map[string]any{"type": "busy"}}),
			wantOK: false,
		},
		{
			name:   "session.status idle for other session dropped",
			event:  eventWithProps(t, "session.status", map[string]any{"sessionID": "ses_other", "status": map[string]any{"type": "idle"}}),
			wantOK: false,
		},
		{
			name:   "server.heartbeat not forwarded",
			event:  eventWithProps(t, "server.heartbeat", map[string]any{}),
			wantOK: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			adapter := testAdapter(&mockClient{
				listMessages: func(context.Context, string) ([]opencode.Message, error) {
					return []opencode.Message{}, nil
				},
				streamEvents: blockingStreamEvents(tt.event),
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			out, err := adapter.Subscribe(ctx, pluginbridge.NativeSession{NativeSessionID: subscribeSession})
			if err != nil {
				t.Fatalf("Subscribe error: %v", err)
			}
			if !tt.wantOK {
				assertNoEvent(t, out)
				return
			}
			event := nextEvent(t, out)
			if event.Type != tt.wantType {
				t.Fatalf("Type = %q, want %q", event.Type, tt.wantType)
			}
			if event.Status != tt.wantStatus {
				t.Fatalf("Status = %q, want %q", event.Status, tt.wantStatus)
			}
			if tt.wantSummarySub != "" && !contains(event.Summary, tt.wantSummarySub) {
				t.Fatalf("Summary = %q, want substring %q", event.Summary, tt.wantSummarySub)
			}
			for key, want := range tt.wantPayload {
				got, ok := event.Payload[key]
				if !ok {
					t.Fatalf("payload missing %q: %+v", key, event.Payload)
				}
				if fmtValue(got) != fmtValue(want) {
					t.Fatalf("payload[%s] = %v, want %v", key, got, want)
				}
			}
		})
	}
}

func TestSubscribeSessionIdleBuildsTerminalEvent(t *testing.T) {
	adapter := testAdapter(&mockClient{
		listMessages: func(context.Context, string) ([]opencode.Message, error) {
			return []opencode.Message{
				userMessage("msg_u", "please summarize", 1000),
				assistantMessage("msg_a", "here is the summary of everything", 2000),
			}, nil
		},
		streamEvents: blockingStreamEvents(eventWithProps(t, "session.idle", map[string]any{
			"sessionID": subscribeSession,
			"info":      map[string]any{"reason": "idle", "timestamp": 1234},
		})),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out, err := adapter.Subscribe(ctx, pluginbridge.NativeSession{NativeSessionID: subscribeSession})
	if err != nil {
		t.Fatalf("Subscribe error: %v", err)
	}
	event := nextEvent(t, out)
	if event.Type != "run.completed" || event.Status != "completed" {
		t.Fatalf("terminal event = %s/%s, want run.completed/completed", event.Type, event.Status)
	}
	if !contains(event.Summary, "here is the summary") {
		t.Fatalf("Summary = %q, want latest assistant text", event.Summary)
	}
	if event.Payload["session_id"] != subscribeSession {
		t.Fatalf("payload session_id = %v", event.Payload["session_id"])
	}
}

// TestSubscribeSessionStatusIdleBuildsTerminalEvent pins the opencode 1.19.4
// idle signal: session.status{status:{type:"idle"}} is the terminal event and
// the preceding busy transition must not leak a terminal event first.
func TestSubscribeSessionStatusIdleBuildsTerminalEvent(t *testing.T) {
	adapter := testAdapter(&mockClient{
		listMessages: func(context.Context, string) ([]opencode.Message, error) {
			return []opencode.Message{
				assistantMessage("msg_a", "status-idle final answer", 2000),
			}, nil
		},
		streamEvents: blockingStreamEvents(
			eventWithProps(t, "session.status", map[string]any{"sessionID": subscribeSession, "status": map[string]any{"type": "busy"}}),
			eventWithProps(t, "message.part.updated", map[string]any{"sessionID": subscribeSession, "part": map[string]any{"type": "text", "text": "partial"}}),
			eventWithProps(t, "session.status", map[string]any{"sessionID": subscribeSession, "status": map[string]any{"type": "idle"}}),
		),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out, err := adapter.Subscribe(ctx, pluginbridge.NativeSession{NativeSessionID: subscribeSession})
	if err != nil {
		t.Fatalf("Subscribe error: %v", err)
	}
	first := nextEvent(t, out)
	if first.Type != "message.part.updated" {
		t.Fatalf("first event = %s, want message.part.updated (busy must not emit)", first.Type)
	}
	terminal := nextEvent(t, out)
	if terminal.Type != "run.completed" || terminal.Status != "completed" {
		t.Fatalf("terminal event = %s/%s, want run.completed/completed", terminal.Type, terminal.Status)
	}
	if !contains(terminal.Summary, "status-idle final answer") {
		t.Fatalf("Summary = %q, want latest assistant text", terminal.Summary)
	}
	if terminal.Payload["session_id"] != subscribeSession {
		t.Fatalf("payload session_id = %v", terminal.Payload["session_id"])
	}
}

// TestSessionStatusTypeShapes pins the defensive parsing of the busy/idle
// discriminator: nested object (real 1.19.4), bare string, or absent.
func TestSessionStatusTypeShapes(t *testing.T) {
	cases := []struct {
		name  string
		props map[string]any
		want  string
	}{
		{"nested object", map[string]any{"status": map[string]any{"type": "Idle"}}, "idle"},
		{"bare string", map[string]any{"status": "busy"}, "busy"},
		{"missing", map[string]any{"sessionID": "ses_1"}, ""},
		{"wrong type", map[string]any{"status": 42}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sessionStatusType(tc.props); got != tc.want {
				t.Fatalf("sessionStatusType = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSubscribeStreamFailureEmitsSubscriptionError(t *testing.T) {
	adapter := testAdapter(&mockClient{
		streamEvents: func(ctx context.Context, fn func(opencode.Event) error) error {
			return context.DeadlineExceeded // terminal stream error while ctx alive
		},
	})
	out, err := adapter.Subscribe(context.Background(), pluginbridge.NativeSession{NativeSessionID: subscribeSession})
	if err != nil {
		t.Fatalf("Subscribe error: %v", err)
	}
	event := nextEvent(t, out)
	if event.Type != "subscription.error" || event.Status != "failed" {
		t.Fatalf("event = %s/%s, want subscription.error/failed", event.Type, event.Status)
	}
	// Stream returned; channel must close afterwards.
	if _, open := <-out; open {
		t.Fatalf("channel should close after subscription error")
	}
}

func TestSubscribeRequiresSessionID(t *testing.T) {
	adapter := testAdapter(&mockClient{})
	if _, err := adapter.Subscribe(context.Background(), pluginbridge.NativeSession{}); err == nil {
		t.Fatalf("expected error for missing session id")
	}
}

// --- small shared helpers ---

func nextEvent(t *testing.T, out <-chan pluginbridge.PluginEvent) pluginbridge.PluginEvent {
	t.Helper()
	select {
	case event, ok := <-out:
		if !ok {
			t.Fatalf("event channel closed before expected event")
		}
		return event
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for event")
		return pluginbridge.PluginEvent{}
	}
}

func assertNoEvent(t *testing.T, out <-chan pluginbridge.PluginEvent) {
	t.Helper()
	select {
	case event, ok := <-out:
		if ok {
			t.Fatalf("unexpected event: %+v", event)
		}
	case <-time.After(150 * time.Millisecond):
	}
}

func contains(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}

func fmtValue(value any) string {
	return strings.TrimSpace(fmt.Sprint(value))
}
