// Package opencode is the HTTP+SSE client for a locally running opencode
// server (default 127.0.0.1:4096, `opencode serve` or the one embedded in the
// TUI/desktop app).
//
// The wire format follows opencode v1 (verified against opencode-ai 1.19.4's
// OpenAPI spec served at GET /doc and live traffic): JSON keys are camelCase
// (parentID, modelID, providerID), session times are epoch milliseconds under
// time.created/time.updated, SSE events are `data: {"type": ...,
// "properties": {...}}` frames on GET /event.
package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"sort"
)

// ErrServerOffline is returned when no opencode server can be reached.
var ErrServerOffline = errors.New("opencode server offline")

// Session is an opencode session (GET /session, POST /session).
type Session struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	ParentID string `json:"parentID"`
	// Directory is the working directory the session runs in.
	Directory string      `json:"directory"`
	Time      SessionTime `json:"time"`
	// Raw keeps fields this contract does not model (e.g. share).
	Raw map[string]any `json:"-"`
}

// SessionTime holds epoch-millisecond timestamps.
type SessionTime struct {
	Created int64 `json:"created"`
	Updated int64 `json:"updated"`
}

// UnmarshalJSON decodes a wire session and keeps every unmodelled top-level
// field (slug, projectID, cost, tokens, share, model, ...) in Raw.
func (s *Session) UnmarshalJSON(b []byte) error {
	type wire struct {
		ID        string       `json:"id"`
		Title     string       `json:"title"`
		ParentID  string       `json:"parentID"`
		Directory string       `json:"directory"`
		Time      *SessionTime `json:"time"`
	}
	var w wire
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	s.ID = w.ID
	s.Title = w.Title
	s.ParentID = w.ParentID
	s.Directory = w.Directory
	if w.Time != nil {
		s.Time = *w.Time
	}
	s.Raw = extraFields(b, "id", "title", "parentID", "directory", "time")
	return nil
}

// CreateSessionRequest creates a session (POST /session).
type CreateSessionRequest struct {
	ParentID  string    `json:"parentID,omitempty"`
	Directory string    `json:"directory,omitempty"`
	Title     string    `json:"title,omitempty"`
	Model     *ModelRef `json:"model,omitempty"`
}

type ModelRef struct {
	ID         string `json:"id"`
	ProviderID string `json:"providerID"`
}

// PatchSessionRequest renames / updates a session (PATCH /session/:id).
type PatchSessionRequest struct {
	Title string `json:"title,omitempty"`
}

// Message is one user/assistant/system message (GET /session/:id/message).
//
// The v1 wire format wraps each message as {"info": {...}, "parts": [...]};
// UnmarshalJSON accepts both that envelope and a bare info object (the shape
// carried by "message.updated" SSE events). Assistant messages carry
// modelID/providerID/cost at the top level of info; user messages nest them
// under info.model — both are normalized into the exported fields.
type Message struct {
	ID         string         `json:"id"`
	Role       string         `json:"role"` // "user" | "assistant" | "system"
	Time       MessageTime    `json:"time"`
	Parts      []MessagePart  `json:"parts"`
	ModelID    string         `json:"modelID,omitempty"`
	ProviderID string         `json:"providerID,omitempty"`
	Cost       float64        `json:"cost"`
	Raw        map[string]any `json:"-"`
}

// MessageTime holds epoch-millisecond timestamps.
type MessageTime struct {
	Created int64 `json:"created"`
}

// UnmarshalJSON decodes either {"info":..., "parts":[...]} (list endpoint) or
// a bare message info object (SSE events). Unmodelled info fields (sessionID,
// agent, tokens, time.completed, error, ...) land in Raw.
func (m *Message) UnmarshalJSON(b []byte) error {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(b, &probe); err != nil {
		return err
	}
	info := b
	// "time" stays in Raw: only time.created is modelled, and assistant
	// messages also carry time.completed there.
	modeled := []string{"id", "role", "modelID", "providerID", "cost"}
	if infoRaw, ok := probe["info"]; ok {
		info = infoRaw
		if partsRaw, ok := probe["parts"]; ok {
			var parts []MessagePart
			if err := json.Unmarshal(partsRaw, &parts); err != nil {
				return err
			}
			m.Parts = parts
		}
	} else if partsRaw, ok := probe["parts"]; ok {
		// Bare info object with inline parts (tolerated shape).
		modeled = append(modeled, "parts")
		var parts []MessagePart
		if err := json.Unmarshal(partsRaw, &parts); err != nil {
			return err
		}
		m.Parts = parts
	}

	type wire struct {
		ID         string       `json:"id"`
		Role       string       `json:"role"`
		Time       *MessageTime `json:"time"`
		ModelID    string       `json:"modelID"`
		ProviderID string       `json:"providerID"`
		Cost       *float64     `json:"cost"`
	}
	var w wire
	if err := json.Unmarshal(info, &w); err != nil {
		return err
	}
	m.ID = w.ID
	m.Role = w.Role
	if w.Time != nil {
		m.Time = *w.Time
	}
	m.ModelID = w.ModelID
	m.ProviderID = w.ProviderID
	if w.Cost != nil {
		m.Cost = *w.Cost
	}

	extras := extraFields(info, modeled...)
	// User messages nest the model under info.model{modelID,providerID,...}.
	if m.ModelID == "" || m.ProviderID == "" {
		if nested, ok := extras["model"].(map[string]any); ok {
			id, _ := nested["modelID"].(string)
			prov, _ := nested["providerID"].(string)
			if m.ModelID == "" {
				m.ModelID = id
			}
			if m.ProviderID == "" {
				m.ProviderID = prov
			}
			delete(extras, "model")
		}
	} else {
		delete(extras, "model")
	}
	m.Raw = extras
	return nil
}

// MessagePart is a message part; text parts carry Text, everything else
// keeps its raw JSON in Raw for the adapter to interpret (tool calls…).
type MessagePart struct {
	Type string         `json:"type"` // "text", "tool", "step-start", ...
	Text string         `json:"text,omitempty"`
	Raw  map[string]any `json:"-"`
}

// UnmarshalJSON decodes a part and keeps unmodelled fields (id, sessionID,
// messageID, tool state, synthetic, ...) in Raw.
func (p *MessagePart) UnmarshalJSON(b []byte) error {
	type wire struct {
		Type string  `json:"type"`
		Text *string `json:"text"`
	}
	var w wire
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	p.Type = w.Type
	if w.Text != nil {
		p.Text = *w.Text
	}
	p.Raw = extraFields(b, "type", "text")
	return nil
}

// Config is the merged opencode config (GET /config).
type Config struct {
	// Model is the default model in "providerID/modelID" form, if set.
	Model string         `json:"model,omitempty"`
	Raw   map[string]any `json:"-"`
}

// UnmarshalJSON decodes the config and keeps everything besides "model" in Raw.
func (c *Config) UnmarshalJSON(b []byte) error {
	type wire struct {
		Model string `json:"model"`
	}
	var w wire
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	c.Model = w.Model
	c.Raw = extraFields(b, "model")
	return nil
}

// PatchConfigRequest patches the config (PATCH /config); Model is
// "providerID/modelID".
type PatchConfigRequest struct {
	Model string `json:"model,omitempty"`
}

// Agent is an opencode agent definition (GET /agent), e.g. "build", "plan".
type Agent struct {
	Name        string `json:"name"`
	Mode        string `json:"mode"` // "primary" | "subagent" | "all"
	Description string `json:"description"`
}

// ModelInfo describes a usable model (from /config/providers).
type ModelInfo struct {
	ProviderID string `json:"providerID"`
	ModelID    string `json:"modelID"`
	Name       string `json:"name"`
}

// Event is one server-sent bus event (GET /event SSE). Type examples from a
// live 1.19.4 server: "server.connected", "server.heartbeat" (periodic
// keep-alive event, not a comment line), "session.updated",
// "session.status" (properties.status.type = "busy"|"idle"),
// "session.idle" (legacy idle signal, still emitted alongside session.status),
// "message.updated", "message.part.updated", "permission.asked",
// "permission.responded".
type Event struct {
	Type       string          `json:"type"`
	Properties json.RawMessage `json:"properties"`
}

// ServerInfo describes a discovered running opencode server.
type ServerInfo struct {
	BaseURL  string // e.g. http://127.0.0.1:4096
	Password string // basic-auth password if OPENCODE_SERVER_PASSWORD is set
}

// Client talks to one opencode server over HTTP.
type Client struct {
	baseURL  string
	password string
	http     httpClientState
}

// New returns a Client for baseURL (e.g. http://127.0.0.1:4096); password may
// be empty for no-auth local servers.
func New(baseURL, password string) *Client {
	return &Client{baseURL: baseURL, password: password}
}

// DiscoverOptions controls server discovery: explicit URL/port, and where to
// look for instance files of servers started by the TUI/desktop (random port).
type DiscoverOptions struct {
	ExplicitURL string // from flag/env, highest priority
	DataDir     string // opencode data dir override ("" = default)
}

// Discover finds a running opencode server. It must check the explicit URL
// first, then well-known ports (4096), then instance/port files under the
// opencode data dir. Returns ErrServerOffline when nothing responds.
func Discover(ctx context.Context, opts DiscoverOptions) (*ServerInfo, error) {
	return discover(ctx, opts)
}

// ListSessions lists sessions sorted newest-updated first.
func (c *Client) ListSessions(ctx context.Context) ([]Session, error) {
	var sessions []Session
	if err := c.doJSON(ctx, "GET", "/session", nil, nil, &sessions); err != nil {
		return nil, err
	}
	sort.SliceStable(sessions, func(i, j int) bool {
		return sessions[i].Time.Updated > sessions[j].Time.Updated
	})
	return sessions, nil
}

// GetSession fetches one session.
func (c *Client) GetSession(ctx context.Context, id string) (*Session, error) {
	var session Session
	if err := c.doJSON(ctx, "GET", "/session/"+url.PathEscape(id), nil, nil, &session); err != nil {
		return nil, err
	}
	return &session, nil
}

// CreateSession creates a new session. ParentID and Title go in the JSON
// body; Directory (when set) is sent as the ?directory= query parameter, the
// way the v1 server expects it.
func (c *Client) CreateSession(ctx context.Context, req CreateSessionRequest) (*Session, error) {
	body := struct {
		ParentID string    `json:"parentID,omitempty"`
		Title    string    `json:"title,omitempty"`
		Model    *ModelRef `json:"model,omitempty"`
	}{ParentID: req.ParentID, Title: req.Title, Model: req.Model}
	var query url.Values
	if req.Directory != "" {
		query = url.Values{"directory": []string{req.Directory}}
	}
	var session Session
	if err := c.doJSON(ctx, "POST", "/session", query, body, &session); err != nil {
		return nil, err
	}
	return &session, nil
}

// PatchSession renames a session.
func (c *Client) PatchSession(ctx context.Context, id string, patch PatchSessionRequest) (*Session, error) {
	var session Session
	if err := c.doJSON(ctx, "PATCH", "/session/"+url.PathEscape(id), nil, patch, &session); err != nil {
		return nil, err
	}
	return &session, nil
}

// DeleteSession removes a session.
func (c *Client) SwitchSessionModel(ctx context.Context, id string, model ModelRef) error {
	return c.doJSON(ctx, "POST", "/api/session/"+url.PathEscape(id)+"/model", nil, map[string]any{"model": model}, nil)
}

func (c *Client) DeleteSession(ctx context.Context, id string) error {
	return c.doJSON(ctx, "DELETE", "/session/"+url.PathEscape(id), nil, nil, nil)
}

// ListMessages returns a session's messages in chronological order with
// parts in original order.
func (c *Client) ListMessages(ctx context.Context, sessionID string) ([]Message, error) {
	var messages []Message
	if err := c.doJSON(ctx, "GET", "/session/"+url.PathEscape(sessionID)+"/message", nil, nil, &messages); err != nil {
		return nil, err
	}
	// v1 already returns ascending order; tolerate a newest-first payload
	// (e.g. a v2-style server) by reversing strictly-descending lists.
	if strictlyDescending(messages) {
		for i, j := 0, len(messages)-1; i < j; i, j = i+1, j-1 {
			messages[i], messages[j] = messages[j], messages[i]
		}
	}
	return messages, nil
}

// Prompt submits user text fire-and-forget (POST /session/:id/prompt_async,
// expect 204). Streaming arrives via StreamEvents ("message.updated" …).
func (c *Client) Prompt(ctx context.Context, sessionID, text string) error {
	body := promptAsyncRequest{Parts: []promptPart{{Type: "text", Text: text}}}
	return c.doJSON(ctx, "POST", "/session/"+url.PathEscape(sessionID)+"/prompt_async", nil, body, nil)
}

// Abort interrupts the active run (POST /session/:id/abort).
func (c *Client) Abort(ctx context.Context, sessionID string) error {
	return c.doJSON(ctx, "POST", "/session/"+url.PathEscape(sessionID)+"/abort", nil, nil, nil)
}

// GetConfig reads the merged config.
func (c *Client) GetConfig(ctx context.Context) (*Config, error) {
	var cfg Config
	if err := c.doJSON(ctx, "GET", "/config", nil, nil, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// PatchConfig updates config (model switch).
func (c *Client) PatchConfig(ctx context.Context, patch PatchConfigRequest) (*Config, error) {
	var cfg Config
	if err := c.doJSON(ctx, "PATCH", "/config", nil, patch, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// ListAgents returns agent definitions (GET /agent), hiding the internal
// agents the server marks "hidden" (title/summary/compaction machinery).
func (c *Client) ListAgents(ctx context.Context) ([]Agent, error) {
	var wire []struct {
		Name        string `json:"name"`
		Mode        string `json:"mode"`
		Description string `json:"description"`
		Hidden      bool   `json:"hidden"`
	}
	if err := c.doJSON(ctx, "GET", "/agent", nil, nil, &wire); err != nil {
		return nil, err
	}
	agents := make([]Agent, 0, len(wire))
	for _, a := range wire {
		if a.Hidden {
			continue
		}
		agents = append(agents, Agent{Name: a.Name, Mode: a.Mode, Description: a.Description})
	}
	return agents, nil
}

// ListModels returns models advertised by configured providers
// (GET /config/providers).
func (c *Client) ListModels(ctx context.Context) ([]ModelInfo, error) {
	var wire struct {
		Providers []struct {
			ID     string `json:"id"`
			Models map[string]struct {
				Name string `json:"name"`
			} `json:"models"`
		} `json:"providers"`
	}
	if err := c.doJSON(ctx, "GET", "/config/providers", nil, nil, &wire); err != nil {
		return nil, err
	}
	var models []ModelInfo
	for _, p := range wire.Providers {
		for id, m := range p.Models {
			name := m.Name
			if name == "" {
				name = id
			}
			models = append(models, ModelInfo{ProviderID: p.ID, ModelID: id, Name: name})
		}
	}
	sort.Slice(models, func(i, j int) bool {
		if models[i].ProviderID != models[j].ProviderID {
			return models[i].ProviderID < models[j].ProviderID
		}
		return models[i].ModelID < models[j].ModelID
	})
	return models, nil
}

// StreamEvents subscribes to GET /event (SSE) and calls fn for every event
// until ctx is cancelled. It must survive transient errors by reconnecting
// with backoff (re-emitting nothing on its own) and must not call fn after
// ctx is done.
//
// It returns ctx.Err() when ctx is cancelled, the error fn returned if fn
// fails, and otherwise keeps reconnecting through transient errors until ctx
// is done.
func (c *Client) StreamEvents(ctx context.Context, fn func(Event) error) error {
	return c.streamEvents(ctx, fn)
}

// strictlyDescending reports whether messages are ordered by strictly
// decreasing creation time.
func strictlyDescending(messages []Message) bool {
	for i := 1; i < len(messages); i++ {
		if messages[i-1].Time.Created <= messages[i].Time.Created {
			return false
		}
	}
	return len(messages) > 1
}

// extraFields decodes b into a generic map, drops the modelled keys and
// returns the remainder (nil when nothing is left).
func extraFields(b []byte, modeled ...string) map[string]any {
	var all map[string]any
	if err := json.Unmarshal(b, &all); err != nil || all == nil {
		return nil
	}
	for _, k := range modeled {
		delete(all, k)
	}
	if len(all) == 0 {
		return nil
	}
	return all
}

// promptAsyncRequest is the body of POST /session/:id/prompt_async.
type promptAsyncRequest struct {
	Parts []promptPart `json:"parts"`
}

type promptPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}
