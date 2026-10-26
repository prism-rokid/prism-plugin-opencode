package adapter

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Rokid-Prism/prism-plugin-opencode/internal/opencode"
)

// mockClient implements adapter.Client with per-method stubs. Methods without
// a stub fail loudly ("mock: X not stubbed") so a test never silently passes
// through an unintended code path.
type mockClient struct {
	listSessions    func(ctx context.Context) ([]opencode.Session, error)
	getSession      func(ctx context.Context, id string) (*opencode.Session, error)
	createSession   func(ctx context.Context, req opencode.CreateSessionRequest) (*opencode.Session, error)
	patchSession    func(ctx context.Context, id string, patch opencode.PatchSessionRequest) (*opencode.Session, error)
	deleteSession   func(ctx context.Context, id string) error
	switchModel     func(ctx context.Context, id string, model opencode.ModelRef) error
	listMessages    func(ctx context.Context, sessionID string) ([]opencode.Message, error)
	prompt          func(ctx context.Context, sessionID, text string) error
	promptWithParts func(ctx context.Context, sessionID string, parts []opencode.InputPart) error
	abort           func(ctx context.Context, sessionID string) error
	getConfig       func(ctx context.Context) (*opencode.Config, error)
	patchConfig     func(ctx context.Context, patch opencode.PatchConfigRequest) (*opencode.Config, error)
	listAgents      func(ctx context.Context) ([]opencode.Agent, error)
	listModels      func(ctx context.Context) ([]opencode.ModelInfo, error)
	streamEvents    func(ctx context.Context, fn func(opencode.Event) error) error
}

var _ Client = (*mockClient)(nil)

func (m *mockClient) SwitchSessionModel(ctx context.Context, id string, model opencode.ModelRef) error {
	if m.switchModel == nil {
		return errNotStubbed("SwitchSessionModel")
	}
	return m.switchModel(ctx, id, model)
}

func errNotStubbed(name string) error {
	return &notStubbedError{method: name}
}

type notStubbedError struct{ method string }

func (e *notStubbedError) Error() string { return "mock: " + e.method + " not stubbed" }

func isNotStubbed(err error) bool {
	_, ok := err.(*notStubbedError)
	return ok
}

func (m *mockClient) ListSessions(ctx context.Context) ([]opencode.Session, error) {
	if m.listSessions == nil {
		return nil, errNotStubbed("ListSessions")
	}
	return m.listSessions(ctx)
}

func (m *mockClient) GetSession(ctx context.Context, id string) (*opencode.Session, error) {
	if m.getSession == nil {
		return nil, errNotStubbed("GetSession")
	}
	return m.getSession(ctx, id)
}

func (m *mockClient) CreateSession(ctx context.Context, req opencode.CreateSessionRequest) (*opencode.Session, error) {
	if m.createSession == nil {
		return nil, errNotStubbed("CreateSession")
	}
	return m.createSession(ctx, req)
}

func (m *mockClient) PatchSession(ctx context.Context, id string, patch opencode.PatchSessionRequest) (*opencode.Session, error) {
	if m.patchSession == nil {
		return nil, errNotStubbed("PatchSession")
	}
	return m.patchSession(ctx, id, patch)
}

func (m *mockClient) DeleteSession(ctx context.Context, id string) error {
	if m.deleteSession == nil {
		return errNotStubbed("DeleteSession")
	}
	return m.deleteSession(ctx, id)
}

func (m *mockClient) ListMessages(ctx context.Context, sessionID string) ([]opencode.Message, error) {
	if m.listMessages == nil {
		return nil, errNotStubbed("ListMessages")
	}
	return m.listMessages(ctx, sessionID)
}

func (m *mockClient) Prompt(ctx context.Context, sessionID, text string) error {
	if m.prompt == nil {
		return errNotStubbed("Prompt")
	}
	return m.prompt(ctx, sessionID, text)
}

func (m *mockClient) PromptWithParts(ctx context.Context, sessionID string, parts []opencode.InputPart) error {
	if m.promptWithParts == nil {
		return errNotStubbed("PromptWithParts")
	}
	return m.promptWithParts(ctx, sessionID, parts)
}

func (m *mockClient) Abort(ctx context.Context, sessionID string) error {
	if m.abort == nil {
		return errNotStubbed("Abort")
	}
	return m.abort(ctx, sessionID)
}

func (m *mockClient) GetConfig(ctx context.Context) (*opencode.Config, error) {
	if m.getConfig == nil {
		return nil, errNotStubbed("GetConfig")
	}
	return m.getConfig(ctx)
}

func (m *mockClient) PatchConfig(ctx context.Context, patch opencode.PatchConfigRequest) (*opencode.Config, error) {
	if m.patchConfig == nil {
		return nil, errNotStubbed("PatchConfig")
	}
	return m.patchConfig(ctx, patch)
}

func (m *mockClient) ListAgents(ctx context.Context) ([]opencode.Agent, error) {
	if m.listAgents == nil {
		return nil, errNotStubbed("ListAgents")
	}
	return m.listAgents(ctx)
}

func (m *mockClient) ListModels(ctx context.Context) ([]opencode.ModelInfo, error) {
	if m.listModels == nil {
		return nil, errNotStubbed("ListModels")
	}
	return m.listModels(ctx)
}

func (m *mockClient) StreamEvents(ctx context.Context, fn func(opencode.Event) error) error {
	if m.streamEvents == nil {
		return errNotStubbed("StreamEvents")
	}
	return m.streamEvents(ctx, fn)
}

// --- test helpers ---

func testAdapter(client Client) *Adapter {
	return New(Config{Client: client, Endpoint: "http://127.0.0.1:4096"})
}

// eventWithProps builds a native event whose properties object is encoded
// from a Go map (camelCase keys preserved, as the real client forwards them).
func eventWithProps(t *testing.T, eventType string, props map[string]any) opencode.Event {
	t.Helper()
	raw, err := json.Marshal(props)
	if err != nil {
		t.Fatalf("marshal props: %v", err)
	}
	return opencode.Event{Type: eventType, Properties: raw}
}

// blockingStreamEvents replays scripted events then holds the stream open
// until ctx is done, mirroring a healthy SSE subscription.
func blockingStreamEvents(scripted ...opencode.Event) func(context.Context, func(opencode.Event) error) error {
	return func(ctx context.Context, fn func(opencode.Event) error) error {
		for _, ev := range scripted {
			if err := fn(ev); err != nil {
				return err
			}
		}
		<-ctx.Done()
		return ctx.Err()
	}
}

func textPart(text string) opencode.MessagePart {
	return opencode.MessagePart{Type: "text", Text: text}
}

func userMessage(id, text string, created int64) opencode.Message {
	return opencode.Message{
		ID: id, Role: "user", Time: opencode.MessageTime{Created: created},
		Parts: []opencode.MessagePart{textPart(text)},
	}
}

func assistantMessage(id, text string, created int64) opencode.Message {
	return opencode.Message{
		ID: id, Role: "assistant", Time: opencode.MessageTime{Created: created},
		Parts: []opencode.MessagePart{textPart(text)},
	}
}
