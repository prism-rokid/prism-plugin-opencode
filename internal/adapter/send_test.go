package adapter

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pluginbridge "github.com/Rokid-Prism/prism-plugin-sdk"

	"github.com/Rokid-Prism/prism-plugin-opencode/internal/opencode"
)

func TestSendDeliversPrompt(t *testing.T) {
	var gotSession string
	var gotParts []opencode.InputPart
	adapter := testAdapter(&mockClient{promptWithParts: func(_ context.Context, sessionID string, parts []opencode.InputPart) error {
		gotSession, gotParts = sessionID, parts
		return nil
	}})
	receipt, err := adapter.Send(context.Background(),
		pluginbridge.NativeSession{NativeSessionID: "ses_1"},
		pluginbridge.InboundMessage{PrismMessageID: "pm_9", Text: "  fix the bug  "})
	if err != nil {
		t.Fatalf("Send error: %v", err)
	}
	if gotSession != "ses_1" || len(gotParts) != 1 || gotParts[0] != opencode.TextInput("fix the bug") {
		t.Fatalf("PromptWithParts called with (%q, %+v)", gotSession, gotParts)
	}
	if !receipt.Accepted || !receipt.Visible {
		t.Fatalf("receipt should be accepted+visible: %+v", receipt)
	}
	if receipt.NativeMessageID != "pm_9" {
		t.Fatalf("NativeMessageID = %q, want pm_9", receipt.NativeMessageID)
	}
	if receipt.CanonicalNativeSessionID != "ses_1" {
		t.Fatalf("CanonicalNativeSessionID = %q, want ses_1", receipt.CanonicalNativeSessionID)
	}
}

func TestSendInlinesAttachmentsAsFileParts(t *testing.T) {
	file := filepath.Join(t.TempDir(), "shot.png")
	png := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
	if err := os.WriteFile(file, png, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	var gotParts []opencode.InputPart
	adapter := testAdapter(&mockClient{promptWithParts: func(_ context.Context, _ string, parts []opencode.InputPart) error {
		gotParts = parts
		return nil
	}})
	receipt, err := adapter.Send(context.Background(),
		pluginbridge.NativeSession{NativeSessionID: "ses_1"},
		pluginbridge.InboundMessage{
			Text: "what is this",
			Attachments: []pluginbridge.Attachment{
				{Name: "shot.png", MIMEType: "image/png", LocalPath: file},
				{Name: "doc.pdf", LocalPath: file}, // no mime: must not crash
			},
		})
	if err != nil {
		t.Fatalf("Send error: %v", err)
	}
	if !receipt.Accepted {
		t.Fatalf("receipt should be accepted: %+v", receipt)
	}
	if len(gotParts) != 3 || gotParts[0] != opencode.TextInput("what is this") {
		t.Fatalf("first part should be text, got %+v", gotParts)
	}
	first, second := gotParts[1], gotParts[2]
	if first.Type != "file" || first.Mime != "image/png" || first.Filename != "shot.png" {
		t.Fatalf("image part = %+v", first)
	}
	wantURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
	if first.URL != wantURL {
		t.Fatalf("image data URL = %q", first.URL)
	}
	if second.Mime == "" || second.Filename != "doc.pdf" {
		t.Fatalf("fallback part = %+v", second)
	}
	if !strings.Contains(receipt.Detail, "2 attachment") {
		t.Fatalf("Detail = %q", receipt.Detail)
	}
}

func TestSendAllowsAttachmentOnlyMessage(t *testing.T) {
	file := filepath.Join(t.TempDir(), "img.jpg")
	if err := os.WriteFile(file, []byte("jpeg"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	var gotParts []opencode.InputPart
	adapter := testAdapter(&mockClient{promptWithParts: func(_ context.Context, _ string, parts []opencode.InputPart) error {
		gotParts = parts
		return nil
	}})
	if _, err := adapter.Send(context.Background(),
		pluginbridge.NativeSession{NativeSessionID: "ses_1"},
		pluginbridge.InboundMessage{Attachments: []pluginbridge.Attachment{{Name: "img.jpg", MIMEType: "image/jpeg", LocalPath: file}}}); err != nil {
		t.Fatalf("attachment-only Send error: %v", err)
	}
	if len(gotParts) != 1 || gotParts[0].Type != "file" {
		t.Fatalf("parts = %+v", gotParts)
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
			"attachment file missing",
			pluginbridge.NativeSession{NativeSessionID: "ses_1"},
			pluginbridge.InboundMessage{Text: "x", Attachments: []pluginbridge.Attachment{{Name: "f.txt", LocalPath: "/nonexistent/f.txt"}}},
			"read file",
		},
		{
			"attachment without path",
			pluginbridge.NativeSession{NativeSessionID: "ses_1"},
			pluginbridge.InboundMessage{Text: "x", Attachments: []pluginbridge.Attachment{{Name: "f.txt"}}},
			"local path required",
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
	adapter := testAdapter(&mockClient{promptWithParts: func(context.Context, string, []opencode.InputPart) error {
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
