package adapter

import (
	"context"
	"fmt"
	"strings"
	"time"

	pluginbridge "github.com/Rokid-Prism/prism-plugin-sdk"

	"github.com/Rokid-Prism/prism-plugin-opencode/internal/opencode"
)

// Subscribe translates one opencode SSE event stream into PluginEvents for a
// single session. Native event mapping (stable Prism-facing keys, never
// native RPC names):
//
//	session.updated          -> session.updated      (status/summary change)
//	message.updated          -> message.<role>       (assistant output / user echo)
//	message.part.updated     -> message.part.updated (streaming output)
//	session.status(type=idle)-> run.completed / run.failed (aborted) terminal
//	session.idle             -> run.completed / run.failed (legacy idle signal)
//	permission.asked         -> approval.required    (approval panel)
//	permission.responded     -> approval.resolve
//	session.deleted          -> session.deleted
//
// session.status(type=busy) transitions and transport-level events
// (server.connected, server.heartbeat) are never forwarded. Events that cannot
// be attributed to the subscribed session are dropped.

// Subscribe implements pluginbridge.PluginAdapter. The returned channel
// closes when ctx is cancelled (the StdioServer cancels it on unsubscribe).
func (a *Adapter) Subscribe(ctx context.Context, session pluginbridge.NativeSession) (<-chan pluginbridge.PluginEvent, error) {
	ctx = opencode.WithDirectory(ctx, session.Cwd)
	sessionID := strings.TrimSpace(session.NativeSessionID)
	if sessionID == "" {
		return nil, fmt.Errorf("opencode: native session id required")
	}
	out := make(chan pluginbridge.PluginEvent, 32)
	go func() {
		defer close(out)
		err := a.client.StreamEvents(ctx, func(ev opencode.Event) error {
			props := eventProperties(ev)
			if props == nil {
				return nil
			}
			if !eventMatchesSession(props, sessionID) {
				return nil
			}
			translated, ok := a.translateSessionEvent(ctx, sessionID, ev.Type, props)
			if !ok {
				return nil
			}
			if !sendPluginEvent(ctx, out, translated) {
				return fmt.Errorf("opencode: subscription consumer closed")
			}
			return nil
		})
		if err != nil && ctx.Err() == nil {
			sendPluginEvent(ctx, out, pluginbridge.PluginEvent{
				ID:        sessionID + ":subscription-error",
				Type:      "subscription.error",
				Status:    "failed",
				Summary:   "opencode 事件流中断：" + err.Error(),
				CreatedAt: time.Now().UTC(),
			})
		}
	}()
	return out, nil
}

// translateSessionEvent converts one attributed native event. ok=false means
// the event carries nothing the Hub consumes.
func (a *Adapter) translateSessionEvent(ctx context.Context, sessionID, eventType string, props map[string]any) (pluginbridge.PluginEvent, bool) {
	switch eventType {
	case "session.execution.succeeded", "session.execution.failed", "session.execution.interrupted":
		return a.runTerminalEvent(ctx, sessionID, terminalReason(eventType, props), ""), true
	case "session.text.delta", "session.text.ended", "session.step.started", "session.step.ended", "session.inbox.enqueued", "session.tool.called", "session.tool.success", "session.tool.failed":
		return pluginbridge.PluginEvent{ID: sessionID + ":" + eventType, Type: "session.updated", Status: "running", Summary: "OpenCode 会话内容已更新。", Payload: map[string]any{"session_id": sessionID}, CreatedAt: time.Now().UTC()}, true
	case "session.renamed", "session.model.selected":
		return pluginbridge.PluginEvent{ID: sessionID + ":" + eventType, Type: "session.updated", Status: "idle", Summary: firstNonEmpty(propString(props, "title"), "OpenCode 会话已更新。"), Payload: map[string]any{"session_id": sessionID}, CreatedAt: time.Now().UTC()}, true
	case "session.updated":
		title := sessionEventTitle(props)
		return pluginbridge.PluginEvent{
			ID:        sessionID + ":updated",
			Type:      "session.updated",
			Status:    "idle",
			Summary:   firstNonEmpty(title, "opencode 会话已更新。"),
			Payload:   map[string]any{"session_id": sessionID, "title": title},
			CreatedAt: time.Now().UTC(),
		}, true
	case "message.updated":
		return translateMessageUpdated(sessionID, props), true
	case "message.part.updated":
		return translateMessagePartUpdated(sessionID, props), true
	case "session.status":
		// opencode 1.19+ run lifecycle signal; only the idle transition is
		// terminal (busy is the run starting, which message.* already covers).
		if sessionStatusType(props) != "idle" {
			return pluginbridge.PluginEvent{}, false
		}
		return a.runTerminalEvent(ctx, sessionID, idleReason(props), ""), true
	case "session.idle":
		return a.runTerminalEvent(ctx, sessionID, idleReason(props), ""), true
	case "permission.asked":
		return translatePermissionAsked(sessionID, props), true
	case "permission.responded":
		return translatePermissionResponded(sessionID, props), true
	case "session.deleted":
		return pluginbridge.PluginEvent{
			ID:        sessionID + ":deleted",
			Type:      "session.deleted",
			Status:    "completed",
			Summary:   "opencode 会话已删除。",
			Payload:   map[string]any{"session_id": sessionID},
			CreatedAt: time.Now().UTC(),
		}, true
	default:
		return pluginbridge.PluginEvent{}, false
	}
}

// sessionEventTitle reads the session title from a session.updated event:
// opencode v1 nests the session object under "info"; "session" is tolerated.
func sessionEventTitle(props map[string]any) string {
	for _, key := range []string{"info", "session"} {
		if session, ok := props[key].(map[string]any); ok {
			if title := propString(session, "title"); title != "" {
				return title
			}
		}
	}
	return ""
}

// translateMessageUpdated maps message.updated. Assistant messages stream
// until the idle signal lands, so they stay Status "running"; user echoes are
// complete the moment the server persists them. The raw message rides in
// properties.info (v1 wire shape); message.updated info usually carries no
// parts, in which case the summary falls back and the text arrives via
// message.part.updated events.
func translateMessageUpdated(sessionID string, props map[string]any) pluginbridge.PluginEvent {
	message := eventMessageObject(props)
	role := rawMessageRole(message)
	text := rawMessageText(message)
	status := "running"
	if role != "assistant" {
		status = "completed"
	}
	eventType := "message.updated"
	if role != "" {
		eventType = "message." + role
	}
	summary := strings.Join(strings.Fields(text), " ")
	if strings.TrimSpace(summary) == "" {
		switch role {
		case "assistant":
			summary = "opencode 正在生成回复。"
		case "user":
			summary = "opencode 已记录用户消息。"
		default:
			summary = "opencode 会话有新消息。"
		}
	}
	messageID := eventMessageID(props)
	event := pluginbridge.PluginEvent{
		ID:      firstNonEmpty(messageID, sessionID+":message"),
		Type:    eventType,
		Status:  status,
		Summary: trimRunes(summary, 240),
		Payload: map[string]any{
			"session_id": sessionID,
			"message_id": messageID,
			"role":       role,
			"text":       text,
			"streaming":  status == "running",
		},
		CreatedAt: time.Now().UTC(),
	}
	return event
}

// translateMessagePartUpdated maps a streaming part update. Only text parts
// carry user-visible output; other part types surface as progress metadata.
func translateMessagePartUpdated(sessionID string, props map[string]any) pluginbridge.PluginEvent {
	part, _ := props["part"].(map[string]any)
	partType := propString(part, "type")
	partText := propString(part, "text")
	messageID := eventMessageID(props)
	partID := propString(props, "partID", "partId", "id")
	if partID == "" && part != nil {
		partID = propString(part, "id")
	}
	payload := map[string]any{
		"session_id": sessionID,
		"message_id": messageID,
		"part_type":  partType,
	}
	if partType == "text" {
		payload["text"] = partText
	}
	event := pluginbridge.PluginEvent{
		ID:        firstNonEmpty(partID, messageID+":part"),
		Type:      "message.part.updated",
		Status:    "running",
		Summary:   firstNonEmpty(trimRunes(strings.Join(strings.Fields(partText), " "), 240), "opencode 正在输出。"),
		Payload:   payload,
		CreatedAt: time.Now().UTC(),
	}
	return event
}

// translatePermissionAsked surfaces a pending native permission as an
// approval panel. The opencode client surface has no permission-respond API,
// so the panel is informational; the adapter does not declare CanApproval.
func translatePermissionAsked(sessionID string, props map[string]any) pluginbridge.PluginEvent {
	permission, _ := props["permission"].(map[string]any)
	permissionID := propString(props, "permissionID", "permissionId")
	if permissionID == "" && permission != nil {
		permissionID = propString(permission, "id")
	}
	title := propString(permission, "title")
	kind := propString(permission, "type")
	pattern := propString(permission, "pattern")
	payload := map[string]any{
		"session_id":          sessionID,
		"approval_request_id": permissionID,
		"title":               title,
		"resolvable":          false,
	}
	if kind != "" {
		payload["kind"] = kind
	}
	if pattern != "" {
		payload["pattern"] = pattern
	}
	return pluginbridge.PluginEvent{
		ID:        firstNonEmpty(permissionID, sessionID+":permission"),
		Type:      "approval.required",
		Status:    "waiting_approval",
		Summary:   firstNonEmpty(title, kind, "opencode 正在等待权限确认。"),
		Payload:   payload,
		CreatedAt: time.Now().UTC(),
	}
}

func translatePermissionResponded(sessionID string, props map[string]any) pluginbridge.PluginEvent {
	permissionID := propString(props, "permissionID", "permissionId")
	return pluginbridge.PluginEvent{
		ID:      firstNonEmpty(permissionID, sessionID+":permission") + ":resolved",
		Type:    "approval.resolve",
		Status:  "completed",
		Summary: "opencode 权限已处理。",
		Payload: map[string]any{
			"session_id":          sessionID,
			"approval_request_id": permissionID,
		},
		CreatedAt: time.Now().UTC(),
	}
}

// runTerminalEvent builds the terminal run event for an idle transition
// (session.status type=idle, or the legacy session.idle). An "aborted" reason
// maps to run.failed. The summary prefers the latest assistant body text so
// remote clients see the final answer.
func (a *Adapter) runTerminalEvent(ctx context.Context, sessionID, reason, runID string) pluginbridge.PluginEvent {
	status := "completed"
	eventType := "run.completed"
	summary, failure := a.latestAssistantResult(ctx, sessionID, 600)
	// Real v1 aborts emit plain idle without a reason. The terminal cause is
	// persisted on the newest assistant's info.error instead.
	if len(failure) > 0 {
		status = "failed"
		eventType = "run.failed"
		if propString(failure, "name") == "MessageAbortedError" {
			reason = "aborted"
		} else {
			data, _ := failure["data"].(map[string]any)
			summary = firstNonEmpty(propString(data, "message"), propString(failure, "message"), propString(failure, "name", "type"), "opencode 运行失败。")
		}
	}
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "failed":
		status = "failed"
		eventType = "run.failed"
		if len(failure) == 0 || summary == "" {
			summary = "opencode 运行失败。"
		}
	case "aborted", "cancelled", "canceled", "interrupted":
		status = "failed"
		eventType = "run.failed"
		summary = "opencode 运行已被中断。"
	}
	if summary == "" {
		summary = "opencode 已完成。"
	}
	payload := map[string]any{"session_id": sessionID}
	if strings.TrimSpace(runID) != "" {
		payload["run_id"] = strings.TrimSpace(runID)
	}
	if strings.TrimSpace(reason) != "" {
		payload["reason"] = strings.TrimSpace(reason)
	}
	return pluginbridge.PluginEvent{
		ID:        firstNonEmpty(runID, sessionID+":idle"),
		Type:      eventType,
		Status:    status,
		Summary:   summary,
		Payload:   payload,
		CreatedAt: time.Now().UTC(),
	}
}
