package adapter

import (
	"context"
	"fmt"
	"strings"

	pluginbridge "github.com/Rokid-Prism/prism-plugin-sdk"
)

// Send delivers one user prompt and Interrupt aborts the active run. Output
// does not come back through Send: prompt_async is fire-and-forget and the
// body arrives over the Subscribe / WaitForRun event paths.

// Send implements the forward path: POST the prompt (async) and return a
// receipt. NativeMessageID carries the Prism message id when present because
// prompt_async does not return a native message id.
func (a *Adapter) Send(ctx context.Context, session pluginbridge.NativeSession, msg pluginbridge.InboundMessage) (pluginbridge.SendReceipt, error) {
	sessionID := strings.TrimSpace(session.NativeSessionID)
	if sessionID == "" {
		return pluginbridge.SendReceipt{}, fmt.Errorf("opencode: native session id required")
	}
	if len(msg.Attachments) > 0 {
		return pluginbridge.SendReceipt{}, fmt.Errorf("opencode: attachments are not supported by the opencode adapter")
	}
	text := strings.TrimSpace(msg.Text)
	if text == "" {
		return pluginbridge.SendReceipt{}, fmt.Errorf("opencode: message text required")
	}
	if err := a.client.Prompt(ctx, sessionID, text); err != nil {
		return pluginbridge.SendReceipt{}, fmt.Errorf("opencode: prompt failed: %w", err)
	}
	return pluginbridge.SendReceipt{
		NativeMessageID:          strings.TrimSpace(msg.PrismMessageID),
		CanonicalNativeSessionID: sessionID,
		Accepted:                 true,
		Visible:                  true,
		Detail:                   "prompt accepted (async); output arrives via the event stream",
	}, nil
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
