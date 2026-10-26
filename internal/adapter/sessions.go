package adapter

import (
	"context"
	"fmt"
	"strings"
	"time"

	pluginbridge "github.com/Rokid-Prism/prism-plugin-sdk"

	"github.com/Rokid-Prism/prism-plugin-opencode/internal/opencode"
)

// Native session directory operations: ListSessions (SessionLister),
// AttachSession (SessionAttacher) and StartSessionWithMessage (SessionStarter)
// plus the VerifyVisibility poll used by the starter.

// nativeSessionFrom maps an opencode session onto the Prism NativeSession.
// The opencode session id is the conversation container; there is no separate
// thread concept, so NativeThreadID stays empty.
func (a *Adapter) nativeSessionFrom(session opencode.Session, cwdFallback string) pluginbridge.NativeSession {
	cwd := strings.TrimSpace(session.Directory)
	if cwd == "" {
		cwd = strings.TrimSpace(cwdFallback)
	}
	return pluginbridge.NativeSession{
		PluginID:        a.ID(),
		NativeSessionID: session.ID,
		Surface:         Surface,
		Endpoint:        a.endpoint,
		Cwd:             cwd,
		Visible:         true,
	}
}

// ListSessions implements pluginbridge.SessionLister. The client returns
// sessions newest-updated first; that order is preserved.
func (a *Adapter) ListSessions(ctx context.Context) ([]pluginbridge.NativeSessionHint, error) {
	sessions, err := a.client.ListSessions(ctx)
	if err != nil {
		return nil, fmt.Errorf("opencode: list sessions failed: %w", err)
	}
	hints := make([]pluginbridge.NativeSessionHint, 0, len(sessions))
	for _, session := range sessions {
		if strings.TrimSpace(session.ID) == "" {
			continue
		}
		metadata := map[string]string{}
		if session.ParentID != "" {
			metadata["parent_id"] = session.ParentID
		}
		hints = append(hints, pluginbridge.NativeSessionHint{
			PluginID:        a.ID(),
			NativeSessionID: session.ID,
			Surface:         Surface,
			Endpoint:        a.endpoint,
			Cwd:             strings.TrimSpace(session.Directory),
			Title:           strings.TrimSpace(session.Title),
			LastActivityAt:  msToTime(session.Time.Updated),
			Visible:         true,
			Metadata:        metadata,
		})
	}
	return hints, nil
}

// AttachSession implements pluginbridge.SessionAttacher. The native id is
// validated against the server so attaching to a deleted session fails here
// instead of silently producing a dead conversation.
func (a *Adapter) AttachSession(ctx context.Context, req pluginbridge.AttachSessionRequest) (pluginbridge.NativeSession, error) {
	sessionID := strings.TrimSpace(req.NativeSessionID)
	if sessionID == "" {
		return pluginbridge.NativeSession{}, fmt.Errorf("opencode: native session id required")
	}
	session, err := a.client.GetSession(ctx, sessionID)
	if err != nil {
		return pluginbridge.NativeSession{}, fmt.Errorf("opencode: attach session %s failed: %w", sessionID, err)
	}
	if session == nil {
		return pluginbridge.NativeSession{}, fmt.Errorf("opencode: attach session %s failed: session not found", sessionID)
	}
	return a.nativeSessionFrom(*session, req.Cwd), nil
}

// StartSessionWithMessage implements pluginbridge.SessionStarter: create the
// native session in req.Cwd, deliver the first prompt fire-and-forget, then
// verify the user text became visible in the session body.
func (a *Adapter) StartSessionWithMessage(ctx context.Context, req pluginbridge.StartSessionWithMessageRequest) (pluginbridge.StartSessionWithMessageResult, error) {
	text := strings.TrimSpace(req.Message.Text)
	if text == "" {
		return pluginbridge.StartSessionWithMessageResult{}, fmt.Errorf("opencode: first message text required")
	}
	if len(req.Message.Attachments) > 0 {
		return pluginbridge.StartSessionWithMessageResult{}, fmt.Errorf("opencode: attachments are not supported by the opencode adapter")
	}
	create := opencode.CreateSessionRequest{}
	if cwd := strings.TrimSpace(req.Cwd); cwd != "" {
		create.Directory = cwd
	}
	session, err := a.client.CreateSession(ctx, create)
	if err != nil {
		return pluginbridge.StartSessionWithMessageResult{}, fmt.Errorf("opencode: create session failed: %w", err)
	}
	if session == nil || strings.TrimSpace(session.ID) == "" {
		return pluginbridge.StartSessionWithMessageResult{}, fmt.Errorf("opencode: create session returned no session id")
	}
	native := a.nativeSessionFrom(*session, req.Cwd)
	receipt, err := a.Send(ctx, native, req.Message)
	if err != nil {
		return pluginbridge.StartSessionWithMessageResult{}, err
	}
	visibility, err := a.VerifyVisibility(ctx, native, text)
	if err != nil {
		return pluginbridge.StartSessionWithMessageResult{}, err
	}
	return pluginbridge.StartSessionWithMessageResult{
		Session:    native,
		Receipt:    receipt,
		Visibility: visibility,
	}, nil
}

// VerifyVisibility polls the session body until the marker text appears in
// any message (the echoed user prompt) or the deadline passes. prompt_async
// is fire-and-forget, so a short poll window bridges the accept/persist gap.
func (a *Adapter) VerifyVisibility(ctx context.Context, session pluginbridge.NativeSession, marker string) (pluginbridge.VisibilityResult, error) {
	sessionID := strings.TrimSpace(session.NativeSessionID)
	marker = strings.TrimSpace(marker)
	checkedAt := time.Now().UTC()
	if sessionID == "" {
		return pluginbridge.VisibilityResult{Marker: marker, CheckedAt: checkedAt, FailureReason: "native session id required"}, nil
	}
	if marker == "" {
		return pluginbridge.VisibilityResult{Marker: marker, CheckedAt: checkedAt, FailureReason: "marker required"}, nil
	}
	deadline := time.Now().Add(a.visibilityTimeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	for {
		messages, err := a.client.ListMessages(ctx, sessionID)
		checkedAt = time.Now().UTC()
		if err != nil {
			return pluginbridge.VisibilityResult{Marker: marker, CheckedAt: checkedAt, FailureReason: err.Error()}, err
		}
		if evidence, ok := findMarkerEvidence(messages, marker); ok {
			return pluginbridge.VisibilityResult{
				Visible:   true,
				Marker:    marker,
				Evidence:  evidence,
				CheckedAt: checkedAt,
			}, nil
		}
		if time.Now().After(deadline) {
			return pluginbridge.VisibilityResult{
				Visible:       false,
				Marker:        marker,
				CheckedAt:     checkedAt,
				FailureReason: "marker not found in session messages",
			}, nil
		}
		select {
		case <-ctx.Done():
			return pluginbridge.VisibilityResult{Marker: marker, CheckedAt: checkedAt, FailureReason: ctx.Err().Error()}, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// findMarkerEvidence searches every message body for the marker and returns
// a bounded snippet around the first match.
func findMarkerEvidence(messages []opencode.Message, marker string) (string, bool) {
	for _, message := range messages {
		text := messageText(message)
		index := strings.Index(text, marker)
		if index < 0 {
			continue
		}
		prefix := []rune(text[:index])
		suffix := []rune(text[index+len(marker):])
		if len(prefix) > 80 {
			prefix = prefix[len(prefix)-80:]
		}
		if len(suffix) > 80 {
			suffix = suffix[:80]
		}
		return strings.TrimSpace(string(prefix) + marker + string(suffix)), true
	}
	return "", false
}
