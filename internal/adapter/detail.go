package adapter

import (
	"context"
	"fmt"
	"strings"
	"time"

	pluginbridge "github.com/Rokid-Prism/prism-plugin-sdk"

	"github.com/Rokid-Prism/prism-plugin-opencode/internal/opencode"
)

// Detail snapshot (the readable half of the unified capability contract).
// Field names follow the Hub's canonical detail_snapshot shape; fields the
// opencode server cannot source are omitted rather than fabricated. Only
// controls backed by a real call are advertised in "actions" — currently
// model.switch, matching ControlSession.

var _ pluginbridge.DetailReader = (*Adapter)(nil)

// ReadDetail implements pluginbridge.DetailReader.
func (a *Adapter) ReadDetail(ctx context.Context, session pluginbridge.NativeSession) (map[string]any, error) {
	return a.buildDetailSnapshot(ctx, session)
}

func (a *Adapter) buildDetailSnapshot(ctx context.Context, session pluginbridge.NativeSession) (map[string]any, error) {
	ctx = opencode.WithDirectory(ctx, session.Cwd)
	sessionID := strings.TrimSpace(session.NativeSessionID)
	if sessionID == "" {
		return nil, fmt.Errorf("opencode: native session id required")
	}
	snapshot := map[string]any{
		"plugin_id":          a.ID(),
		"conversation_id":    sessionID,
		"desktop_foreground": false,
		"detail_stale":       false,
		"updated_at":         time.Now().UTC().UnixMilli(),
		"status":             "idle",
		"primary_action":     "send",
		"model_options":      []any{},
		"reasoning_options":  []any{},
		"actions":            []any{},
		"run":                idleRunSnapshot(),
	}
	if cwd := strings.TrimSpace(session.Cwd); cwd != "" {
		snapshot["cwd"] = cwd
	}

	// Session row: title and working directory. A failed read means the
	// session is gone; that is a real error for detail, not a degradation.
	if native, err := a.client.GetSession(ctx, sessionID); err != nil {
		return nil, fmt.Errorf("opencode: read session %s failed: %w", sessionID, err)
	} else if native != nil {
		if model, ok := native.Raw["model"].(map[string]any); ok {
			if provider, id := propString(model, "providerID"), propString(model, "id"); provider != "" && id != "" {
				snapshot["current_model"] = optionObject(provider + "/" + id)
			}
		}
		if title := strings.TrimSpace(native.Title); title != "" {
			snapshot["title"] = title
		}
		if directory := strings.TrimSpace(native.Directory); directory != "" {
			snapshot["cwd"] = directory
		}
	}

	// Current default model from the merged config. opencode config model is
	// already in "providerID/modelID" form.
	if config, err := a.client.GetConfig(ctx); snapshot["current_model"] == nil && err == nil && config != nil && strings.TrimSpace(config.Model) != "" {
		snapshot["current_model"] = optionObject(strings.TrimSpace(config.Model))
	}

	// Model catalog backs both model_options and the advertised actions.
	if models, err := a.client.ListModels(ctx); err == nil && len(models) > 0 {
		options := modelOptions(models)
		snapshot["model_options"] = options
		snapshot["actions"] = []any{
			map[string]any{"id": "model.switch", "label": "切换模型", "available": true},
		}
	}
	return snapshot, nil
}

func idleRunSnapshot() map[string]any {
	return map[string]any{
		"status":           "idle",
		"summary":          "当前会话空闲。",
		"active":           false,
		"interruptible":    false,
		"approval":         nil,
		"approval_blocked": false,
		"primary_action":   "send",
	}
}

// optionObject builds the {key,value,label,displayName,target} option shape
// used by current_model and model_options entries.
func optionObject(key string) map[string]any {
	key = strings.TrimSpace(key)
	return map[string]any{
		"key":         key,
		"value":       key,
		"label":       key,
		"displayName": key,
		"target":      map[string]any{"option_id": key},
	}
}

func modelOptions(models []opencode.ModelInfo) []any {
	out := make([]any, 0, len(models))
	seen := map[string]bool{}
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
		if seen[key] {
			continue
		}
		seen[key] = true
		option := optionObject(key)
		if label := strings.TrimSpace(model.Name); label != "" {
			option["label"] = label
			option["displayName"] = label
		}
		out = append(out, option)
	}
	return out
}
