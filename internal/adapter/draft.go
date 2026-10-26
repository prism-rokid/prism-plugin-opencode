package adapter

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Rokid-Prism/prism-plugin-opencode/internal/opencode"
	pluginbridge "github.com/Rokid-Prism/prism-plugin-sdk"
)

// Drafts are private composer state. Opening one never creates a native
// conversation; the native session is created only when the first message starts.
type draftState struct {
	cwd, model      string
	created         time.Time
	session         *pluginbridge.NativeSession
	receipt         *pluginbridge.SendReceipt
	messageID, text string
	result          *pluginbridge.StartSessionWithMessageResult
}

var (
	_ pluginbridge.DraftOpener     = (*Adapter)(nil)
	_ pluginbridge.DraftController = (*Adapter)(nil)
	_ pluginbridge.DraftStarter    = (*Adapter)(nil)
)

func (a *Adapter) draftControls(ctx context.Context, draft *draftState) (map[string]any, error) {
	ctx = opencode.WithDirectory(ctx, draft.cwd)
	models, err := a.client.ListModels(ctx)
	if err != nil {
		return nil, fmt.Errorf("opencode: draft model catalog: %w", err)
	}
	controls := map[string]any{
		"model_options": modelOptions(models),
		"actions":       []any{map[string]any{"id": "model.switch", "available": true}},
	}
	if draft.model != "" {
		controls["current_model"] = optionObject(draft.model)
	}
	return controls, nil
}

func (a *Adapter) OpenDraft(ctx context.Context, req pluginbridge.DraftOpenRequest) (pluginbridge.DraftOpenResult, error) {
	id := strings.TrimSpace(req.DraftID)
	if id == "" {
		return pluginbridge.DraftOpenResult{}, fmt.Errorf("opencode: draft id required")
	}
	draft := &draftState{cwd: strings.TrimSpace(req.Cwd), created: time.Now()}
	if cfg, err := a.client.GetConfig(opencode.WithDirectory(ctx, draft.cwd)); err == nil && cfg != nil {
		draft.model = cfg.Model
	}
	controls, err := a.draftControls(ctx, draft)
	if err != nil {
		return pluginbridge.DraftOpenResult{}, err
	}
	a.draftMu.Lock()
	defer a.draftMu.Unlock()
	if a.drafts == nil {
		a.drafts = map[string]*draftState{}
	}
	for key, value := range a.drafts {
		if time.Since(value.created) > 30*time.Minute {
			delete(a.drafts, key)
		}
	}
	if _, exists := a.drafts[id]; exists || len(a.drafts) >= 256 {
		return pluginbridge.DraftOpenResult{}, fmt.Errorf("opencode: draft_stale: duplicate draft or draft limit reached")
	}
	a.drafts[id] = draft
	return pluginbridge.DraftOpenResult{DraftID: id, Cwd: draft.cwd, Controls: controls, DraftFingerprint: id}, nil
}

func (a *Adapter) getDraft(id string) (*draftState, error) {
	draft := a.drafts[strings.TrimSpace(id)]
	if draft == nil || time.Since(draft.created) > 30*time.Minute {
		return nil, fmt.Errorf("opencode: draft_stale: draft not found or expired")
	}
	return draft, nil
}

func (a *Adapter) ControlDraft(ctx context.Context, req pluginbridge.DraftControlRequest) (pluginbridge.DraftControlResult, error) {
	a.draftMu.Lock()
	defer a.draftMu.Unlock()
	draft, err := a.getDraft(req.DraftID)
	if err != nil {
		return pluginbridge.DraftControlResult{}, err
	}
	if draft.session != nil {
		return pluginbridge.DraftControlResult{}, fmt.Errorf("opencode: draft_stale: draft already started")
	}
	if normalizeAction(req.Action) != "model.switch" {
		return pluginbridge.DraftControlResult{}, fmt.Errorf("opencode: unsupported draft control %q", req.Action)
	}
	models, err := a.client.ListModels(opencode.WithDirectory(ctx, draft.cwd))
	if err != nil {
		return pluginbridge.DraftControlResult{}, err
	}
	target := normalizeControlTarget(req.Target)
	if !modelTargetExists(models, target) {
		return pluginbridge.DraftControlResult{}, fmt.Errorf("opencode: draft model not advertised: %s", target)
	}
	draft.model = target
	controls, err := a.draftControls(ctx, draft)
	if err != nil {
		return pluginbridge.DraftControlResult{}, err
	}
	return pluginbridge.DraftControlResult{DraftID: req.DraftID, Cwd: draft.cwd, Controls: controls}, nil
}

func (a *Adapter) StartDraftWithMessage(ctx context.Context, req pluginbridge.StartDraftWithMessageRequest) (pluginbridge.StartSessionWithMessageResult, error) {
	a.draftMu.Lock()
	defer a.draftMu.Unlock()
	draft, err := a.getDraft(req.DraftID)
	if err != nil {
		return pluginbridge.StartSessionWithMessageResult{}, err
	}
	if strings.TrimSpace(req.Message.Text) == "" || len(req.Message.Attachments) > 0 {
		return pluginbridge.StartSessionWithMessageResult{}, fmt.Errorf("opencode: draft requires text without attachments")
	}
	if strings.TrimSpace(req.Cwd) != "" && strings.TrimSpace(req.Cwd) != draft.cwd {
		return pluginbridge.StartSessionWithMessageResult{}, fmt.Errorf("opencode: draft_stale: working directory changed")
	}
	if draft.session != nil && (draft.messageID != req.Message.PrismMessageID || draft.text != req.Message.Text) {
		return pluginbridge.StartSessionWithMessageResult{}, fmt.Errorf("opencode: draft_stale: first message changed")
	}
	if draft.result != nil {
		return *draft.result, nil
	}
	if draft.session == nil {
		create := opencode.CreateSessionRequest{Directory: draft.cwd}
		if draft.model != "" {
			provider, model, ok := strings.Cut(draft.model, "/")
			if !ok {
				return pluginbridge.StartSessionWithMessageResult{}, fmt.Errorf("opencode: invalid draft model")
			}
			create.Model = &opencode.ModelRef{ID: model, ProviderID: provider}
		}
		native, err := a.client.CreateSession(ctx, create)
		if err != nil {
			return pluginbridge.StartSessionWithMessageResult{}, err
		}
		if native == nil || native.ID == "" {
			return pluginbridge.StartSessionWithMessageResult{}, fmt.Errorf("opencode: create session returned no identity")
		}
		session := a.nativeSessionFrom(*native, draft.cwd)
		draft.session = &session
		draft.messageID, draft.text = req.Message.PrismMessageID, req.Message.Text
	}
	if draft.receipt == nil {
		receipt, err := a.Send(ctx, *draft.session, req.Message)
		if err != nil {
			return pluginbridge.StartSessionWithMessageResult{}, err
		}
		draft.receipt = &receipt
	}
	visibility, err := a.VerifyVisibility(ctx, *draft.session, req.Message.Text)
	if err != nil {
		return pluginbridge.StartSessionWithMessageResult{}, err
	}
	result := pluginbridge.StartSessionWithMessageResult{Session: *draft.session, Receipt: *draft.receipt, Visibility: visibility}
	if visibility.Visible {
		draft.result = &result
	}
	return result, nil
}
