package adapter

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	pluginbridge "github.com/Rokid-Prism/prism-plugin-sdk"

	"github.com/Rokid-Prism/prism-plugin-opencode/internal/opencode"
)

func TestListSessionsMapsDirectory(t *testing.T) {
	adapter := testAdapter(&mockClient{listSessions: func(context.Context) ([]opencode.Session, error) {
		return []opencode.Session{
			{ID: "ses_2", Title: "second", Directory: "/tmp/b", Time: opencode.SessionTime{Updated: 2000}},
			{ID: "ses_1", Title: "first", Directory: "/tmp/a", ParentID: "ses_0", Time: opencode.SessionTime{Updated: 1000}},
		}, nil
	}})
	hints, err := adapter.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("ListSessions error: %v", err)
	}
	if len(hints) != 2 {
		t.Fatalf("len(hints) = %d, want 2", len(hints))
	}
	// Client order (newest first) is preserved.
	if hints[0].NativeSessionID != "ses_2" || hints[1].NativeSessionID != "ses_1" {
		t.Fatalf("unexpected order: %s, %s", hints[0].NativeSessionID, hints[1].NativeSessionID)
	}
	first := hints[1]
	if first.Title != "first" || first.Cwd != "/tmp/a" || first.Endpoint != "http://127.0.0.1:4096" || !first.Visible {
		t.Fatalf("unexpected hint mapping: %+v", first)
	}
	if first.Metadata["parent_id"] != "ses_0" {
		t.Fatalf("parent_id metadata missing: %+v", first.Metadata)
	}
	if !first.LastActivityAt.Equal(time.Unix(1, 0).UTC()) {
		t.Fatalf("LastActivityAt = %v", first.LastActivityAt)
	}
}

func TestAttachSession(t *testing.T) {
	tests := []struct {
		name    string
		req     pluginbridge.AttachSessionRequest
		getFunc func(ctx context.Context, id string) (*opencode.Session, error)
		wantErr string
		wantCwd string
	}{
		{
			name: "attaches and refreshes cwd from server",
			req:  pluginbridge.AttachSessionRequest{NativeSessionID: " ses_1 ", Cwd: "/fallback"},
			getFunc: func(_ context.Context, id string) (*opencode.Session, error) {
				if id != "ses_1" {
					t.Errorf("GetSession id = %q, want ses_1", id)
				}
				return &opencode.Session{ID: "ses_1", Title: "t", Directory: "/real/cwd"}, nil
			},
			wantCwd: "/real/cwd",
		},
		{
			name:    "empty native id rejected",
			req:     pluginbridge.AttachSessionRequest{},
			wantErr: "native session id required",
		},
		{
			name: "unknown session fails",
			req:  pluginbridge.AttachSessionRequest{NativeSessionID: "ses_x"},
			getFunc: func(context.Context, string) (*opencode.Session, error) {
				return nil, errors.New("session not found")
			},
			wantErr: "attach session ses_x failed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			adapter := testAdapter(&mockClient{getSession: tt.getFunc})
			session, err := adapter.AttachSession(context.Background(), tt.req)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("AttachSession error: %v", err)
			}
			if session.NativeSessionID != "ses_1" || session.PluginID != "opencode" || !session.Visible {
				t.Fatalf("unexpected session: %+v", session)
			}
			if session.Cwd != tt.wantCwd {
				t.Fatalf("Cwd = %q, want %q", session.Cwd, tt.wantCwd)
			}
		})
	}
}

func TestStartSessionWithMessage(t *testing.T) {
	var prompted struct {
		sessionID string
		text      string
	}
	adapter := New(Config{
		Client: &mockClient{
			createSession: func(_ context.Context, req opencode.CreateSessionRequest) (*opencode.Session, error) {
				if req.Directory != "/work" {
					t.Errorf("CreateSession directory = %q, want /work", req.Directory)
				}
				return &opencode.Session{ID: "ses_new", Directory: "/work"}, nil
			},
			prompt: func(_ context.Context, sessionID, text string) error {
				prompted.sessionID, prompted.text = sessionID, text
				return nil
			},
			listMessages: func(context.Context, string) ([]opencode.Message, error) {
				return []opencode.Message{userMessage("msg_1", "hello opencode", 1000)}, nil
			},
		},
		Endpoint:          "http://127.0.0.1:4096",
		VisibilityTimeout: 2 * time.Second,
	})
	result, err := adapter.StartSessionWithMessage(context.Background(), pluginbridge.StartSessionWithMessageRequest{
		Cwd:     "/work",
		Message: pluginbridge.InboundMessage{PrismMessageID: "pm_1", Text: " hello opencode "},
	})
	if err != nil {
		t.Fatalf("StartSessionWithMessage error: %v", err)
	}
	if result.Session.NativeSessionID != "ses_new" {
		t.Fatalf("session id = %q, want ses_new", result.Session.NativeSessionID)
	}
	if prompted.sessionID != "ses_new" || prompted.text != "hello opencode" {
		t.Fatalf("prompt called with (%q, %q)", prompted.sessionID, prompted.text)
	}
	if !result.Receipt.Accepted || !result.Receipt.Visible {
		t.Fatalf("receipt not accepted: %+v", result.Receipt)
	}
	if !result.Visibility.Visible {
		t.Fatalf("visibility not confirmed: %+v", result.Visibility)
	}
	if !strings.Contains(result.Visibility.Evidence, "hello opencode") {
		t.Fatalf("evidence = %q", result.Visibility.Evidence)
	}
}

func TestStartSessionWithMessageRejectsAttachmentsAndEmptyText(t *testing.T) {
	adapter := testAdapter(&mockClient{})
	if _, err := adapter.StartSessionWithMessage(context.Background(), pluginbridge.StartSessionWithMessageRequest{
		Message: pluginbridge.InboundMessage{
			Text:        "hi",
			Attachments: []pluginbridge.Attachment{{Name: "a.png", LocalPath: "/tmp/a.png"}},
		},
	}); err == nil || !strings.Contains(err.Error(), "attachments") {
		t.Fatalf("attachments error = %v", err)
	}
	if _, err := adapter.StartSessionWithMessage(context.Background(), pluginbridge.StartSessionWithMessageRequest{
		Message: pluginbridge.InboundMessage{Text: "   "},
	}); err == nil || !strings.Contains(err.Error(), "text required") {
		t.Fatalf("empty text error = %v", err)
	}
}

func TestVerifyVisibilityTimesOutWithoutMarker(t *testing.T) {
	adapter := New(Config{
		Client: &mockClient{listMessages: func(context.Context, string) ([]opencode.Message, error) {
			return []opencode.Message{assistantMessage("msg_1", "unrelated", 1000)}, nil
		}},
		Endpoint:          "http://127.0.0.1:4096",
		VisibilityTimeout: 30 * time.Millisecond,
	})
	result, err := adapter.VerifyVisibility(context.Background(), pluginbridge.NativeSession{NativeSessionID: "ses_1"}, "marker")
	if err != nil {
		t.Fatalf("VerifyVisibility error: %v", err)
	}
	if result.Visible || result.FailureReason == "" {
		t.Fatalf("expected invisible result with reason: %+v", result)
	}
	if result.Marker != "marker" {
		t.Fatalf("Marker = %q", result.Marker)
	}
}
