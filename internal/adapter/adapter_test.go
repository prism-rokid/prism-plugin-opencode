package adapter

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Rokid-Prism/prism-plugin-opencode/internal/opencode"
	pluginbridge "github.com/Rokid-Prism/prism-plugin-sdk"
)

func TestProbeOfflineReportsReason(t *testing.T) {
	tests := []struct {
		name       string
		listErr    error
		wantReason string
	}{
		{
			name:       "server offline sentinel",
			listErr:    opencode.ErrServerOffline,
			wantReason: "opencode server offline",
		},
		{
			name:       "generic transport failure",
			listErr:    errors.New("connection refused"),
			wantReason: "connection refused",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			adapter := testAdapter(&mockClient{listSessions: func(context.Context) ([]opencode.Session, error) {
				return nil, tt.listErr
			}})
			capability, err := adapter.Probe(context.Background())
			if err != nil {
				t.Fatalf("Probe returned error: %v", err)
			}
			if capability.Available {
				t.Fatalf("expected Available=false, got true")
			}
			if !strings.Contains(capability.UnavailableReason, tt.wantReason) {
				t.Fatalf("UnavailableReason = %q, want substring %q", capability.UnavailableReason, tt.wantReason)
			}
			if capability.NativeVisibleInput || capability.NativeVisibleOutput || capability.CanForwardSync {
				t.Fatalf("offline probe must not claim visibility or forward sync: %+v", capability)
			}
		})
	}
}

func TestProbeOnlineDeclaresExactlyImplementedCapabilities(t *testing.T) {
	adapter := testAdapter(&mockClient{listSessions: func(context.Context) ([]opencode.Session, error) {
		return []opencode.Session{}, nil
	}})
	capability, err := adapter.Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe returned error: %v", err)
	}
	if !capability.Available {
		t.Fatalf("expected Available=true, reason %q", capability.UnavailableReason)
	}
	if !capability.NativeVisibleInput || !capability.NativeVisibleOutput || !capability.CanForwardSync {
		t.Fatalf("online probe should claim visibility + forward sync: %+v", capability)
	}

	declaredTrue := map[string]bool{
		"CanAttachSession":           capability.CanAttachSession,
		"CanStartSessionWithMessage": capability.CanStartSessionWithMessage,
		"CanListSessions":            capability.CanListSessions,
		"CanReadHistory":             capability.CanReadHistory,
		"CanInterrupt":               capability.CanInterrupt,
		"CanReverseSync":             capability.CanReverseSync,
		"CanWaitRun":                 capability.CanWaitRun,
		"CanControlSession":          capability.CanControlSession,
		"CanOpenDraft":               capability.CanOpenDraft,
	}
	for name, value := range declaredTrue {
		if !value {
			t.Errorf("capability %s should be declared", name)
		}
	}
	// Not implemented: no ApprovalResolver, no PluginWideSubscriber, no
	// StatusReader, no draft/terminal support.
	if capability.CanApproval || capability.CanPluginWideWatch || capability.CanReadStatus ||
		capability.CanOpenManagedTerminal {
		t.Fatalf("probe must not declare unimplemented capabilities: %+v", capability)
	}
	if capability.IntegrationMode != pluginbridge.IntegrationModeProtocolNative {
		t.Fatalf("IntegrationMode = %q, want protocol-native", capability.IntegrationMode)
	}
	if capability.PluginID != "opencode" || capability.VisibilitySurface != Surface {
		t.Fatalf("unexpected identity fields: %+v", capability)
	}
}

func TestDiscoverListsSessionDirectory(t *testing.T) {
	adapter := testAdapter(&mockClient{listSessions: func(context.Context) ([]opencode.Session, error) {
		return []opencode.Session{
			{ID: "ses_1", Title: "Fix login bug", Time: opencode.SessionTime{Updated: 1000}},
			{ID: "ses_2", Title: "", Time: opencode.SessionTime{Updated: 2000}},
		}, nil
	}})
	result, err := adapter.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	if !result.Verified {
		t.Fatalf("expected Verified=true, detail %q", result.Detail)
	}
	if result.Endpoint != "http://127.0.0.1:4096" || result.Surface != Surface || result.PluginID != "opencode" {
		t.Fatalf("unexpected discovery identity: %+v", result)
	}
	if got := result.SessionHints["ses_1"]; got != "Fix login bug" {
		t.Fatalf("SessionHints[ses_1] = %q", got)
	}
	if got, ok := result.SessionHints["ses_2"]; !ok || got != "" {
		t.Fatalf("SessionHints[ses_2] = %q, ok=%v", got, ok)
	}
}

func TestDiscoverOfflineIsUnverifiedNotError(t *testing.T) {
	adapter := testAdapter(&mockClient{listSessions: func(context.Context) ([]opencode.Session, error) {
		return nil, opencode.ErrServerOffline
	}})
	result, err := adapter.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover should not error when the server is offline: %v", err)
	}
	if result.Verified {
		t.Fatalf("expected Verified=false")
	}
	if !strings.Contains(result.Detail, "offline") {
		t.Fatalf("Detail = %q, want offline reason", result.Detail)
	}
	if len(result.SessionHints) != 0 {
		t.Fatalf("offline discovery must not report session hints")
	}
}

func TestCloseIsNoop(t *testing.T) {
	if err := testAdapter(&mockClient{}).Close(context.Background()); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
}
