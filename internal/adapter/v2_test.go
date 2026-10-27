package adapter

import (
	"context"
	"github.com/Rokid-Prism/prism-plugin-opencode/internal/opencode"
	pluginbridge "github.com/Rokid-Prism/prism-plugin-sdk"
	"testing"
)

func TestV2ExecutionTerminalEvents(t *testing.T) {
	for _, tc := range []struct{ native, want string }{{"session.execution.succeeded", "run.completed"}, {"session.execution.failed", "run.failed"}, {"session.execution.interrupted", "run.failed"}} {
		t.Run(tc.native, func(t *testing.T) {
			a := waitAdapter(blockingStreamEvents(eventWithProps(t, tc.native, map[string]any{"sessionID": "ses_1"})), nil)
			got, err := a.WaitForRun(context.Background(), pluginbridge.NativeSession{NativeSessionID: "ses_1"}, "run")
			if err != nil || got.Type != tc.want {
				t.Fatalf("terminal %#v %v", got, err)
			}
			event, ok := a.translateSessionEvent(context.Background(), "ses_1", tc.native, map[string]any{"sessionID": "ses_1"})
			if !ok || event.Type != tc.want {
				t.Fatalf("subscribe terminal %#v", event)
			}
		})
	}
}

func TestV2StructuredFailureVisible(t *testing.T) {
	m := opencode.Message{ID: "a", Role: "assistant", Raw: map[string]any{"error": map[string]any{"type": "ProviderError", "message": "test provider unavailable"}}}
	got := historyMessage(m)
	if got.Content != "OpenCode 运行失败：test provider unavailable" {
		t.Fatalf("failure hidden: %#v", got)
	}
}
