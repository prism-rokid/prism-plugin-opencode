package adapter

import (
	"context"
	"encoding/base64"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"strings"

	pluginbridge "github.com/Rokid-Prism/prism-plugin-sdk"

	"github.com/Rokid-Prism/prism-plugin-opencode/internal/opencode"
)

// maxAttachmentBytes caps one attachment before it is inlined as a data URL.
// opencode itself persists multi-megabyte images as data URLs; the cap only
// guards against runaway files.
const maxAttachmentBytes = 20 << 20

// Send delivers one user prompt and Interrupt aborts the active run. Output
// does not come back through Send: prompt_async is fire-and-forget and the
// body arrives over the Subscribe / WaitForRun event paths.

// Send implements the forward path: POST the prompt (async) and return a
// receipt. NativeMessageID carries the Prism message id when present because
// prompt_async does not return a native message id. Attachments are inlined
// as opencode file parts using data URLs — the same form the opencode
// clients use for images.
func (a *Adapter) Send(ctx context.Context, session pluginbridge.NativeSession, msg pluginbridge.InboundMessage) (pluginbridge.SendReceipt, error) {
	sessionID := strings.TrimSpace(session.NativeSessionID)
	if sessionID == "" {
		return pluginbridge.SendReceipt{}, fmt.Errorf("opencode: native session id required")
	}
	text := strings.TrimSpace(msg.Text)
	parts, err := promptParts(text, msg.Attachments)
	if err != nil {
		return pluginbridge.SendReceipt{}, err
	}
	if len(parts) == 0 {
		return pluginbridge.SendReceipt{}, fmt.Errorf("opencode: message text required")
	}
	if err := a.client.PromptWithParts(ctx, sessionID, parts); err != nil {
		return pluginbridge.SendReceipt{}, fmt.Errorf("opencode: prompt failed: %w", err)
	}
	detail := "prompt accepted (async); output arrives via the event stream"
	if len(msg.Attachments) > 0 {
		detail = fmt.Sprintf("prompt accepted with %d attachment(s); output arrives via the event stream", len(msg.Attachments))
	}
	return pluginbridge.SendReceipt{
		NativeMessageID:          strings.TrimSpace(msg.PrismMessageID),
		CanonicalNativeSessionID: sessionID,
		Accepted:                 true,
		Visible:                  true,
		Detail:                   detail,
	}, nil
}

// promptParts builds the opencode parts for one inbound message: a single
// text part (when non-empty) plus one file part per attachment.
func promptParts(text string, attachments []pluginbridge.Attachment) ([]opencode.InputPart, error) {
	parts := make([]opencode.InputPart, 0, 1+len(attachments))
	if text != "" {
		parts = append(parts, opencode.TextInput(text))
	}
	for _, att := range attachments {
		part, err := attachmentPart(att)
		if err != nil {
			return nil, fmt.Errorf("opencode: attachment %q: %w", att.Name, err)
		}
		parts = append(parts, part)
	}
	return parts, nil
}

// attachmentPart inlines one hub-materialized local file as a data-URL file
// part. The opencode API requires mime and url; filename is best-effort.
func attachmentPart(att pluginbridge.Attachment) (opencode.InputPart, error) {
	name := strings.TrimSpace(att.Name)
	path := strings.TrimSpace(att.LocalPath)
	if path == "" {
		return opencode.InputPart{}, fmt.Errorf("local path required")
	}
	info, err := os.Stat(path)
	if err != nil {
		return opencode.InputPart{}, fmt.Errorf("read file: %w", err)
	}
	if info.Size() > maxAttachmentBytes {
		return opencode.InputPart{}, fmt.Errorf("file is %d bytes, over the %d byte inline cap", info.Size(), maxAttachmentBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return opencode.InputPart{}, fmt.Errorf("read file: %w", err)
	}
	mimeType := strings.TrimSpace(att.MIMEType)
	if mimeType == "" {
		if byExt := mime.TypeByExtension(strings.ToLower(filepath.Ext(path))); byExt != "" {
			mimeType = byExt
		} else {
			mimeType = "application/octet-stream"
		}
	}
	if name == "" {
		name = filepath.Base(path)
	}
	dataURL := "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(data)
	return opencode.FileInput(mimeType, name, dataURL), nil
}

// Interrupt aborts the session's active run. opencode aborts per session, so
// taskID is informational only.
func (a *Adapter) Interrupt(ctx context.Context, session pluginbridge.NativeSession, taskID string) error {
	sessionID := strings.TrimSpace(session.NativeSessionID)
	if sessionID == "" {
		return fmt.Errorf("opencode: native session id required")
	}
	if err := a.client.Abort(ctx, sessionID); err != nil {
		return fmt.Errorf("opencode: abort failed: %w", err)
	}
	return nil
}
