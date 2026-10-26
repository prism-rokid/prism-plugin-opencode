package adapter

import (
	"context"
	"testing"
	"time"

	"github.com/Rokid-Prism/prism-plugin-opencode/internal/opencode"
	pluginbridge "github.com/Rokid-Prism/prism-plugin-sdk"
)

func TestDraftCreatesOnlyOnStartAndPreservesModelAndDirectory(t *testing.T) {
	ctx := context.Background()
	creates, sends := 0, 0
	var created opencode.CreateSessionRequest
	a := testAdapter(&mockClient{
		getConfig: func(context.Context) (*opencode.Config, error) { return &opencode.Config{}, nil },
		listModels: func(context.Context) ([]opencode.ModelInfo, error) {
			return []opencode.ModelInfo{{ProviderID: "rokid", ModelID: "deepseek"}}, nil
		},
		createSession: func(_ context.Context, req opencode.CreateSessionRequest) (*opencode.Session, error) {
			creates++
			created = req
			return &opencode.Session{ID: "ses_draft", Directory: req.Directory}, nil
		},
		prompt: func(context.Context, string, string) error { sends++; return nil },
		listMessages: func(context.Context, string) ([]opencode.Message, error) {
			return []opencode.Message{userMessage("msg_1", "draft first message", 1)}, nil
		},
	})
	opened, err := a.OpenDraft(ctx, pluginbridge.DraftOpenRequest{DraftID: "draft_1", Cwd: "/work"})
	if err != nil || opened.DraftID != "draft_1" || creates != 0 {
		t.Fatalf("open: %+v %v creates=%d", opened, err, creates)
	}
	_, err = a.ControlDraft(ctx, pluginbridge.DraftControlRequest{DraftID: "draft_1", Action: "model.switch", Target: map[string]any{"option_id": "rokid/deepseek"}})
	if err != nil || creates != 0 {
		t.Fatalf("control created native session: %v", err)
	}
	req := pluginbridge.StartDraftWithMessageRequest{DraftID: "draft_1", Cwd: "/work", Message: pluginbridge.InboundMessage{PrismMessageID: "msg_1", Text: "draft first message"}}
	for i := 0; i < 2; i++ {
		started, err := a.StartDraftWithMessage(ctx, req)
		if err != nil || !started.Visibility.Visible || !started.Receipt.Accepted || started.Session.NativeSessionID != "ses_draft" {
			t.Fatalf("start: %+v %v", started, err)
		}
	}
	if creates != 1 || sends != 1 || created.Directory != "/work" || created.Model == nil || created.Model.ID != "deepseek" || created.Model.ProviderID != "rokid" {
		t.Fatalf("wrong start side effects: creates=%d sends=%d req=%+v", creates, sends, created)
	}
	req.Message.Text = "changed"
	if _, err := a.StartDraftWithMessage(ctx, req); err == nil {
		t.Fatal("started draft accepted another first message")
	}
	if _, err := a.ControlDraft(ctx, pluginbridge.DraftControlRequest{DraftID: "draft_1", Action: "model.switch", Target: "rokid/deepseek"}); err == nil {
		t.Fatal("started draft accepted controls")
	}
}

func TestDraftStaleOrUnknownModelRejected(t *testing.T) {
	a := testAdapter(&mockClient{listModels: func(context.Context) ([]opencode.ModelInfo, error) { return []opencode.ModelInfo{}, nil }})
	a.drafts = map[string]*draftState{"expired": {created: time.Now().Add(-time.Hour)}, "open": {created: time.Now()}}
	for _, id := range []string{"missing", "expired", "open"} {
		if _, err := a.ControlDraft(context.Background(), pluginbridge.DraftControlRequest{DraftID: id, Action: "model.switch", Target: "unknown/model"}); err == nil {
			t.Fatalf("invalid draft/model accepted: %s", id)
		}
	}
}

func TestProviderFailureVisibleInHistory(t *testing.T) {
	m := assistantMessage("msg_failed", "", 1)
	m.Raw = map[string]any{"error": map[string]any{"name": "APIError", "data": map[string]any{"message": "Model gateway is unavailable"}}}
	h := historyMessage(m)
	if h.Status != "failed" || h.Content != "OpenCode 运行失败：Model gateway is unavailable" {
		t.Fatalf("provider failure hidden: %+v", h)
	}
}
