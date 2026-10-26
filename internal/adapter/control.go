package adapter

import (
	"context"
	"fmt"
	"strings"

	pluginbridge "github.com/Rokid-Prism/prism-plugin-sdk"

	"github.com/Rokid-Prism/prism-plugin-opencode/internal/opencode"
)

// ControlSession is the writable half of the unified capability contract.
// Supported stable key (matching the detail snapshot's advertised actions):
//
//	conversation.select -> validate the server-owned session identity
//	model.switch -> POST /api/session/:id/model {model:{id,providerID}}
//
// The target may be a plain "providerID/modelID" string or an option object
// ({option_id|key|value}). Any other action returns an explicit error; the
// adapter never claims a control it cannot execute.

var _ pluginbridge.SessionController = (*Adapter)(nil)

// normalizeAction lowercases and accepts both "model.switch" and
// "model_switch" forms.
func normalizeAction(action string) string {
	normalized := strings.ToLower(strings.TrimSpace(action))
	normalized = strings.ReplaceAll(normalized, "_", ".")
	normalized = strings.ReplaceAll(normalized, "..", ".")
	return normalized
}

// ControlSession implements pluginbridge.SessionController.
func (a *Adapter) ControlSession(ctx context.Context, req pluginbridge.ControlSessionRequest) (pluginbridge.ControlSessionResult, error) {
	ctx = opencode.WithDirectory(ctx, req.Session.Cwd)
	sessionID := strings.TrimSpace(req.Session.NativeSessionID)
	if sessionID == "" {
		return pluginbridge.ControlSessionResult{}, fmt.Errorf("opencode: native session id required for control")
	}
	action := normalizeAction(req.Action)
	if action == "conversation.select" {
		// Headless sessions have no desktop foreground to switch. Confirm the
		// exact native identity; a deleted or mismatched session must fail.
		native, err := a.client.GetSession(ctx, sessionID)
		if err != nil {
			return pluginbridge.ControlSessionResult{}, fmt.Errorf("opencode: select session %s failed: %w", sessionID, err)
		}
		if native == nil || native.ID != sessionID {
			return pluginbridge.ControlSessionResult{}, fmt.Errorf("opencode: control_target_stale: session %s not found or identity mismatch", sessionID)
		}
		return pluginbridge.ControlSessionResult{
			OK: true, Action: action, ThreadID: sessionID, ConversationID: sessionID,
			Message: "OpenCode 会话已确认。",
			Details: map[string]any{"selected": true, "native_session_id": sessionID},
		}, nil
	}
	if action != "model.switch" {
		return pluginbridge.ControlSessionResult{}, fmt.Errorf("opencode: unsupported control action %q (supported: model.switch, conversation.select)", action)
	}

	target := normalizeControlTarget(req.Target)
	if target == "" {
		return pluginbridge.ControlSessionResult{}, fmt.Errorf("opencode: model.switch requires a target model in providerID/modelID form")
	}
	if !strings.Contains(target, "/") {
		return pluginbridge.ControlSessionResult{}, fmt.Errorf("opencode: model target must be in providerID/modelID form, got %q", target)
	}
	// Best-effort catalog validation: a target the server does not advertise
	// fails fast here instead of producing a broken default model.
	if models, err := a.client.ListModels(ctx); err == nil && !modelTargetExists(models, target) {
		return pluginbridge.ControlSessionResult{}, fmt.Errorf("opencode: model target is not advertised by the opencode server: %s", target)
	}
	provider, model, _ := strings.Cut(target, "/")
	if err := a.client.SwitchSessionModel(ctx, sessionID, opencode.ModelRef{ID: model, ProviderID: provider}); err != nil {
		return pluginbridge.ControlSessionResult{}, fmt.Errorf("opencode: session model switch failed: %w", err)
	}

	result := pluginbridge.ControlSessionResult{
		OK:             true,
		Action:         "model.switch",
		ThreadID:       sessionID,
		ConversationID: sessionID,
		Message:        fmt.Sprintf("已切换 opencode 会话模型为 %s。", target),
	}
	// Return the refreshed detail snapshot synchronously; the Subscribe loop
	// converges asynchronously through later session events.
	if snapshot, err := a.buildDetailSnapshot(ctx, req.Session); err == nil {
		result.Details = snapshot
	}
	result.DetailsConfirmed = true
	return result, nil
}

// normalizeControlTarget coerces Target (string, option object, number) into
// a plain model key.
func normalizeControlTarget(target any) string {
	switch value := target.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(value)
	case map[string]any:
		for _, key := range []string{"option_id", "key", "value", "id", "label", "displayName"} {
			if raw, ok := value[key].(string); ok {
				if trimmed := strings.TrimSpace(raw); trimmed != "" {
					return trimmed
				}
			}
		}
		return ""
	default:
		return strings.TrimSpace(fmt.Sprint(target))
	}
}

func modelTargetExists(models []opencode.ModelInfo, target string) bool {
	target = strings.TrimSpace(target)
	for _, model := range models {
		provider := strings.TrimSpace(model.ProviderID)
		id := strings.TrimSpace(model.ModelID)
		if id == "" {
			continue
		}
		key := id
		if provider != "" && !strings.Contains(id, "/") {
			key = provider + "/" + id
		}
		if key == target || id == target {
			return true
		}
	}
	return false
}
