package opencode

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// SSE reconnect backoff. Package-level (unexported) so tests can shorten it.
var (
	sseBackoffStart = 250 * time.Millisecond
	sseBackoffMax   = 10 * time.Second
)

// sseMaxLineSize bounds one SSE line (JSON events with big tool states can
// exceed bufio.Scanner's 64 KB default).
const sseMaxLineSize = 1 << 20

// streamEvents implements Client.StreamEvents.
//
// Loop: connect to GET /event, parse `data:` frames, dispatch each complete
// event to fn. On stream termination (server close, network error, non-200)
// sleep with exponential backoff and reconnect; nothing is replayed on our
// own initiative — after a reconnect the server itself sends a fresh
// "server.connected" event. ctx cancellation unwinds everything.
func (c *Client) streamEvents(ctx context.Context, fn func(Event) error) error {
	backoff := sseBackoffStart
	textParts := map[string]string{}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		resp, err := c.connectSSE(ctx)
		if err != nil {
			if isContextError(ctx, err) {
				return ctx.Err()
			}
			if wakeErr := sleepCtx(ctx, backoff); wakeErr != nil {
				return wakeErr
			}
			backoff = nextBackoff(backoff)
			continue
		}

		backoff = sseBackoffStart // connection established: reset the ladder
		stopped, err := scanEvents(ctx, resp.Body, func(ev Event) error {
			if d := directoryFromContext(ctx); d != "" && ev.Location.Directory != "" && ev.Location.Directory != d {
				return nil
			}
			if ev.Type == "server.connected" {
				clear(textParts)
			}
			if ev.Type == "session.text.delta" || ev.Type == "session.text.ended" {
				var p struct {
					SessionID string `json:"sessionID"`
					MessageID string `json:"assistantMessageID"`
					Ordinal   int    `json:"ordinal"`
					Delta     string `json:"delta"`
					Text      string `json:"text"`
				}
				if err := json.Unmarshal(ev.Properties, &p); err != nil {
					return nil
				}
				key := p.SessionID + ":" + p.MessageID + ":" + fmt.Sprint(p.Ordinal)
				text := p.Text
				if ev.Type == "session.text.delta" {
					text = textParts[key] + p.Delta
					textParts[key] = text
				} else {
					delete(textParts, key)
				}
				props, _ := json.Marshal(map[string]any{"sessionID": p.SessionID, "messageID": p.MessageID, "part": map[string]any{"id": key, "messageID": p.MessageID, "type": "text", "text": text}})
				ev.Type = "message.part.updated"
				ev.Properties = props
			}
			return fn(ev)
		})
		if stopped {
			return err
		}
		if isContextError(ctx, err) {
			return ctx.Err()
		}
		// Stream broke mid-read (EOF / network error): reconnect.
		if wakeErr := sleepCtx(ctx, backoff); wakeErr != nil {
			return wakeErr
		}
		backoff = nextBackoff(backoff)
	}
}

// scanEvents reads SSE frames from body until the stream ends, ctx is done,
// or fn fails. It returns stopped=true when the caller must return (ctx done
// or fn error) instead of reconnecting.
func scanEvents(ctx context.Context, body io.ReadCloser, fn func(Event) error) (stopped bool, err error) {
	defer body.Close()
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64<<10), sseMaxLineSize)

	var dataLines []string
	// dispatch delivers one accumulated event; fnErr carries the handler
	// error back so the caller can return it verbatim.
	dispatch := func() (ok bool, fnErr error) {
		if len(dataLines) == 0 {
			return true, nil
		}
		payload := strings.Join(dataLines, "\n")
		dataLines = dataLines[:0]
		var event Event
		// Effect's V2 SSE encoder transports V2EventEncoded as a JSON string.
		raw := []byte(payload)
		var encoded string
		if json.Unmarshal(raw, &encoded) == nil {
			raw = []byte(encoded)
		}
		if jsonErr := json.Unmarshal(raw, &event); jsonErr != nil {
			return true, nil // ignore malformed frames, keep streaming
		}
		if event.Type == "" {
			return true, nil
		}
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		if err := fn(event); err != nil {
			return false, err
		}
		return true, nil
	}

	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		switch {
		case line == "": // blank line terminates an event
			if ok, fnErr := dispatch(); !ok {
				if ctx.Err() != nil {
					return true, ctx.Err()
				}
				return true, fnErr
			}
		case strings.HasPrefix(line, ":"): // comment / heartbeat line
			// ignored
		case strings.HasPrefix(line, "data:"):
			dataLines = append(dataLines, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		default:
			// `event:`, `id:`, `retry:` and any other SSE field are
			// tolerated and ignored; opencode v1 only sends `data:`.
		}
		if ctx.Err() != nil {
			return true, ctx.Err()
		}
	}
	// Flush a trailing event left pending at EOF (no final blank line).
	if ok, fnErr := dispatch(); !ok {
		if ctx.Err() != nil {
			return true, ctx.Err()
		}
		return true, fnErr
	}
	return false, scanner.Err()
}

func nextBackoff(d time.Duration) time.Duration {
	d *= 2
	if d > sseBackoffMax {
		d = sseBackoffMax
	}
	return d
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
