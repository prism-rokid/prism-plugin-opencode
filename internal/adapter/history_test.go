package adapter

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	pluginbridge "github.com/Rokid-Prism/prism-plugin-sdk"

	"github.com/Rokid-Prism/prism-plugin-opencode/internal/opencode"
)

// threeTurns is a chronological body: turn N = user + assistant, plus a
// system message that must never surface in the remote body.
func threeTurns() []opencode.Message {
	return []opencode.Message{
		{ID: "sys_0", Role: "system", Time: opencode.MessageTime{Created: 100}},
		userMessage("msg_u1", "first question", 1000),
		assistantMessage("msg_a1", "first answer", 1100),
		userMessage("msg_u2", "second question", 2000),
		assistantMessage("msg_a2", "second answer", 2100),
		userMessage("msg_u3", "third question", 3000),
		assistantMessage("msg_a3", "third answer", 3100),
	}
}

func nextHistoryEvent(t *testing.T, out <-chan pluginbridge.HistoryStreamEvent) pluginbridge.HistoryStreamEvent {
	t.Helper()
	select {
	case event, ok := <-out:
		if !ok {
			t.Fatalf("history channel closed before expected event")
		}
		return event
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for history event")
		return pluginbridge.HistoryStreamEvent{}
	}
}

func readHistoryEvents(t *testing.T, out <-chan pluginbridge.HistoryStreamEvent, untilType string) []pluginbridge.HistoryStreamEvent {
	t.Helper()
	var events []pluginbridge.HistoryStreamEvent
	for {
		event := nextHistoryEvent(t, out)
		events = append(events, event)
		if event.Type == untilType {
			return events
		}
		if event.Type == "error" {
			t.Fatalf("unexpected history error: %+v", event)
		}
	}
}

func TestHistoryStreamSnapshotEndsWithTurnsNewestFirst(t *testing.T) {
	adapter := testAdapter(&mockClient{listMessages: func(context.Context, string) ([]opencode.Message, error) {
		return threeTurns(), nil
	}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out, err := adapter.ReadHistoryStream(ctx, pluginbridge.NativeSession{NativeSessionID: "ses_1"}, pluginbridge.HistoryStreamRequest{StreamID: "s1"})
	if err != nil {
		t.Fatalf("ReadHistoryStream error: %v", err)
	}
	events := readHistoryEvents(t, out, "end")
	if len(events) != 4 { // 3 turns + end
		t.Fatalf("got %d events, want 4", len(events))
	}
	// Newest turn first.
	first := events[0]
	if first.Type != "turn" || first.Source != "initial" || first.Operation != "append" || first.StreamID != "s1" {
		t.Fatalf("first event = %+v", first)
	}
	if first.Turn == nil || first.Turn.TurnID != "msg_u3" {
		t.Fatalf("first turn = %+v, want newest turn msg_u3", first.Turn)
	}
	if first.Turn.Revision != 1 {
		t.Fatalf("initial revision = %d, want 1", first.Turn.Revision)
	}
	roles := []string{}
	for _, message := range first.Turn.Messages {
		roles = append(roles, message.Role)
	}
	if strings.Join(roles, ",") != "user,assistant" {
		t.Fatalf("turn roles = %v", roles)
	}
	if !strings.Contains(first.Turn.Messages[0].Content, "third question") {
		t.Fatalf("unexpected content: %+v", first.Turn.Messages[0])
	}
}

func TestHistoryStreamSlicesToLatestTurns(t *testing.T) {
	adapter := testAdapter(&mockClient{listMessages: func(context.Context, string) ([]opencode.Message, error) {
		return threeTurns(), nil
	}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out, err := adapter.ReadHistoryStream(ctx, pluginbridge.NativeSession{NativeSessionID: "ses_1"}, pluginbridge.HistoryStreamRequest{StreamID: "s1", Limit: 2})
	if err != nil {
		t.Fatalf("ReadHistoryStream error: %v", err)
	}
	events := readHistoryEvents(t, out, "end")
	turns := 0
	for _, event := range events {
		if event.Type != "turn" {
			continue
		}
		turns++
		if event.Turn.TurnID != "msg_u3" && event.Turn.TurnID != "msg_u2" {
			t.Fatalf("turn %s outside the latest-2 window", event.Turn.TurnID)
		}
	}
	if turns != 2 {
		t.Fatalf("turns = %d, want 2", turns)
	}
}

func TestHistoryStreamMapsToolPartsToProgressSteps(t *testing.T) {
	messages := []opencode.Message{
		userMessage("msg_u1", "run the tool", 1000),
		{
			ID: "msg_a1", Role: "assistant", Time: opencode.MessageTime{Created: 1100},
			ProviderID: "anthropic", ModelID: "claude-sonnet-4",
			Parts: []opencode.MessagePart{
				{Type: "step-start"},
				{Type: "tool", Raw: map[string]any{"tool": "read", "callID": "call_1", "state": map[string]any{"status": "completed"}}},
				{Type: "tool", Raw: map[string]any{"tool": "bash", "state": map[string]any{"status": "error"}}},
				textPart("used the tools"),
			},
		},
	}
	adapter := testAdapter(&mockClient{listMessages: func(context.Context, string) ([]opencode.Message, error) {
		return messages, nil
	}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out, err := adapter.ReadHistoryStream(ctx, pluginbridge.NativeSession{NativeSessionID: "ses_1"}, pluginbridge.HistoryStreamRequest{StreamID: "s1"})
	if err != nil {
		t.Fatalf("ReadHistoryStream error: %v", err)
	}
	events := readHistoryEvents(t, out, "end")
	turn := events[0].Turn
	if len(turn.Messages) != 2 {
		t.Fatalf("messages = %d", len(turn.Messages))
	}
	assistant := turn.Messages[1]
	if assistant.Content != "used the tools" {
		t.Fatalf("Content = %q", assistant.Content)
	}
	if assistant.Progress == nil || len(assistant.Progress.Steps) != 2 {
		t.Fatalf("Progress = %+v", assistant.Progress)
	}
	first, second := assistant.Progress.Steps[0], assistant.Progress.Steps[1]
	if first.Kind != "tool" || first.Title != "read" || first.CallID != "call_1" || first.Status != "completed" {
		t.Fatalf("first step = %+v", first)
	}
	if second.Title != "bash" || second.Status != "failed" {
		t.Fatalf("second step = %+v", second)
	}
	if assistant.Metadata["model_id"] != "claude-sonnet-4" {
		t.Fatalf("metadata = %+v", assistant.Metadata)
	}
}

func TestHistoryStreamErrorEventOnListFailure(t *testing.T) {
	adapter := testAdapter(&mockClient{listMessages: func(context.Context, string) ([]opencode.Message, error) {
		return nil, context.DeadlineExceeded
	}})
	out, err := adapter.ReadHistoryStream(context.Background(), pluginbridge.NativeSession{NativeSessionID: "ses_1"}, pluginbridge.HistoryStreamRequest{StreamID: "s1"})
	if err != nil {
		t.Fatalf("ReadHistoryStream error: %v", err)
	}
	event := nextHistoryEvent(t, out)
	if event.Type != "error" || !event.Retryable {
		t.Fatalf("event = %+v, want retryable error", event)
	}
}

func TestHistoryStreamLiveEmitsPageEndAndReplacesChangedTurns(t *testing.T) {
	initial := threeTurns()
	updated := make([]opencode.Message, len(initial))
	copy(updated, initial)
	// Assistant edits its answer and a brand-new turn arrives.
	for index, message := range updated {
		if message.ID == "msg_a3" {
			updated[index].Parts = []opencode.MessagePart{textPart("third answer, revised")}
		}
	}
	updated = append(updated, userMessage("msg_u4", "fourth question", 4000), assistantMessage("msg_a4", "fourth answer", 4100))

	calls := 0
	adapter := testAdapter(&mockClient{
		listMessages: func(context.Context, string) ([]opencode.Message, error) {
			calls++
			if calls == 1 {
				return initial, nil
			}
			return updated, nil
		},
		streamEvents: blockingStreamEvents(eventWithProps(t, "message.part.updated", map[string]any{
			"sessionID": "ses_1", "messageID": "msg_a3",
		})),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out, err := adapter.ReadHistoryStream(ctx, pluginbridge.NativeSession{NativeSessionID: "ses_1"}, pluginbridge.HistoryStreamRequest{StreamID: "s1", Live: true})
	if err != nil {
		t.Fatalf("ReadHistoryStream error: %v", err)
	}
	events := readHistoryEvents(t, out, "page_end")
	if events[len(events)-1].Type != "page_end" {
		t.Fatalf("last initial event = %+v", events[len(events)-1])
	}

	// The live reload emits newest-first: append(msg_u4) then
	// replace(msg_u3); unchanged turns are not re-emitted.
	appended := nextHistoryEvent(t, out)
	if appended.Type != "turn" || appended.Source != "live" || appended.Operation != "append" {
		t.Fatalf("first live event = %+v", appended)
	}
	if appended.Turn == nil || appended.Turn.TurnID != "msg_u4" {
		t.Fatalf("newest live turn = %+v, want msg_u4", appended.Turn)
	}
	replaced := nextHistoryEvent(t, out)
	if replaced.Type != "turn" || replaced.Operation != "replace" || replaced.Turn == nil || replaced.Turn.TurnID != "msg_u3" {
		t.Fatalf("second live event = %+v", replaced)
	}
	if replaced.Turn.Revision != 2 {
		t.Fatalf("replaced revision = %d, want 2", replaced.Turn.Revision)
	}
	if !strings.Contains(replaced.Turn.Messages[1].Content, "revised") {
		t.Fatalf("replaced content = %q", replaced.Turn.Messages[1].Content)
	}
	if appended.Turn.Revision != 1 {
		t.Fatalf("appended revision = %d, want 1", appended.Turn.Revision)
	}
	// Unchanged turns are not re-emitted: exactly two live events for one
	// reload. The stream must stay open (live) afterwards.
	assertNoHistoryEvent(t, out)
}

func TestHistoryStreamCancelClosesChannel(t *testing.T) {
	adapter := testAdapter(&mockClient{
		listMessages: func(context.Context, string) ([]opencode.Message, error) {
			return threeTurns(), nil
		},
		streamEvents: blockingStreamEvents(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	out, err := adapter.ReadHistoryStream(ctx, pluginbridge.NativeSession{NativeSessionID: "ses_1"}, pluginbridge.HistoryStreamRequest{StreamID: "s1", Live: true})
	if err != nil {
		t.Fatalf("ReadHistoryStream error: %v", err)
	}
	readHistoryEvents(t, out, "page_end")
	cancel()
	select {
	case _, open := <-out:
		if open {
			t.Fatalf("channel must close after cancellation")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("channel did not close after cancellation")
	}
}

func TestHistoryStreamRequiresSessionID(t *testing.T) {
	adapter := testAdapter(&mockClient{})
	if _, err := adapter.ReadHistoryStream(context.Background(), pluginbridge.NativeSession{}, pluginbridge.HistoryStreamRequest{StreamID: "s1"}); err == nil {
		t.Fatalf("expected error for missing session id")
	}
}

func assertNoHistoryEvent(t *testing.T, out <-chan pluginbridge.HistoryStreamEvent) {
	t.Helper()
	select {
	case event, ok := <-out:
		if ok {
			t.Fatalf("unexpected history event: %+v", event)
		}
	case <-time.After(150 * time.Millisecond):
	}
}

func TestHistoryMessageExposesAttachmentMetadataOnly(t *testing.T) {
	msg := userMessage("msg_att", "look at this", 1000)
	msg.Parts = append(msg.Parts, opencode.MessagePart{
		Type: "file",
		Raw: map[string]any{
			"mime":     "image/png",
			"filename": "shot.png",
			"url":      "data:image/png;base64,VERYLONG",
		},
	})
	hm := historyMessage(msg)
	attachments, ok := hm.Metadata["attachments"].([]map[string]any)
	if !ok || len(attachments) != 1 {
		t.Fatalf("attachments metadata = %#v", hm.Metadata["attachments"])
	}
	att := attachments[0]
	if att["name"] != "shot.png" || att["mime_type"] != "image/png" || att["kind"] != "file" {
		t.Fatalf("attachment entry = %#v", att)
	}
	encoded, _ := json.Marshal(hm.Metadata)
	if strings.Contains(string(encoded), "data:image") {
		t.Fatalf("metadata must not leak the data URL: %s", encoded)
	}
}
