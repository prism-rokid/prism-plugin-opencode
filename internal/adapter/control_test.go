package adapter

import (
	"context"
	"errors"
	"strings"
	"testing"

	pluginbridge "github.com/Rokid-Prism/prism-plugin-sdk"

	"github.com/Rokid-Prism/prism-plugin-opencode/internal/opencode"
)

const switchTarget = "anthropic/claude-sonnet-4"

func TestSelectConfirmsExactSessionWithoutReadingControls(t *testing.T) {
	a := testAdapter(&mockClient{getSession: func(_ context.Context, id string) (*opencode.Session, error) {
		return &opencode.Session{ID: id}, nil
	}})
	result, err := a.ControlSession(context.Background(), pluginbridge.ControlSessionRequest{
		Session: pluginbridge.NativeSession{NativeSessionID: "ses_1"}, Action: "conversation.select",
	})
	if err != nil || !result.OK || result.ThreadID != "ses_1" || result.Details["selected"] != true || result.DetailsConfirmed {
		t.Fatalf("selection = %+v, error = %v", result, err)
	}
	if _, exists := result.Details["model_options"]; exists {
		t.Fatal("selection must not read or fabricate composer controls")
	}
}

func TestSelectRejectsMissingOrMismatchedSession(t *testing.T) {
	for _, kind := range []string{"deleted", "nil", "different"} {
		t.Run(kind, func(t *testing.T) {
			a := testAdapter(&mockClient{getSession: func(context.Context, string) (*opencode.Session, error) {
				switch kind {
				case "deleted":
					return nil, errors.New("not found")
				case "different":
					return &opencode.Session{ID: "ses_other"}, nil
				default:
					return nil, nil
				}
			}})
			result, err := a.ControlSession(context.Background(), pluginbridge.ControlSessionRequest{
				Session: pluginbridge.NativeSession{NativeSessionID: "ses_1"}, Action: "conversation.select",
			})
			if err == nil || result.OK {
				t.Fatalf("invalid session accepted: %+v, %v", result, err)
			}
		})
	}
}

func controlAdapter(patched *string) *Adapter {
	return testAdapter(&mockClient{
		getSession: func(_ context.Context, id string) (*opencode.Session, error) {
			return &opencode.Session{ID: id, Title: "session", Directory: "/work"}, nil
		},
		getConfig: func(context.Context) (*opencode.Config, error) {
			if patched != nil && *patched != "" {
				return &opencode.Config{Model: *patched}, nil
			}
			return &opencode.Config{Model: "openai/gpt-5"}, nil
		},
		switchModel: func(_ context.Context, _ string, model opencode.ModelRef) error {
			patch := opencode.PatchConfigRequest{Model: model.ProviderID + "/" + model.ID}
			if patched != nil {
				*patched = patch.Model
			}
			return nil
		},
		listModels: func(context.Context) ([]opencode.ModelInfo, error) {
			return []opencode.ModelInfo{
				{ProviderID: "anthropic", ModelID: "claude-sonnet-4", Name: "Claude Sonnet 4"},
				{ProviderID: "openai", ModelID: "gpt-5"},
			}, nil
		},
	})
}

func TestControlSessionModelSwitch(t *testing.T) {
	tests := []struct {
		name   string
		action string
		target any
	}{
		{name: "dotted key with string target", action: "model.switch", target: switchTarget},
		{name: "underscore key accepted", action: "model_switch", target: switchTarget},
		{name: "uppercase key accepted", action: "Model.Switch", target: switchTarget},
		{
			name:   "option object target",
			action: "model.switch",
			target: map[string]any{"option_id": switchTarget, "label": "Claude Sonnet 4"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var patched string
			adapter := controlAdapter(&patched)
			result, err := adapter.ControlSession(context.Background(), pluginbridge.ControlSessionRequest{
				Session: pluginbridge.NativeSession{NativeSessionID: "ses_1"},
				Action:  tt.action,
				Target:  tt.target,
			})
			if err != nil {
				t.Fatalf("ControlSession error: %v", err)
			}
			if !result.OK || !result.DetailsConfirmed {
				t.Fatalf("result = %+v", result)
			}
			if result.Action != "model.switch" {
				t.Fatalf("Action = %q", result.Action)
			}
			if patched != switchTarget {
				t.Fatalf("PatchConfig model = %q, want %q", patched, switchTarget)
			}
			if !strings.Contains(result.Message, switchTarget) {
				t.Fatalf("Message = %q", result.Message)
			}
			current, ok := result.Details["current_model"].(map[string]any)
			if !ok || current["key"] != switchTarget {
				t.Fatalf("Details current_model = %+v, want key %q", result.Details["current_model"], switchTarget)
			}
		})
	}
}

func TestControlSessionModelSwitchCatalogIncludesCurrent(t *testing.T) {
	adapter := controlAdapter(nil)
	snapshot, err := adapter.ReadDetail(context.Background(), pluginbridge.NativeSession{NativeSessionID: "ses_1"})
	if err != nil {
		t.Fatalf("ReadDetail error: %v", err)
	}
	options, ok := snapshot["model_options"].([]any)
	if !ok || len(options) != 2 {
		t.Fatalf("model_options = %+v", snapshot["model_options"])
	}
	first, _ := options[0].(map[string]any)
	if first["key"] != switchTarget || first["label"] != "Claude Sonnet 4" {
		t.Fatalf("first option = %+v", first)
	}
	actions, ok := snapshot["actions"].([]any)
	if !ok || len(actions) != 1 {
		t.Fatalf("actions = %+v", snapshot["actions"])
	}
	if action, _ := actions[0].(map[string]any); action["id"] != "model.switch" {
		t.Fatalf("actions[0] = %+v", actions[0])
	}
}

func TestControlSessionRejections(t *testing.T) {
	tests := []struct {
		name    string
		action  string
		target  any
		wantErr string
	}{
		{
			name:    "unsupported action",
			action:  "pin",
			target:  true,
			wantErr: `unsupported control action "pin" (supported: model.switch, conversation.select)`,
		},
		{
			name:    "missing target",
			action:  "model.switch",
			target:  nil,
			wantErr: "requires a target model",
		},
		{
			name:    "bare model id without provider",
			action:  "model.switch",
			target:  "claude-sonnet-4",
			wantErr: "providerID/modelID",
		},
		{
			name:    "target not in catalog",
			action:  "model.switch",
			target:  "unknown/model-x",
			wantErr: "not advertised",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			adapter := controlAdapter(nil)
			_, err := adapter.ControlSession(context.Background(), pluginbridge.ControlSessionRequest{
				Session: pluginbridge.NativeSession{NativeSessionID: "ses_1"},
				Action:  tt.action,
				Target:  tt.target,
			})
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestControlSessionRequiresSessionID(t *testing.T) {
	adapter := testAdapter(&mockClient{})
	_, err := adapter.ControlSession(context.Background(), pluginbridge.ControlSessionRequest{Action: "model.switch", Target: switchTarget})
	if err == nil || !strings.Contains(err.Error(), "session id required") {
		t.Fatalf("error = %v", err)
	}
}

func TestControlSessionCatalogFailureStillSwitches(t *testing.T) {
	// If the catalog endpoint is temporarily unavailable the switch still
	// reaches the server, which rejects invalid models itself.
	var patched string
	adapter := testAdapter(&mockClient{
		getSession: func(_ context.Context, id string) (*opencode.Session, error) {
			return &opencode.Session{ID: id}, nil
		},
		getConfig: func(context.Context) (*opencode.Config, error) {
			return &opencode.Config{Model: patched}, nil
		},
		switchModel: func(_ context.Context, _ string, model opencode.ModelRef) error {
			patch := opencode.PatchConfigRequest{Model: model.ProviderID + "/" + model.ID}
			patched = patch.Model
			return nil
		},
		listModels: func(context.Context) ([]opencode.ModelInfo, error) {
			return nil, context.DeadlineExceeded
		},
	})
	result, err := adapter.ControlSession(context.Background(), pluginbridge.ControlSessionRequest{
		Session: pluginbridge.NativeSession{NativeSessionID: "ses_1"},
		Action:  "model.switch",
		Target:  switchTarget,
	})
	if err != nil {
		t.Fatalf("ControlSession error: %v", err)
	}
	if patched != switchTarget || !result.OK {
		t.Fatalf("patched = %q, result = %+v", patched, result)
	}
}
