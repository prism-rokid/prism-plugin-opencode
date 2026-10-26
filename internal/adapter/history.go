package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	pluginbridge "github.com/Rokid-Prism/prism-plugin-sdk"

	"github.com/Rokid-Prism/prism-plugin-opencode/internal/opencode"
)

// ReadHistoryStream projects a session body (GET /session/:id/message) into
// the SDK history.stream protocol: "turn" events (source initial|live,
// operation append|replace, monotonically increasing per-turn Revision),
// then "end" for snapshot reads or "page_end" for live reads. Live reads keep
// listening to the native event stream and re-emit turns whose content
// signature changed; cancelling the stream context (adapter.cancelHistoryStream
// on the Hub side) ends the goroutine and closes the channel.

var errHistoryConsumerClosed = fmt.Errorf("opencode: history stream consumer closed")

// ReadHistoryStream implements pluginbridge.HistoryStreamer.
func (a *Adapter) ReadHistoryStream(ctx context.Context, session pluginbridge.NativeSession, req pluginbridge.HistoryStreamRequest) (<-chan pluginbridge.HistoryStreamEvent, error) {
	ctx = opencode.WithDirectory(ctx, session.Cwd)
	sessionID := strings.TrimSpace(session.NativeSessionID)
	if sessionID == "" {
		return nil, fmt.Errorf("opencode: native session id required")
	}
	streamID := strings.TrimSpace(req.StreamID)
	if streamID == "" {
		streamID = fmt.Sprintf("opencode-history-%d", time.Now().UnixNano())
	}
	limit := boundedHistoryTurnLimit(req.Limit)
	out := make(chan pluginbridge.HistoryStreamEvent, 32)
	go a.runHistoryStream(ctx, historyRun{
		sessionID: sessionID,
		streamID:  streamID,
		limit:     limit,
		live:      req.Live,
		out:       out,
	})
	return out, nil
}

type historyRun struct {
	sessionID string
	streamID  string
	limit     int
	live      bool
	out       chan<- pluginbridge.HistoryStreamEvent
}

func (a *Adapter) runHistoryStream(ctx context.Context, run historyRun) {
	defer close(run.out)

	revisions := map[string]int64{}
	signatures := map[string]string{}

	turns, err := a.loadHistoryTurns(ctx, run.sessionID, run.limit)
	if err != nil {
		sendHistoryEvent(ctx, run.out, pluginbridge.HistoryStreamEvent{
			StreamID: run.streamID, Type: "error", Source: "initial", Operation: "append",
			Error: err.Error(), Retryable: true,
		})
		return
	}
	for _, turn := range turns {
		revisions[turn.TurnID] = 1
		signatures[turn.TurnID] = historyTurnSignature(turn)
		turn.Revision = 1
		if !sendHistoryEvent(ctx, run.out, pluginbridge.HistoryStreamEvent{
			StreamID: run.streamID, Type: "turn", Source: "initial", Operation: "append", Turn: &turn,
		}) {
			return
		}
	}
	terminalType := "end"
	if run.live {
		terminalType = "page_end"
	}
	if !sendHistoryEvent(ctx, run.out, pluginbridge.HistoryStreamEvent{
		StreamID: run.streamID, Type: terminalType, Source: "initial", Operation: "append",
	}) || !run.live {
		return
	}

	// Live tail: any body-affecting native event for this session triggers a
	// reload and a diff against the emitted turn signatures. session.status is
	// included so the idle transition (final assistant body) converges on
	// opencode 1.19+; busy transitions reload to no change and are diffed away.
	err = a.client.StreamEvents(ctx, func(ev opencode.Event) error {
		switch ev.Type {
		case "message.updated", "message.part.updated", "message.removed", "session.idle", "session.status":
		default:
			return nil
		}
		props := eventProperties(ev)
		if !eventMatchesSession(props, run.sessionID) {
			return nil
		}
		latest, err := a.loadHistoryTurns(ctx, run.sessionID, run.limit)
		if err != nil {
			// One failed reload does not end the stream; the next event retries.
			return nil
		}
		liveIDs := make(map[string]struct{}, len(latest))
		for _, turn := range latest {
			liveIDs[turn.TurnID] = struct{}{}
			signature := historyTurnSignature(turn)
			previous, exists := signatures[turn.TurnID]
			if exists && previous == signature {
				continue
			}
			operation := "append"
			if exists {
				operation = "replace"
			}
			revisions[turn.TurnID]++
			if revisions[turn.TurnID] == 0 {
				revisions[turn.TurnID] = 1
			}
			signatures[turn.TurnID] = signature
			turn.Revision = revisions[turn.TurnID]
			if !sendHistoryEvent(ctx, run.out, pluginbridge.HistoryStreamEvent{
				StreamID: run.streamID, Type: "turn", Source: "live", Operation: operation, Turn: &turn,
			}) {
				return errHistoryConsumerClosed
			}
		}
		for turnID := range signatures {
			if _, exists := liveIDs[turnID]; !exists {
				delete(signatures, turnID)
				delete(revisions, turnID)
			}
		}
		return nil
	})
	if err != nil && ctx.Err() == nil && err != errHistoryConsumerClosed {
		sendHistoryEvent(ctx, run.out, pluginbridge.HistoryStreamEvent{
			StreamID: run.streamID, Type: "error", Source: "live", Operation: "replace",
			Error: err.Error(), Retryable: true,
		})
	}
}

// loadHistoryTurns fetches the message page and groups it into turns.
func (a *Adapter) loadHistoryTurns(ctx context.Context, sessionID string, limit int) ([]pluginbridge.HistoryTurn, error) {
	messages, err := a.client.ListMessages(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("opencode: list messages failed: %w", err)
	}
	return historyTurns(messages, limit), nil
}

// historyTurns groups chronological messages into user-anchored turns: a user
// message opens a turn, following assistant messages join it, and an orphan
// assistant message (session trimmed or resumed mid-conversation) becomes its
// own turn. System messages are internal bookkeeping and are not part of the
// remote body. The newest `limit` turns are returned newest-first.
func historyTurns(messages []opencode.Message, limit int) []pluginbridge.HistoryTurn {
	turns := make([]pluginbridge.HistoryTurn, 0)
	var current *pluginbridge.HistoryTurn
	for _, message := range messages {
		role := strings.ToLower(strings.TrimSpace(message.Role))
		switch role {
		case "user":
			if current != nil {
				turns = append(turns, *current)
			}
			current = &pluginbridge.HistoryTurn{
				TurnID:   message.ID,
				OrderKey: historyOrderKey(message),
				Messages: []pluginbridge.HistoryMessage{historyMessage(message)},
			}
		case "assistant":
			if current == nil {
				current = &pluginbridge.HistoryTurn{
					TurnID:   message.ID,
					OrderKey: historyOrderKey(message),
					Messages: []pluginbridge.HistoryMessage{historyMessage(message)},
				}
			} else {
				current.Messages = append(current.Messages, historyMessage(message))
			}
		default:
			continue
		}
	}
	if current != nil {
		turns = append(turns, *current)
	}
	limit = boundedHistoryTurnLimit(limit)
	if len(turns) > limit {
		turns = turns[len(turns)-limit:]
	}
	for left, right := 0, len(turns)-1; left < right; left, right = left+1, right-1 {
		turns[left], turns[right] = turns[right], turns[left]
	}
	return turns
}

func historyOrderKey(message opencode.Message) string {
	created := msToTime(message.Time.Created)
	if created.IsZero() {
		return message.ID
	}
	return created.Format(time.RFC3339Nano)
}

// historyMessage maps one native message. Text parts become the body; tool
// parts become structured Progress steps so remote clients render execution,
// not just prose. opencode messages have no per-message status; persisted
// messages are complete.
func historyMessage(message opencode.Message) pluginbridge.HistoryMessage {
	role := strings.ToLower(strings.TrimSpace(message.Role))
	createdAt := msToTime(message.Time.Created)
	if createdAt.IsZero() {
		createdAt = time.Unix(0, 0).UTC()
	}
	historyMessage := pluginbridge.HistoryMessage{
		ID:        message.ID,
		Role:      role,
		Type:      "text",
		Content:   messageText(message),
		Status:    "completed",
		CreatedAt: createdAt,
		UpdatedAt: createdAt,
	}
	if failure, ok := message.Raw["error"].(map[string]any); ok && len(failure) > 0 {
		data, _ := failure["data"].(map[string]any)
		historyMessage.Status = "failed"
		if historyMessage.Content == "" {
			historyMessage.Content = "OpenCode 运行失败：" + firstNonEmpty(propString(data, "message"), propString(failure, "name"))
		}
	}
	if steps := historyProgressSteps(message, createdAt); len(steps) > 0 {
		historyMessage.Progress = &pluginbridge.HistoryProgress{
			Status:      "completed",
			StartedAt:   createdAt,
			CompletedAt: createdAt,
			Steps:       steps,
		}
	}
	metadata := map[string]any{}
	if message.ProviderID != "" {
		metadata["provider_id"] = message.ProviderID
	}
	if message.ModelID != "" {
		metadata["model_id"] = message.ModelID
	}
	if message.Cost > 0 {
		metadata["cost"] = message.Cost
	}
	if len(metadata) > 0 {
		historyMessage.Metadata = metadata
	}
	if attachments := historyAttachments(message); len(attachments) > 0 {
		if historyMessage.Metadata == nil {
			historyMessage.Metadata = map[string]any{}
		}
		historyMessage.Metadata["attachments"] = attachments
	}
	return historyMessage
}

// historyAttachments lists a message's file parts as lightweight metadata:
// name, mime type and kind only — remote clients must never receive the
// data URL or the binary.
func historyAttachments(message opencode.Message) []map[string]any {
	attachments := make([]map[string]any, 0)
	for _, part := range message.Parts {
		if part.Type != "file" {
			continue
		}
		name := firstNonEmpty(rawString(part.Raw, "filename"), "file")
		attachments = append(attachments, map[string]any{
			"name":      name,
			"mime_type": rawString(part.Raw, "mime"),
			"kind":      "file",
		})
	}
	return attachments
}

// historyProgressSteps maps tool parts to structured progress steps. Part
// state arrives as a raw map (only Type/Text are typed on MessagePart).
func historyProgressSteps(message opencode.Message, createdAt time.Time) []pluginbridge.HistoryProgressStep {
	steps := make([]pluginbridge.HistoryProgressStep, 0)
	for index, part := range message.Parts {
		if part.Type != "tool" {
			continue
		}
		title := firstNonEmpty(rawString(part.Raw, "tool"), rawString(part.Raw, "name"), "tool")
		step := pluginbridge.HistoryProgressStep{
			ID:        fmt.Sprintf("%s-%d", message.ID, index),
			Kind:      "tool",
			CallID:    rawString(part.Raw, "callID"),
			Title:     title,
			Status:    historyStepStatus(part.Raw),
			CreatedAt: createdAt,
		}
		steps = append(steps, step)
	}
	return steps
}

func historyStepStatus(raw map[string]any) string {
	if state, ok := raw["state"].(map[string]any); ok {
		if status := rawString(state, "status"); status != "" {
			return normalizeStepStatus(status)
		}
	}
	if status := rawString(raw, "status"); status != "" {
		return normalizeStepStatus(status)
	}
	return "completed"
}

func normalizeStepStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "running", "pending":
		return "running"
	case "error":
		return "failed"
	default:
		return "completed"
	}
}

func rawString(raw map[string]any, keys ...string) string {
	if raw == nil {
		return ""
	}
	for _, key := range keys {
		if value, ok := raw[key].(string); ok {
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

// historyTurnSignature fingerprints a turn's user-visible content so the live
// loop only emits real changes.
func historyTurnSignature(turn pluginbridge.HistoryTurn) string {
	b, err := json.Marshal(turn.Messages)
	if err != nil {
		return fmt.Sprintf("%s:%d", turn.TurnID, len(turn.Messages))
	}
	return string(b)
}

func boundedHistoryTurnLimit(value int) int {
	if value < 1 {
		return maxHistoryTurnLimit
	}
	if value > maxHistoryTurnLimit {
		return maxHistoryTurnLimit
	}
	return value
}
