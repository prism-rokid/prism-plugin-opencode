package adapter

import (
	"context"
	"errors"
	"strings"
	"testing"

	pluginbridge "github.com/Rokid-Prism/prism-plugin-sdk"
)

func TestSendDeliversPrompt(t *testing.T) {
	var gotSession, gotText string
	adapter := testAdapter(&mockClient{prompt: func(_ context.Context, sessionID, text string) error {
		gotSession, gotText = sessionID, text
		return nil
	}})
	receipt, err := adapter.Send(context.Background(),
		pluginbridge.NativeSession{NativeSessionID: "ses_1"},
		pluginbridge.InboundMessage{PrismMessageID: "pm_9", Text: "  fix the bug  "})
	if err != nil {
		t.Fatalf("Send error: %v", err)
	}
	if gotSession != "ses_1" || gotText != "fix the bug" {
		t.Fatalf("Prompt called with (%q, %q)", gotSession, gotText)
	}
	if !receipt.Accepted || !receipt.Visible {
		t.Fatalf("receipt should be accepted+visible: %+v", receipt)
	}
	if receipt.NativeMessageID != "pm_9" {
		t.Fatalf("NativeMessageID = %q, want pm_9", receipt.NativeMessageID)
	}
	if receipt.CanonicalNativeSessionID != "ses_1" {
		t.Fatalf("CanonicalNativeSessionID = %q", receipt.CanonicalNativeSessionID)
	}
}

func TestSendRejectsBadInput(t *testing.T) {
	adapter := testAdapter(&mockClient{})
	tests := []struct {
		name    string
		session pluginbridge.NativeSession
		msg     pluginbridge.InboundMessage
		wantErr string
	}{
		{"missing session", pluginbridge.NativeSession{}, pluginbridge.InboundMessage{Text: "x"}, "native session id required"},
		{"empty text", pluginbridge.NativeSession{NativeSessionID: "ses_1"}, pluginbridge.InboundMessage{Text: "  "}, "text required"},
		{
			"attachments unsupported",
			pluginbridge.NativeSession{NativeSessionID: "ses_1"},
			pluginbridge.InboundMessage{Text: "x", Attachments: []pluginbridge.Attachment{{Name: "f.txt", LocalPath: "/tmp/f.txt"}}},
			"attachments",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := adapter.Send(context.Background(), tt.session, tt.msg); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestSendPropagatesPromptFailure(t *testing.T) {
	adapter := testAdapter(&mockClient{prompt: func(context.Context, string, string) error {
		return errors.New("500 Internal Server Error")
	}})
	if _, err := adapter.Send(context.Background(), pluginbridge.NativeSession{NativeSessionID: "ses_1"}, pluginbridge.InboundMessage{Text: "hi"}); err == nil || !strings.Contains(err.Error(), "prompt failed") {
		t.Fatalf("error = %v", err)
	}
}

func TestInterruptAbortsSession(t *testing.T) {
	var aborted string
	adapter := testAdapter(&mockClient{abort: func(_ context.Context, sessionID string) error {
		aborted = sessionID
		return nil
	}})
	if err := adapter.Interrupt(context.Background(), pluginbridge.NativeSession{NativeSessionID: "ses_1"}, "run_ignored"); err != nil {
		t.Fatalf("Interrupt error: %v", err)
	}
	if aborted != "ses_1" {
		t.Fatalf("Abort called with %q, want ses_1", aborted)
	}
}

func TestInterruptRequiresSession(t *testing.T) {
	adapter := testAdapter(&mockClient{})
	if err := adapter.Interrupt(context.Background(), pluginbridge.NativeSession{}, ""); err == nil || !strings.Contains(err.Error(), "session id required") {
		t.Fatalf("error = %v", err)
	}
}
