package adapter

import (
	"context"
	"strings"
	"testing"
	"time"

	pluginbridge "github.com/Rokid-Prism/prism-plugin-sdk"

	"github.com/Rokid-Prism/prism-plugin-opencode/internal/opencode"
)

func waitAdapter(stream func(context.Context, func(opencode.Event) error) error, messages []opencode.Message) *Adapter {
	return New(Config{
		Client: &mockClient{
			listMessages: func(context.Context, string) ([]opencode.Message, error) {
				return messages, nil
			},
			streamEvents: stream,
		},
		Endpoint:       "http://127.0.0.1:4096",
		WaitRunTimeout: 250 * time.Millisecond,
	})
}

func TestWaitForRunCompletesOnSessionIdle(t *testing.T) {
	adapter := waitAdapter(
		blockingStreamEvents(eventWithProps(t, "session.idle", map[string]any{
			"sessionID": "ses_1",
			"info":      map[string]any{"reason": "idle"},
		})),
		[]opencode.Message{
			userMessage("msg_u", "task", 1000),
			assistantMessage("msg_a", "final answer body", 2000),
		},
	)
	event, err := adapter.WaitForRun(context.Background(), pluginbridge.NativeSession{NativeSessionID: "ses_1"}, "run_7")
	if err != nil {
		t.Fatalf("WaitForRun error: %v", err)
	}
	if event.Type != "run.completed" || event.Status != "completed" {
		t.Fatalf("event = %s/%s, want run.completed/completed", event.Type, event.Status)
	}
	if event.ID != "run_7" {
		t.Fatalf("ID = %q, want run_7 (run id labels the event)", event.ID)
	}
	if !strings.Contains(event.Summary, "final answer body") {
		t.Fatalf("Summary = %q, want latest assistant text", event.Summary)
	}
	if event.Payload["run_id"] != "run_7" || event.Payload["session_id"] != "ses_1" {
		t.Fatalf("payload = %+v", event.Payload)
	}
}

func TestWaitForRunAbortedBecomesFailure(t *testing.T) {
	adapter := waitAdapter(
		blockingStreamEvents(eventWithProps(t, "session.idle", map[string]any{
			"sessionID": "ses_1",
			"info":      map[string]any{"reason": "aborted"},
		})),
		nil,
	)
	event, err := adapter.WaitForRun(context.Background(), pluginbridge.NativeSession{NativeSessionID: "ses_1"}, "")
	if err != nil {
		t.Fatalf("WaitForRun error: %v", err)
	}
	if event.Type != "run.failed" || event.Status != "failed" {
		t.Fatalf("event = %s/%s, want run.failed/failed", event.Type, event.Status)
	}
	if event.ID != "ses_1:idle" {
		t.Fatalf("ID = %q, want session fallback id", event.ID)
	}
}

func TestWaitForRunIgnoresOtherSessions(t *testing.T) {
	adapter := waitAdapter(
		blockingStreamEvents(
			eventWithProps(t, "session.idle", map[string]any{"sessionID": "ses_other", "info": map[string]any{"reason": "idle"}}),
			eventWithProps(t, "message.updated", map[string]any{"sessionID": "ses_1", "message": map[string]any{"id": "m", "role": "assistant"}}),
			eventWithProps(t, "session.idle", map[string]any{"sessionID": "ses_1", "info": map[string]any{"reason": "idle"}}),
		),
		[]opencode.Message{assistantMessage("msg_a", "done text", 2000)},
	)
	event, err := adapter.WaitForRun(context.Background(), pluginbridge.NativeSession{NativeSessionID: "ses_1"}, "run_1")
	if err != nil {
		t.Fatalf("WaitForRun error: %v", err)
	}
	if event.Type != "run.completed" {
		t.Fatalf("event type = %s, want run.completed (other session idle ignored)", event.Type)
	}
}

// TestWaitForRunCompletesOnSessionStatusIdle pins the opencode 1.19.4 idle
// signal: session.status{status:{type:"idle"}} terminates the wait, while the
// busy transition and streamed parts do not.
func TestWaitForRunCompletesOnSessionStatusIdle(t *testing.T) {
	adapter := waitAdapter(
		blockingStreamEvents(
			eventWithProps(t, "session.status", map[string]any{"sessionID": "ses_1", "status": map[string]any{"type": "busy"}}),
			eventWithProps(t, "message.part.updated", map[string]any{"sessionID": "ses_1", "part": map[string]any{"type": "text", "text": "partial"}}),
			eventWithProps(t, "session.status", map[string]any{"sessionID": "ses_other", "status": map[string]any{"type": "idle"}}),
			eventWithProps(t, "session.status", map[string]any{"sessionID": "ses_1", "status": map[string]any{"type": "idle"}}),
		),
		[]opencode.Message{assistantMessage("msg_a", "status idle answer", 2000)},
	)
	event, err := adapter.WaitForRun(context.Background(), pluginbridge.NativeSession{NativeSessionID: "ses_1"}, "run_9")
	if err != nil {
		t.Fatalf("WaitForRun error: %v", err)
	}
	if event.Type != "run.completed" || event.Status != "completed" {
		t.Fatalf("event = %s/%s, want run.completed/completed", event.Type, event.Status)
	}
	if event.ID != "run_9" || event.Payload["run_id"] != "run_9" {
		t.Fatalf("event = %+v, want run id run_9 labelling", event)
	}
	if !strings.Contains(event.Summary, "status idle answer") {
		t.Fatalf("Summary = %q, want latest assistant text", event.Summary)
	}
}

// TestWaitForRunCompletesOnSessionStatusIdleString covers servers that send
// the discriminator as a bare string instead of {type: ...}.
func TestWaitForRunCompletesOnSessionStatusIdleString(t *testing.T) {
	adapter := waitAdapter(
		blockingStreamEvents(eventWithProps(t, "session.status", map[string]any{
			"sessionID": "ses_1",
			"status":    "idle",
		})),
		nil,
	)
	event, err := adapter.WaitForRun(context.Background(), pluginbridge.NativeSession{NativeSessionID: "ses_1"}, "")
	if err != nil {
		t.Fatalf("WaitForRun error: %v", err)
	}
	if event.Type != "run.completed" {
		t.Fatalf("event type = %s, want run.completed", event.Type)
	}
}

// TestWaitForRunTimesOutOnBusyOnly proves a busy transition alone never
// terminates the wait.
func TestWaitForRunTimesOutOnBusyOnly(t *testing.T) {
	adapter := waitAdapter(
		blockingStreamEvents(eventWithProps(t, "session.status", map[string]any{
			"sessionID": "ses_1",
			"status":    map[string]any{"type": "busy"},
		})),
		nil,
	)
	_, err := adapter.WaitForRun(context.Background(), pluginbridge.NativeSession{NativeSessionID: "ses_1"}, "run_1")
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("error = %v, want timeout", err)
	}
}

func TestWaitForRunTimesOut(t *testing.T) {
	adapter := waitAdapter(blockingStreamEvents(), nil)
	_, err := adapter.WaitForRun(context.Background(), pluginbridge.NativeSession{NativeSessionID: "ses_1"}, "run_1")
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("error = %v, want timeout", err)
	}
}

func TestWaitForRunStreamFailureIsError(t *testing.T) {
	adapter := waitAdapter(func(ctx context.Context, fn func(opencode.Event) error) error {
		return context.DeadlineExceeded
	}, nil)
	_, err := adapter.WaitForRun(context.Background(), pluginbridge.NativeSession{NativeSessionID: "ses_1"}, "run_1")
	if err == nil || !strings.Contains(err.Error(), "event stream") {
		t.Fatalf("error = %v, want stream failure", err)
	}
}

func TestWaitForRunRequiresSessionID(t *testing.T) {
	adapter := waitAdapter(blockingStreamEvents(), nil)
	if _, err := adapter.WaitForRun(context.Background(), pluginbridge.NativeSession{}, "run_1"); err == nil {
		t.Fatalf("expected error for missing session id")
	}
}
