package opencode

// V2 wire contracts are intentionally kept at the HTTP boundary. The adapter
// consumes the same normalized Session/Message/Event types for both protocols.
import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

func (c *Client) isV2(ctx context.Context) (bool, error) {
	c.protocolMu.Lock()
	defer c.protocolMu.Unlock()
	if c.protocol != 0 {
		return c.protocol == 2, nil
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", c.endpoint("/api/info"), nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Accept", "application/json")
	if c.password != "" {
		req.SetBasicAuth(basicAuthUsername, c.password)
	}
	resp, err := c.http.get().Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return false, fmt.Errorf("%w: protocol detection: %v", ErrServerOffline, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		c.protocol = 1
		return false, nil
	}
	if resp.StatusCode != 200 {
		return false, c.responseError("GET", "/api/info", resp)
	}
	var info struct {
		Version string `json:"version"`
	}
	// Legacy servers may return their HTML UI for unknown paths.
	if strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
		c.protocol = 1
		return false, nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&info); err != nil {
		return false, fmt.Errorf("opencode: invalid server info: %w", err)
	}
	if strings.HasPrefix(strings.TrimPrefix(info.Version, "v"), "2.") {
		c.protocol = 2
		return true, nil
	}
	if strings.HasPrefix(strings.TrimPrefix(info.Version, "v"), "1.") {
		c.protocol = 1
		return false, nil
	}
	return false, fmt.Errorf("opencode: unsupported server version %q", info.Version)
}

func v2SessionPath(id string) string { return "/api/session/" + url.PathEscape(id) }

type v2Page struct {
	Data   json.RawMessage `json:"data"`
	Cursor struct {
		Next string `json:"next"`
	} `json:"cursor"`
}

// Fetch all pages in their requested order. Reject repeated cursors instead of
// silently looping or truncating an account's history.
func (c *Client) v2Pages(ctx context.Context, path string, query url.Values) ([]json.RawMessage, error) {
	if query == nil {
		query = url.Values{}
	}
	query.Set("order", "asc")
	query.Set("limit", "100")
	var all []json.RawMessage
	seen := map[string]bool{}
	for {
		var page v2Page
		if err := c.doJSON(ctx, "GET", path, query, nil, &page); err != nil {
			return nil, err
		}
		var items []json.RawMessage
		if err := json.Unmarshal(page.Data, &items); err != nil {
			return nil, fmt.Errorf("opencode: invalid V2 page: %w", err)
		}
		all = append(all, items...)
		next := page.Cursor.Next
		if next == "" {
			return all, nil
		}
		if seen[next] {
			return nil, fmt.Errorf("opencode: repeated V2 cursor")
		}
		seen[next] = true
		query.Del("order")
		query.Set("cursor", next)
	}
}

func decodeV2Session(raw json.RawMessage) (*Session, error) {
	var s Session
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	var wire struct {
		Location struct {
			Directory string `json:"directory"`
		} `json:"location"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, err
	}
	s.Directory = wire.Location.Directory
	if s.ID == "" {
		return nil, fmt.Errorf("opencode: V2 session missing id")
	}
	return &s, nil
}
func (c *Client) v2Sessions(ctx context.Context) ([]Session, error) {
	query := url.Values{}
	if d := directoryFromContext(ctx); d != "" {
		query.Set("directory", d)
	}
	raw, err := c.v2Pages(ctx, "/api/session", query)
	if err != nil {
		return nil, err
	}
	out := make([]Session, 0, len(raw))
	for _, r := range raw {
		s, err := decodeV2Session(r)
		if err != nil {
			return nil, err
		}
		out = append(out, *s)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.Updated > out[j].Time.Updated })
	return out, nil
}
func (c *Client) v2Session(ctx context.Context, id string) (*Session, error) {
	var r struct {
		Data json.RawMessage `json:"data"`
	}
	if err := c.doJSON(ctx, "GET", v2SessionPath(id), nil, nil, &r); err != nil {
		return nil, err
	}
	return decodeV2Session(r.Data)
}
func (c *Client) v2Create(ctx context.Context, req CreateSessionRequest) (*Session, error) {
	body := map[string]any{}
	d := req.Directory
	if d == "" {
		d = directoryFromContext(ctx)
	}
	if d != "" {
		body["location"] = map[string]string{"directory": d}
	}
	if req.Title != "" {
		body["title"] = req.Title
	}
	if req.ParentID != "" {
		body["parentID"] = req.ParentID
	}
	if req.Model != nil {
		body["model"] = req.Model
	}
	var r struct {
		Data json.RawMessage `json:"data"`
	}
	if err := c.doJSON(ctx, "POST", "/api/session", nil, body, &r); err != nil {
		return nil, err
	}
	return decodeV2Session(r.Data)
}

func (c *Client) v2Messages(ctx context.Context, id string) ([]Message, error) {
	raw, err := c.v2Pages(ctx, v2SessionPath(id)+"/message", nil)
	if err != nil {
		return nil, err
	}
	out := make([]Message, 0, len(raw))
	for _, r := range raw {
		m, err := decodeV2Message(r)
		if err != nil {
			return nil, err
		}
		if m.Role != "" {
			out = append(out, m)
		}
	}
	return out, nil
}
func decodeV2Message(raw json.RawMessage) (Message, error) {
	var w struct {
		ID      string        `json:"id"`
		Type    string        `json:"type"`
		Text    string        `json:"text"`
		Time    MessageTime   `json:"time"`
		Model   ModelRef      `json:"model"`
		Content []MessagePart `json:"content"`
		Files   []struct {
			URI  string `json:"uri"`
			Name string `json:"name"`
			Mime string `json:"mime"`
		} `json:"files"`
		Cost float64 `json:"cost"`
	}
	if err := json.Unmarshal(raw, &w); err != nil {
		return Message{}, err
	}
	m := Message{ID: w.ID, Time: w.Time, ModelID: w.Model.ID, ProviderID: w.Model.ProviderID, Cost: w.Cost, Raw: extraFields(raw)}
	switch w.Type {
	case "user", "assistant", "system":
		m.Role = w.Type
	default:
		return m, nil
	}
	if w.Type == "assistant" {
		m.Parts = w.Content
	} else {
		m.Parts = []MessagePart{{Type: "text", Text: w.Text}}
	}
	for _, f := range w.Files {
		m.Parts = append(m.Parts, MessagePart{Type: "file", Raw: map[string]any{"url": f.URI, "filename": f.Name, "mime": f.Mime}})
	}
	return m, nil
}
func (c *Client) v2Prompt(ctx context.Context, id string, parts []InputPart) error {
	var text []string
	files := []map[string]string{}
	for _, p := range parts {
		switch p.Type {
		case "text":
			text = append(text, p.Text)
		case "file":
			files = append(files, map[string]string{"uri": p.URL, "name": p.Filename})
		default:
			return fmt.Errorf("opencode: unsupported prompt part %q", p.Type)
		}
	}
	var receipt struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := c.doJSON(ctx, "POST", v2SessionPath(id)+"/prompt", nil, map[string]any{"text": strings.Join(text, "\n"), "files": files}, &receipt); err != nil {
		return err
	}
	c.protocolMu.Lock()
	if c.lastSubmitted == nil {
		c.lastSubmitted = map[string]string{}
	}
	c.lastSubmitted[id] = receipt.Data.ID
	c.protocolMu.Unlock()
	return nil
}

// RunOutcome recovers a terminal V2 execution missed by a late or reconnected
// subscriber. Only an idle marker AFTER the exact admitted user ID qualifies.
func (c *Client) RunOutcome(ctx context.Context, id string) (string, bool, error) {
	v2, err := c.isV2(ctx)
	if err != nil || !v2 {
		return "", false, err
	}
	c.protocolMu.Lock()
	userID := c.lastSubmitted[id]
	c.protocolMu.Unlock()
	if userID == "" {
		return "", false, nil
	}
	raw, err := c.v2Pages(ctx, v2SessionPath(id)+"/message", nil)
	if err != nil {
		return "", false, err
	}
	seen := false
	for _, r := range raw {
		var m struct {
			ID      string `json:"id"`
			Type    string `json:"type"`
			Outcome string `json:"outcome"`
		}
		if json.Unmarshal(r, &m) != nil {
			continue
		}
		if m.ID == userID {
			seen = true
		}
		if seen && m.Type == "idle" {
			switch m.Outcome {
			case "succeeded":
				return "", true, nil
			case "failed", "interrupted":
				return m.Outcome, true, nil
			}
		}
	}
	return "", false, nil
}
func (c *Client) v2Config(ctx context.Context) (*Config, error) {
	var r struct {
		Data *ModelRef `json:"data"`
	}
	if err := c.doJSON(ctx, "GET", "/api/model/default", nil, nil, &r); err != nil {
		return nil, err
	}
	cfg := &Config{}
	if r.Data != nil {
		cfg.Model = r.Data.ProviderID + "/" + r.Data.ID
	}
	return cfg, nil
}
func (c *Client) v2Agents(ctx context.Context) ([]Agent, error) {
	var r struct {
		Data []struct {
			Agent
			Hidden bool `json:"hidden"`
		} `json:"data"`
	}
	if err := c.doJSON(ctx, "GET", "/api/agent", nil, nil, &r); err != nil {
		return nil, err
	}
	out := []Agent{}
	for _, a := range r.Data {
		if !a.Hidden {
			out = append(out, a.Agent)
		}
	}
	return out, nil
}
func (c *Client) v2Models(ctx context.Context) ([]ModelInfo, error) {
	var r struct {
		Data []struct {
			ID         string `json:"id"`
			ProviderID string `json:"providerID"`
			Name       string `json:"name"`
			Enabled    bool   `json:"enabled"`
		} `json:"data"`
	}
	if err := c.doJSON(ctx, "GET", "/api/model", nil, nil, &r); err != nil {
		return nil, err
	}
	out := []ModelInfo{}
	for _, m := range r.Data {
		if m.Enabled {
			out = append(out, ModelInfo{ProviderID: m.ProviderID, ModelID: m.ID, Name: m.Name})
		}
	}
	return out, nil
}
