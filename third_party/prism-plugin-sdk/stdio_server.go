package pluginbridge

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// StdioServer reads JSON-over-newline-stdio requests from In, dispatches them
// to the wrapped PluginAdapter, and writes responses/events to Out.
type StdioServer struct {
	Adapter PluginAdapter
	In      io.Reader
	Out     io.Writer

	cancel  context.CancelFunc
	subs    map[string]context.CancelFunc
	history map[string]context.CancelFunc
}

type lockedEncoder struct {
	mu  sync.Mutex
	enc *json.Encoder
}

type rpcRequest struct {
	ID     string `json:"id"`
	Method string `json:"method"`
	Params any    `json:"params,omitempty"`
}

type rpcResponse struct {
	ID      string          `json:"id"`
	OK      bool            `json:"ok"`
	Payload json.RawMessage `json:"payload,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

type rpcEvent struct {
	Event   string          `json:"event"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

func (e *lockedEncoder) Encode(value any) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.enc.Encode(value)
}

// Serve is the convenience entry point. It wraps the adapter in a StdioServer
// reading from os.Stdin / writing to os.Stdout and blocks until stdin closes
// or the context is cancelled. This is what most plugins call from main().
func Serve(ctx context.Context, adapter PluginAdapter) error {
	server := &StdioServer{Adapter: adapter, In: os.Stdin, Out: os.Stdout}
	return server.Run(ctx)
}

// CheckProtocolVersion reads PRISM_PLUGIN_PROTOCOL_VERSION from the
// environment and returns an error if it is set and does not match the
// ProtocolVersion this SDK implements. The Hub passes this env var when
// spawning a plugin; a mismatch means the Hub and plugin speak different
// on-wire schemas, and the plugin should fail fast rather than silently
// decode fields under the wrong names. When the env var is absent (e.g. a
// plugin run standalone for debugging) the check passes.
func CheckProtocolVersion() error {
	got := strings.TrimSpace(os.Getenv("PRISM_PLUGIN_PROTOCOL_VERSION"))
	if got == "" {
		return nil
	}
	if got != ProtocolVersion {
		return fmt.Errorf("pluginbridge plugin protocol version mismatch: hub sent %q, SDK implements %q", got, ProtocolVersion)
	}
	return nil
}

func (s *StdioServer) Run(ctx context.Context) error {
	if err := CheckProtocolVersion(); err != nil {
		return err
	}
	if s == nil || s.Adapter == nil {
		return fmt.Errorf("pluginbridge plugin stdio server adapter required")
	}
	in := s.In
	if in == nil {
		return fmt.Errorf("pluginbridge plugin stdio server input required")
	}
	out := s.Out
	if out == nil {
		return fmt.Errorf("pluginbridge plugin stdio server output required")
	}
	ctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.subs = map[string]context.CancelFunc{}
	s.history = map[string]context.CancelFunc{}
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)
	enc := &lockedEncoder{enc: json.NewEncoder(out)}
	for scanner.Scan() {
		var req rpcRequest
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			continue
		}
		payload, err := s.handle(ctx, req.Method, req.Params, enc)
		if err != nil {
			_ = enc.Encode(rpcResponse{
				ID: req.ID,
				OK: false,
				Error: &rpcError{
					Code:    "adapter_error",
					Message: err.Error(),
				},
			})
			continue
		}
		raw, err := marshalWire(payload)
		if err != nil {
			_ = enc.Encode(rpcResponse{
				ID: req.ID,
				OK: false,
				Error: &rpcError{
					Code:    "encode_error",
					Message: err.Error(),
				},
			})
			continue
		}
		_ = enc.Encode(rpcResponse{ID: req.ID, OK: true, Payload: raw})
	}
	cancel()
	for _, cancel := range s.subs {
		cancel()
	}
	for _, cancel := range s.history {
		cancel()
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return nil
}

func (s *StdioServer) handle(ctx context.Context, method string, params any, enc *lockedEncoder) (any, error) {
	switch method {
	case "adapter.probe":
		return s.Adapter.Probe(ctx)
	case "adapter.discover":
		return s.Adapter.Discover(ctx)
	case "adapter.startSessionWithMessage":
		starter, ok := s.Adapter.(SessionStarter)
		if !ok {
			return nil, fmt.Errorf("adapter does not implement startSessionWithMessage")
		}
		var req StartSessionWithMessageRequest
		if err := decodeParams(params, &req); err != nil {
			return nil, err
		}
		return starter.StartSessionWithMessage(ctx, req)
	case "adapter.openDraft":
		opener, ok := s.Adapter.(DraftOpener)
		if !ok {
			return nil, fmt.Errorf("adapter does not implement openDraft")
		}
		var req DraftOpenRequest
		if err := decodeParams(params, &req); err != nil {
			return nil, err
		}
		return opener.OpenDraft(ctx, req)
	case "adapter.controlDraft":
		controller, ok := s.Adapter.(DraftController)
		if !ok {
			return nil, fmt.Errorf("adapter does not implement controlDraft")
		}
		var req DraftControlRequest
		if err := decodeParams(params, &req); err != nil {
			return nil, err
		}
		if raw, err := extensibleParams(params); err != nil {
			return nil, err
		} else {
			req.Target = raw["target"]
			req.Metadata = objectMap(raw["metadata"])
		}
		return controller.ControlDraft(ctx, req)
	case "adapter.startDraftWithMessage":
		starter, ok := s.Adapter.(DraftStarter)
		if !ok {
			return nil, fmt.Errorf("adapter does not implement startDraftWithMessage")
		}
		var req StartDraftWithMessageRequest
		if err := decodeParams(params, &req); err != nil {
			return nil, err
		}
		return starter.StartDraftWithMessage(ctx, req)
	case "adapter.listSessions":
		lister, ok := s.Adapter.(SessionLister)
		if !ok {
			return nil, fmt.Errorf("adapter does not implement listSessions")
		}
		return lister.ListSessions(ctx)
	case "adapter.attachSession":
		attacher, ok := s.Adapter.(SessionAttacher)
		if !ok {
			return nil, fmt.Errorf("adapter does not implement attachSession")
		}
		var req AttachSessionRequest
		if err := decodeParams(params, &req); err != nil {
			return nil, err
		}
		return attacher.AttachSession(ctx, req)
	case "adapter.readHistory":
		reader, ok := s.Adapter.(HistoryReader)
		if !ok {
			return nil, fmt.Errorf("adapter does not implement readHistory")
		}
		var req struct {
			Session NativeSession `json:"Session"`
			Limit   int           `json:"Limit"`
		}
		if err := decodeParams(params, &req); err != nil {
			return nil, err
		}
		return reader.ReadHistory(ctx, req.Session, req.Limit)
	case "adapter.readAgentUsage":
		reader, ok := s.Adapter.(AgentUsageReader)
		if !ok {
			return nil, fmt.Errorf("adapter does not implement readAgentUsage")
		}
		return reader.ReadAgentUsage(ctx)
	case "adapter.readDetail":
		reader, ok := s.Adapter.(DetailReader)
		if !ok {
			return nil, fmt.Errorf("adapter does not implement readDetail")
		}
		var req struct {
			Session NativeSession `json:"Session"`
		}
		if err := decodeParams(params, &req); err != nil {
			return nil, err
		}
		return reader.ReadDetail(ctx, req.Session)
	case "adapter.readHistoryStream":
		streamer, ok := s.Adapter.(HistoryStreamer)
		if !ok {
			return nil, fmt.Errorf("adapter does not implement readHistoryStream")
		}
		var req struct {
			Session NativeSession        `json:"Session"`
			Request HistoryStreamRequest `json:"Request"`
		}
		if err := decodeParams(params, &req); err != nil {
			return nil, err
		}
		if req.Request.StreamID == "" {
			req.Request.StreamID = uuid.NewString()
		}
		streamCtx, cancel := context.WithCancel(ctx)
		events, err := streamer.ReadHistoryStream(streamCtx, req.Session, req.Request)
		if err != nil {
			cancel()
			return nil, err
		}
		s.history[req.Request.StreamID] = cancel
		go forwardHistoryEvents(streamCtx, cancel, enc, req.Request.StreamID, events)
		return map[string]any{"stream_id": req.Request.StreamID}, nil
	case "adapter.cancelHistoryStream":
		var req struct {
			StreamID string `json:"StreamID"`
		}
		if err := decodeParams(params, &req); err != nil {
			return nil, err
		}
		if cancel := s.history[req.StreamID]; cancel != nil {
			cancel()
			delete(s.history, req.StreamID)
		}
		return map[string]any{"ok": true}, nil
	case "adapter.resolveApproval":
		resolver, ok := s.Adapter.(ApprovalResolver)
		if !ok {
			return nil, fmt.Errorf("adapter does not implement resolveApproval")
		}
		var req ApprovalResolutionRequest
		if err := decodeParams(params, &req); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, resolver.ResolveApproval(ctx, req)
	case "adapter.readStatus":
		reader, ok := s.Adapter.(StatusReader)
		if !ok {
			return nil, fmt.Errorf("adapter does not implement readStatus")
		}
		var req struct {
			Session NativeSession `json:"Session"`
			RunID   string        `json:"RunID"`
		}
		if err := decodeParams(params, &req); err != nil {
			return nil, err
		}
		return reader.ReadStatus(ctx, req.Session, req.RunID)
	case "adapter.controlSession":
		controller, ok := s.Adapter.(SessionController)
		if !ok {
			return nil, fmt.Errorf("adapter does not implement controlSession")
		}
		var req ControlSessionRequest
		if err := decodeParams(params, &req); err != nil {
			return nil, err
		}
		if raw, err := extensibleParams(params); err != nil {
			return nil, err
		} else {
			req.Target = raw["target"]
			req.Metadata = objectMap(raw["metadata"])
		}
		return controller.ControlSession(ctx, req)
	case "adapter.openManagedTerminal":
		launcher, ok := s.Adapter.(ManagedTerminalLauncher)
		if !ok {
			return nil, fmt.Errorf("adapter does not implement openManagedTerminal")
		}
		var req ManagedTerminalRequest
		if err := decodeParams(params, &req); err != nil {
			return nil, err
		}
		return launcher.OpenManagedTerminal(ctx, req)
	case "adapter.send":
		var req struct {
			Session NativeSession  `json:"Session"`
			Message InboundMessage `json:"Message"`
		}
		if err := decodeParams(params, &req); err != nil {
			return nil, err
		}
		return s.Adapter.Send(ctx, req.Session, req.Message)
	case "adapter.subscribe":
		var req struct {
			SubscriptionID string        `json:"SubscriptionID"`
			Session        NativeSession `json:"Session"`
		}
		if err := decodeParams(params, &req); err != nil {
			return nil, err
		}
		if req.SubscriptionID == "" {
			req.SubscriptionID = uuid.NewString()
		}
		subCtx, cancel := context.WithCancel(ctx)
		events, err := s.Adapter.Subscribe(subCtx, req.Session)
		if err != nil {
			cancel()
			return nil, err
		}
		s.subs[req.SubscriptionID] = cancel
		go forwardPluginEvents(subCtx, cancel, enc, req.SubscriptionID, events)
		return map[string]any{"subscribed": true, "subscription_id": req.SubscriptionID}, nil
	case "adapter.subscribePlugin":
		var req struct {
			SubscriptionID string `json:"SubscriptionID"`
		}
		if err := decodeParams(params, &req); err != nil {
			return nil, err
		}
		watcher, ok := s.Adapter.(PluginWideSubscriber)
		if !ok {
			return nil, fmt.Errorf("adapter does not implement subscribePlugin")
		}
		if req.SubscriptionID == "" {
			req.SubscriptionID = uuid.NewString()
		}
		subCtx, cancel := context.WithCancel(ctx)
		events, err := watcher.SubscribePlugin(subCtx)
		if err != nil {
			cancel()
			return nil, err
		}
		s.subs[req.SubscriptionID] = cancel
		go forwardPluginEvents(subCtx, cancel, enc, req.SubscriptionID, events)
		return map[string]any{"subscribed": true, "subscription_id": req.SubscriptionID}, nil
	case "adapter.unsubscribe":
		var req struct {
			SubscriptionID string `json:"SubscriptionID"`
		}
		if err := decodeParams(params, &req); err != nil {
			return nil, err
		}
		if cancel := s.subs[req.SubscriptionID]; cancel != nil {
			cancel()
			delete(s.subs, req.SubscriptionID)
		}
		return map[string]any{"ok": true}, nil
	case "adapter.interrupt":
		var req struct {
			Session NativeSession `json:"Session"`
			TaskID  string        `json:"TaskID"`
		}
		if err := decodeParams(params, &req); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, s.Adapter.Interrupt(ctx, req.Session, req.TaskID)
	case "adapter.verifyVisibility":
		var req struct {
			Session NativeSession `json:"Session"`
			Marker  string        `json:"Marker"`
		}
		if err := decodeParams(params, &req); err != nil {
			return nil, err
		}
		return s.Adapter.VerifyVisibility(ctx, req.Session, req.Marker)
	case "adapter.waitForRun":
		waiter, ok := s.Adapter.(RunWaiter)
		if !ok {
			return nil, fmt.Errorf("adapter does not implement waitForRun")
		}
		var req struct {
			Session NativeSession `json:"Session"`
			RunID   string        `json:"RunID"`
		}
		if err := decodeParams(params, &req); err != nil {
			return nil, err
		}
		return waiter.WaitForRun(ctx, req.Session, req.RunID)
	case "adapter.close":
		if s.cancel != nil {
			s.cancel()
		}
		return map[string]any{"ok": true}, s.Adapter.Close(ctx)
	default:
		return nil, fmt.Errorf("unknown adapter method %s", method)
	}
}

func decodeParams(params any, out any) error {
	b, err := json.Marshal(params)
	if err != nil {
		return err
	}
	if len(b) == 0 || string(b) == "null" {
		b = []byte(`{}`)
	}
	return unmarshalWire(b, out)
}

// target and metadata are opaque extensions owned by the Plugin. Decode the
// typed envelope normally, but preserve these nested values in their original
// validated snake_case form for the adapter.
func extensibleParams(params any) (map[string]any, error) {
	b, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, err
	}
	if err := validateSnakeWire(raw); err != nil {
		return nil, err
	}
	return raw, nil
}

func objectMap(value any) map[string]any {
	raw, ok := value.(map[string]any)
	if !ok || len(raw) == 0 {
		return map[string]any{}
	}
	return raw
}

func forwardPluginEvents(ctx context.Context, cancel context.CancelFunc, enc *lockedEncoder, subID string, events <-chan PluginEvent) {
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-events:
			if !ok {
				return
			}
			if event.CreatedAt.IsZero() {
				event.CreatedAt = time.Now().UTC()
			}
			if err := enc.Encode(rpcEvent{
				Event: "plugin.event",
				Payload: mustJSONRaw(map[string]any{
					"subscription_id": subID,
					"event":           event,
				}),
			}); err != nil {
				// The parent runtime no longer accepts stdout. End this pump so a
				// live native watcher cannot accumulate undeliverable events.
				cancel()
				return
			}
		}
	}
}

func forwardHistoryEvents(ctx context.Context, cancel context.CancelFunc, enc *lockedEncoder, streamID string, events <-chan HistoryStreamEvent) {
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-events:
			if !ok {
				return
			}
			event.StreamID = streamID
			if err := enc.Encode(rpcEvent{Event: "history.stream", Payload: mustJSONRaw(event)}); err != nil {
				// Same rule as plugin events: a broken stdout ends this relay.
				cancel()
				return
			}
			if event.Type == "end" || event.Type == "error" {
				return
			}
		}
	}
}

func mustJSONRaw(value any) json.RawMessage {
	b, _ := marshalWire(value)
	return b
}
