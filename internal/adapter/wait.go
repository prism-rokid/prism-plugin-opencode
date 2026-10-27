package adapter

import (
	"context"
	"fmt"
	"strings"
	"time"

	pluginbridge "github.com/Rokid-Prism/prism-plugin-sdk"

	"github.com/Rokid-Prism/prism-plugin-opencode/internal/opencode"
)

// WaitForRun waits for the terminal event of one turn. opencode has no native
// run id: prompt_async returns 204 and one session runs at most one turn at a
// time, so the terminal signal is the session's own idle transition —
// session.status{status:{type:"idle"}} on opencode 1.19+, with session.idle
// accepted as the equivalent legacy signal. A timeout (WaitRunTimeout, or the
// caller's earlier deadline) is the fallback so a lost event cannot hang the
// Hub forever. runID, when provided by the Hub, only labels the returned event.

// WaitForRun implements pluginbridge.RunWaiter.
func (a *Adapter) WaitForRun(ctx context.Context, session pluginbridge.NativeSession, runID string) (pluginbridge.PluginEvent, error) {
	ctx = opencode.WithDirectory(ctx, session.Cwd)
	sessionID := strings.TrimSpace(session.NativeSessionID)
	if sessionID == "" {
		return pluginbridge.PluginEvent{}, fmt.Errorf("opencode: native session id required")
	}
	timeout := a.waitRunTimeout
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining > 0 && remaining < timeout {
			timeout = remaining
		}
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	idle := make(chan string, 1)
	streamDone := make(chan error, 1)
	go func() {
		streamDone <- a.client.StreamEvents(waitCtx, func(ev opencode.Event) error {
			props := eventProperties(ev)
			if !isSessionIdleEvent(ev, props) {
				return nil
			}
			if !eventMatchesSession(props, sessionID) {
				return nil
			}
			select {
			case idle <- terminalReason(ev.Type, props):
			default:
			}
			return nil
		})
	}()
	// V2 event subscriptions are live-only. A fast reply can finish between
	// Send and WaitForRun; recover from its persisted user/idle boundary.
	if recovery, ok := a.client.(interface {
		RunOutcome(context.Context, string) (string, bool, error)
	}); ok {
		go func() {
			ticker := time.NewTicker(500 * time.Millisecond)
			defer ticker.Stop()
			for {
				reason, done, _ := recovery.RunOutcome(waitCtx, sessionID)
				if done {
					select {
					case idle <- reason:
					default:
					}
					return
				}
				select {
				case <-waitCtx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	}

	select {
	case reason := <-idle:
		return a.runTerminalEvent(ctx, sessionID, reason, runID), nil
	case err := <-streamDone:
		if err != nil && ctx.Err() == nil {
			return pluginbridge.PluginEvent{}, fmt.Errorf("opencode: event stream failed while waiting for run: %w", err)
		}
		return pluginbridge.PluginEvent{}, fmt.Errorf("opencode: event stream ended before the session went idle (session %s)", sessionID)
	case <-waitCtx.Done():
		if ctx.Err() != nil {
			return pluginbridge.PluginEvent{}, ctx.Err()
		}
		return pluginbridge.PluginEvent{}, fmt.Errorf("opencode: waitForRun timed out after %s (session %s)", timeout, sessionID)
	}
}
