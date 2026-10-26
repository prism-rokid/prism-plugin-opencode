package adapter

import (
	"context"
	"strings"
	"testing"

	"github.com/Rokid-Prism/prism-plugin-opencode/internal/opencode"
	pluginbridge "github.com/Rokid-Prism/prism-plugin-sdk"
)

func TestPartEventMessageID(t *testing.T) {
	// Real 1.19.4 frames carry messageID inside part, not at the top level.
	event := translateMessagePartUpdated("ses_1", map[string]any{
		"sessionID": "ses_1",
		"part": map[string]any{
			"id": "part_1", "messageID": "msg_1", "type": "text", "text": "smoke OK",
		},
	})
	if got := event.Payload["message_id"]; got != "msg_1" {
		t.Fatalf("streaming text lost message association: got %v", got)
	}
}

func TestIdleUsesLatestAssistantError(t *testing.T) {
	for _, name := range []string{"MessageAbortedError", "APIError"} {
		t.Run(name, func(t *testing.T) {
			latest := assistantMessage("msg_failed", "", 3000)
			latest.Raw = map[string]any{"error": map[string]any{
				"name": name, "data": map[string]any{"message": "request failed"},
			}}
			a := waitAdapter(blockingStreamEvents(eventWithProps(t, "session.status", map[string]any{
				"sessionID": "ses_1", "status": map[string]any{"type": "idle"},
			})), []opencode.Message{assistantMessage("msg_old", "previous answer", 1000), latest})
			event, err := a.WaitForRun(context.Background(), pluginbridge.NativeSession{NativeSessionID: "ses_1"}, "run_1")
			if err != nil {
				t.Fatal(err)
			}
			if event.Type != "run.failed" || event.Status != "failed" || strings.Contains(event.Summary, "previous answer") {
				t.Fatalf("failed run reported success or stale output: %+v", event)
			}
			if name == "MessageAbortedError" && event.Payload["reason"] != "aborted" {
				t.Fatalf("abort reason missing: %+v", event)
			}
		})
	}
}
