package adapter

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	pluginbridge "github.com/Rokid-Prism/prism-plugin-sdk"

	"github.com/Rokid-Prism/prism-plugin-opencode/internal/opencode"
)

// This file holds the native-event plumbing shared by Subscribe, WaitForRun
// and the live history stream: defensive parsing of opencode SSE event
// properties (camelCase JSON forwarded raw by the opencode client) and the
// small stable-key helpers used on the Prism-facing side.

// eventProperties decodes an event's raw properties object. A missing or
// malformed payload yields nil.
func eventProperties(ev opencode.Event) map[string]any {
	if len(ev.Properties) == 0 {
		return nil
	}
	var props map[string]any
	if err := json.Unmarshal(ev.Properties, &props); err != nil {
		return nil
	}
	return props
}

// propString returns the first non-empty trimmed string among keys.
func propString(props map[string]any, keys ...string) string {
	if props == nil {
		return ""
	}
	for _, key := range keys {
		if value, ok := props[key].(string); ok {
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

// eventSessionID extracts the session a native event belongs to. Unknown
// attribution returns "" so callers can drop the event rather than broadcast
// it to every subscriber.
func eventSessionID(props map[string]any) string {
	if props == nil {
		return ""
	}
	if id := propString(props, "sessionID", "sessionId"); id != "" {
		return id
	}
	if session, ok := props["session"].(map[string]any); ok {
		return propString(session, "id", "sessionID")
	}
	return ""
}

// eventMatchesSession reports whether a native event belongs to sessionID.
func eventMatchesSession(props map[string]any, sessionID string) bool {
	observed := eventSessionID(props)
	return observed != "" && observed == sessionID
}

// eventMessageID extracts the native message id from message.* events.
func eventMessageID(props map[string]any) string {
	if props == nil {
		return ""
	}
	if id := propString(props, "messageID", "messageId"); id != "" {
		return id
	}
	if message := eventMessageObject(props); message != nil {
		return propString(message, "id")
	}
	if part, ok := props["part"].(map[string]any); ok {
		return propString(part, "messageID", "messageId")
	}
	return ""
}

// eventMessageObject returns the raw message info object carried by
// message.* events. opencode v1 (verified on 1.19.4) nests it under "info";
// "message" is tolerated as an alternative spelling.
func eventMessageObject(props map[string]any) map[string]any {
	if props == nil {
		return nil
	}
	for _, key := range []string{"info", "message"} {
		if message, ok := props[key].(map[string]any); ok {
			return message
		}
	}
	return nil
}

// sessionStatusType extracts the busy/idle discriminator of a session.status
// event. opencode 1.19+ nests it as status:{type:"busy"|"idle"}; a bare
// string status is tolerated. "" means the event says neither.
func sessionStatusType(props map[string]any) string {
	if props == nil {
		return ""
	}
	switch status := props["status"].(type) {
	case string:
		return strings.ToLower(strings.TrimSpace(status))
	case map[string]any:
		return strings.ToLower(propString(status, "type", "status"))
	}
	return ""
}

// isSessionIdleEvent reports whether ev is a terminal idle signal for a
// session. opencode 1.19.4 signals run completion with
// session.status{status:{type:"idle"}}; session.idle is the equivalent (still
// emitted by some builds and older traffic). Both count as terminal.
func isSessionIdleEvent(ev opencode.Event, props map[string]any) bool {
	switch ev.Type {
	case "session.idle":
		return true
	case "session.status":
		return sessionStatusType(props) == "idle"
	}
	return false
}

// idleReason extracts why a session went idle ("aborted" on interruption).
func idleReason(props map[string]any) string {
	if props == nil {
		return ""
	}
	if info, ok := props["info"].(map[string]any); ok {
		if reason := propString(info, "reason"); reason != "" {
			return reason
		}
	}
	return propString(props, "reason")
}

// rawMessageRole reads the role of a raw message object carried in event
// properties.
func rawMessageRole(value any) string {
	message, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	return strings.ToLower(propString(message, "role"))
}

// rawMessageText joins the text parts of a raw message object carried in
// event properties.
func rawMessageText(value any) string {
	message, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	parts, ok := message["parts"].([]any)
	if !ok {
		return ""
	}
	texts := make([]string, 0, len(parts))
	for _, item := range parts {
		part, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if propString(part, "type") != "text" {
			continue
		}
		if text := propString(part, "text"); text != "" {
			texts = append(texts, text)
		}
	}
	return strings.Join(texts, "\n")
}

// messageText joins the text parts of a typed message from ListMessages.
func messageText(message opencode.Message) string {
	texts := make([]string, 0, len(message.Parts))
	for _, part := range message.Parts {
		if part.Type != "text" {
			continue
		}
		if text := strings.TrimSpace(part.Text); text != "" {
			texts = append(texts, text)
		}
	}
	return strings.Join(texts, "\n")
}

// latestAssistantResult reads only the newest assistant. An aborted turn may
// have no text; falling through to an older answer would report stale output.
func (a *Adapter) latestAssistantResult(ctx context.Context, sessionID string, maxChars int) (string, map[string]any) {
	messages, err := a.client.ListMessages(ctx, sessionID)
	if err != nil {
		return "", nil
	}
	for index := len(messages) - 1; index >= 0; index-- {
		if strings.ToLower(strings.TrimSpace(messages[index].Role)) != "assistant" {
			continue
		}
		failure, _ := messages[index].Raw["error"].(map[string]any)
		text := strings.TrimSpace(messageText(messages[index]))
		return trimRunes(strings.Join(strings.Fields(text), " "), maxChars), failure
	}
	return "", nil
}

// msToTime converts an opencode epoch-millisecond timestamp.
func msToTime(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.Unix(ms/1000, (ms%1000)*int64(time.Millisecond)).UTC()
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}

// trimRunes truncates a string to maxChars runes, appending an ellipsis when
// truncation happened.
func trimRunes(value string, maxChars int) string {
	value = strings.TrimSpace(value)
	if maxChars <= 0 || value == "" {
		return value
	}
	runes := []rune(value)
	if len(runes) <= maxChars {
		return value
	}
	return string(runes[:maxChars]) + "..."
}

// sendPluginEvent delivers one event unless the context is done. Returning
// false tells the producing loop to unwind so its native stream closes.
func sendPluginEvent(ctx context.Context, out chan<- pluginbridge.PluginEvent, event pluginbridge.PluginEvent) bool {
	select {
	case out <- event:
		return true
	case <-ctx.Done():
		return false
	}
}

// sendHistoryEvent is the HistoryStreamEvent counterpart of sendPluginEvent.
func sendHistoryEvent(ctx context.Context, out chan<- pluginbridge.HistoryStreamEvent, event pluginbridge.HistoryStreamEvent) bool {
	select {
	case out <- event:
		return true
	case <-ctx.Done():
		return false
	}
}
