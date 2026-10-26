// Command fakeserver is a self-contained fake opencode server for the
// prism-plugin-opencode conformance suite. It implements every endpoint listed
// in docs/CONTRACT.md with the exact wire shapes captured from a live opencode
// 1.19.4 server (camelCase JSON fields, {info, parts} message envelopes,
// providers-as-object, epoch-millisecond time objects, query-parameter session
// directory, SSE event properties carrying a top-level sessionID) so the
// client and adapter under test are exercised end to end exactly as they would
// be against the real binary.
//
// Scriptable behavior:
//
//	flag -port              listen port (0 = pick a free port; the chosen URL is
//	                        printed to stdout as "LISTEN http://127.0.0.1:PORT")
//	flag -prompt-delay-ms   artificial pacing of the prompt_async event sequence
//	flag -password          when non-empty, every request must carry basic auth
//	                        user "opencode" with this password (401 otherwise)
//
// Behavior contract:
//
//   - Two preset sessions exist at startup: ses_conformance_001 (with one
//     seeded user+assistant message pair) and ses_conformance_002 (empty).
//   - POST /session/:id/prompt_async takes the real body shape
//     {"parts":[{"type":"text","text":"..."}]} and returns 204 immediately,
//     then after the configured delay pushes, on GET /event, in order:
//     session.status(busy), message.updated (user message), message.updated
//     (assistant partial), message.part.updated (assistant text part),
//     message.updated (assistant final), session.status(idle). The user and
//     assistant messages are also persisted into GET /session/:id/message as
//     {info, parts} envelopes.
//   - GET /config reflects merges done by PATCH /config.
//   - GET /config/providers returns {providers: [...], default: {...}} with
//     2 providers and 3 models in total (models is a map, like the real API).
//   - GET /agent returns the build/plan agents plus internal agents the real
//     server marks hidden:true (title/summary), which the client filters.
//   - GET /global/health answers {"healthy":true,...} like the real v1.x
//     server (used by the client's discovery probe; /config also works).
//   - Every SSE connection first receives a server.connected event, then a
//     server.heartbeat event every ~15s, mirroring the real event bus.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Wire types (opencode shapes; see internal/opencode/client.go, the contract
// authority for field names).

type timeObj struct {
	Created int64 `json:"created"`
	Updated int64 `json:"updated,omitempty"`
}

type session struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	ParentID  string   `json:"parentID"`
	Directory string   `json:"directory"`
	Time      timeObj  `json:"time"`
	Version   string   `json:"version,omitempty"`
	Revisions []string `json:"revisions,omitempty"`
}

type part struct {
	ID        string `json:"id,omitempty"`
	SessionID string `json:"sessionID,omitempty"`
	MessageID string `json:"messageID,omitempty"`
	Type      string `json:"type"`
	Text      string `json:"text,omitempty"`
}

// messageInfo is the bare message object the real API carries inside SSE
// message.updated events and inside the {info, parts} list envelope. Parts are
// deliberately NOT part of info: the real server streams them separately via
// message.part.updated.
type messageInfo struct {
	ID         string  `json:"id"`
	SessionID  string  `json:"sessionID"`
	Role       string  `json:"role"`
	Time       timeObj `json:"time"`
	ProviderID string  `json:"providerID,omitempty"`
	ModelID    string  `json:"modelID,omitempty"`
	Cost       float64 `json:"cost"`
	// Model nests under info.model for user messages, exactly like the real
	// wire format (the client lifts providerID/modelID out of it).
	Model *modelRef `json:"model,omitempty"`
}

type modelRef struct {
	ProviderID string `json:"providerID"`
	ModelID    string `json:"modelID"`
}

// message is the {info, parts} envelope GET /session/:id/message returns.
type message struct {
	Info  messageInfo `json:"info"`
	Parts []part      `json:"parts"`
}

// promptRequest is the body of POST /session/:id/prompt_async. The real v1
// shape is {"parts":[{"type":"text","text":"..."}]}; a bare "text" field is
// tolerated for manual curling.
type promptRequest struct {
	Parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"parts"`
	Text string `json:"text"`
}

// text returns the prompt text from whichever shape the client sent.
func (r promptRequest) text() string {
	if trimmed := strings.TrimSpace(r.Text); trimmed != "" {
		return trimmed
	}
	for _, p := range r.Parts {
		if p.Type == "text" && strings.TrimSpace(p.Text) != "" {
			return strings.TrimSpace(p.Text)
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Server state.

type fakeServer struct {
	mu       sync.Mutex
	sessions map[string]*session
	messages map[string][]message // sessionID -> chronological messages
	config   map[string]any
	agents   []map[string]any
	runs     map[string]*activeRun // sessionID -> active prompt run, if any

	promptDelay time.Duration

	subMu  sync.Mutex
	subs   map[chan sseEvent]struct{}
	nextID int
}

type activeRun struct {
	cancel chan struct{}
	done   bool
}

type sseEvent struct {
	name string
	data []byte // full event object: {"type":...,"properties":{...}}
}

func now() int64 { return time.Now().UnixMilli() }

func newFakeServer(promptDelay time.Duration) *fakeServer {
	base := now()
	return &fakeServer{
		sessions: map[string]*session{
			"ses_conformance_001": {
				ID:        "ses_conformance_001",
				Title:     "Conformance session one",
				Directory: mustWd(),
				Time:      timeObj{Created: base - 3600_000, Updated: base - 60_000},
				Version:   "fake-v1",
			},
			"ses_conformance_002": {
				ID:        "ses_conformance_002",
				Title:     "Conformance session two",
				Directory: mustWd(),
				Time:      timeObj{Created: base - 1800_000, Updated: base - 120_000},
				Version:   "fake-v1",
			},
		},
		messages: map[string][]message{
			"ses_conformance_001": {
				{
					Info: messageInfo{
						ID: "msg_seed_user", SessionID: "ses_conformance_001", Role: "user",
						Time:  timeObj{Created: base - 3500_000, Updated: base - 3500_000},
						Model: &modelRef{ProviderID: "anthropic", ModelID: "claude-sonnet-4-5"},
					},
					Parts: []part{{ID: "prt_seed_1", SessionID: "ses_conformance_001", MessageID: "msg_seed_user", Type: "text", Text: "seed question"}},
				},
				{
					Info: messageInfo{
						ID: "msg_seed_assistant", SessionID: "ses_conformance_001", Role: "assistant",
						Time:       timeObj{Created: base - 3400_000, Updated: base - 3350_000},
						ProviderID: "anthropic",
						ModelID:    "claude-sonnet-4-5",
					},
					Parts: []part{{ID: "prt_seed_2", SessionID: "ses_conformance_001", MessageID: "msg_seed_assistant", Type: "text", Text: "seed answer"}},
				},
			},
		},
		config: map[string]any{
			"model":      "anthropic/claude-sonnet-4-5",
			"autoupdate": true,
			"share":      "private",
		},
		agents: []map[string]any{
			{"name": "build", "mode": "primary", "description": "Write, edit, and run code", "model": map[string]any{"providerID": "anthropic", "modelID": "claude-sonnet-4-5"}, "native": true},
			{"name": "plan", "mode": "primary", "description": "Plan before touching files", "model": map[string]any{"providerID": "anthropic", "modelID": "claude-haiku-4-5"}, "native": true},
			// Internal agents the real server marks hidden; the client must
			// filter them out of ListAgents.
			{"name": "title", "mode": "primary", "native": true, "hidden": true},
			{"name": "summary", "mode": "primary", "native": true, "hidden": true},
		},
		runs:        map[string]*activeRun{},
		subs:        map[chan sseEvent]struct{}{},
		promptDelay: promptDelay,
		nextID:      1,
	}
}

func mustWd() string {
	wd, err := os.Getwd()
	if err != nil {
		return "/tmp"
	}
	return wd
}

// uid mints a unique id. Callers must already hold s.mu (Go mutexes are not
// reentrant, so this deliberately does not lock).
func (s *fakeServer) uid(prefix string) string {
	s.nextID++
	return fmt.Sprintf("%s_%04d", prefix, s.nextID)
}

// broadcast formats and queues one bus event for every SSE subscriber.
func (s *fakeServer) broadcast(eventType string, properties any) {
	payload, err := json.Marshal(map[string]any{"type": eventType, "properties": properties})
	if err != nil {
		log.Printf("broadcast %s: %v", eventType, err)
		return
	}
	s.subMu.Lock()
	defer s.subMu.Unlock()
	for ch := range s.subs {
		select {
		case ch <- sseEvent{name: eventType, data: payload}:
		default: // slow consumer; drop rather than block the whole server
		}
	}
}

func (s *fakeServer) subscribe() chan sseEvent {
	ch := make(chan sseEvent, 64)
	s.subMu.Lock()
	defer s.subMu.Unlock()
	s.subs[ch] = struct{}{}
	return ch
}

func (s *fakeServer) unsubscribe(ch chan sseEvent) {
	s.subMu.Lock()
	delete(s.subs, ch)
	s.subMu.Unlock()
}

// ---------------------------------------------------------------------------
// Handlers.

func (s *fakeServer) handleRoot(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "server": "opencode-fake"})
}

func (s *fakeServer) handleSessionList(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]session, 0, len(s.sessions))
	for _, ses := range s.sessions {
		out = append(out, *ses)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.Updated > out[j].Time.Updated })
	writeJSON(w, http.StatusOK, out)
}

func (s *fakeServer) handleSessionCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ParentID  string `json:"parentID"`
		Directory string `json:"directory"`
		Title     string `json:"title"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	s.mu.Lock()
	id := s.uid("ses")
	s.mu.Unlock()
	// The real v1 API takes the directory as a ?directory= query parameter,
	// never as a body field; the body spelling is tolerated for convenience.
	directory := r.URL.Query().Get("directory")
	if directory == "" {
		directory = req.Directory
	}
	if directory == "" {
		directory = mustWd()
	}
	ses := &session{ID: id, Title: req.Title, ParentID: req.ParentID, Directory: directory, Time: timeObj{Created: now(), Updated: now()}, Version: "fake-v1"}
	s.mu.Lock()
	s.sessions[id] = ses
	s.messages[id] = nil
	s.mu.Unlock()
	s.broadcast("session.created", map[string]any{"sessionID": id, "info": ses})
	s.broadcast("session.updated", map[string]any{"sessionID": id, "info": ses})
	writeJSON(w, http.StatusOK, ses)
}

func (s *fakeServer) handleSessionGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	ses, ok := s.sessions[id]
	s.mu.Unlock()
	if !ok {
		writeJSON(w, http.StatusNotFound, notFound("Session", id))
		return
	}
	writeJSON(w, http.StatusOK, ses)
}

func (s *fakeServer) handleSessionPatch(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Title string `json:"title"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	s.mu.Lock()
	ses, ok := s.sessions[id]
	if ok && req.Title != "" {
		ses.Title = req.Title
		ses.Time.Updated = now()
	}
	s.mu.Unlock()
	if !ok {
		writeJSON(w, http.StatusNotFound, notFound("Session", id))
		return
	}
	s.broadcast("session.updated", map[string]any{"sessionID": id, "info": ses})
	writeJSON(w, http.StatusOK, ses)
}

func (s *fakeServer) handleSessionDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	_, ok := s.sessions[id]
	delete(s.sessions, id)
	delete(s.messages, id)
	s.mu.Unlock()
	if !ok {
		writeJSON(w, http.StatusNotFound, notFound("Session", id))
		return
	}
	writeJSON(w, http.StatusOK, true) // real API returns a boolean body
}

func (s *fakeServer) handleMessages(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[id]; !ok {
		writeJSON(w, http.StatusNotFound, notFound("Session", id))
		return
	}
	msgs := s.messages[id]
	if msgs == nil {
		msgs = []message{}
	}
	writeJSON(w, http.StatusOK, msgs)
}

func (s *fakeServer) handlePromptAsync(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req promptRequest
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	text := req.text()
	if text == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "text required"})
		return
	}
	s.mu.Lock()
	ses, ok := s.sessions[id]
	if !ok {
		s.mu.Unlock()
		writeJSON(w, http.StatusNotFound, notFound("Session", id))
		return
	}
	// The run's model: the merged config default, split from its
	// "providerID/modelID" form (the real client sends no per-prompt model).
	providerID, modelID := "", ""
	if merged := fmt.Sprint(s.config["model"]); strings.Contains(merged, "/") {
		halves := strings.SplitN(merged, "/", 2)
		providerID, modelID = halves[0], halves[1]
	}
	ts := now()
	userID := s.uid("msg")
	userMsg := message{
		Info: messageInfo{
			ID: userID, SessionID: id, Role: "user",
			Time:  timeObj{Created: ts, Updated: ts},
			Model: &modelRef{ProviderID: providerID, ModelID: modelID},
		},
		Parts: []part{
			{ID: s.uid("prt"), SessionID: id, MessageID: userID, Type: "text", Text: text},
		},
	}
	assistantID := s.uid("msg")
	s.messages[id] = append(s.messages[id], userMsg)
	ses.Time.Updated = ts
	run := &activeRun{cancel: make(chan struct{})}
	s.runs[id] = run
	s.mu.Unlock()

	// The real server flips the session to busy the moment a run starts and
	// back to idle when it finishes.
	s.broadcast("session.status", map[string]any{"sessionID": id, "status": map[string]any{"type": "busy"}})
	go s.simulateRun(id, run, userMsg, assistantID, providerID, modelID)
	w.WriteHeader(http.StatusNoContent) // per CONTRACT: prompt_async returns 204
}

// simulateRun drives the scripted event sequence for one prompt turn, all in
// the real wire shapes: message.updated carries {sessionID, info} with NO
// parts, streamed text arrives as message.part.updated {sessionID, part}, and
// the run terminates with session.status{status:{type:"idle"}}. Aborting the
// run fast-forwards to the final assistant state and emits the idle transition
// early.
func (s *fakeServer) simulateRun(sessionID string, run *activeRun, userMsg message, assistantID, providerID, modelID string) {
	delay := s.promptDelay
	if delay <= 0 {
		delay = 400 * time.Millisecond
	}
	reply := "fake reply to: " + userMsg.Parts[0].Text
	partial := reply
	if len(partial) > 8 {
		partial = partial[:len(partial)/2]
	}

	s.broadcast("message.updated", map[string]any{"sessionID": sessionID, "info": userMsg.Info})

	assistantBase := messageInfo{
		ID: assistantID, SessionID: sessionID, Role: "assistant",
		ProviderID: providerID, ModelID: modelID,
	}
	// emit persists one assistant state and returns its envelope.
	emit := func(text string, ts int64) message {
		partID := s.uid("prt")
		m := message{
			Info:  assistantBase,
			Parts: []part{{ID: partID, SessionID: sessionID, MessageID: assistantID, Type: "text", Text: text}},
		}
		m.Info.Time = timeObj{Created: ts, Updated: ts}
		s.mu.Lock()
		if msgs, ok := s.messages[sessionID]; ok {
			replaced := false
			for i := range msgs {
				if msgs[i].Info.ID == assistantID {
					msgs[i] = m
					replaced = true
				}
			}
			if !replaced {
				msgs = append(msgs, m)
				s.messages[sessionID] = msgs
			}
		}
		s.mu.Unlock()
		return m
	}

	ts := now()
	partialMsg := emit(partial, ts)
	select {
	case <-time.After(delay / 4):
	case <-run.cancel:
		final := emit(reply, now())
		s.broadcast("message.updated", map[string]any{"sessionID": sessionID, "info": final.Info})
		s.finishRun(sessionID)
		return
	}
	s.broadcast("message.updated", map[string]any{"sessionID": sessionID, "info": partialMsg.Info})

	select {
	case <-time.After(delay / 4):
	case <-run.cancel:
		final := emit(reply, now())
		s.broadcast("message.updated", map[string]any{"sessionID": sessionID, "info": final.Info})
		s.finishRun(sessionID)
		return
	}
	// Streaming output: the real bus pushes the part itself, not the message.
	s.broadcast("message.part.updated", map[string]any{
		"sessionID": sessionID,
		"part":      part{ID: s.uid("prt"), SessionID: sessionID, MessageID: assistantID, Type: "text", Text: reply},
	})

	select {
	case <-time.After(delay / 4):
	case <-run.cancel:
	}
	final := emit(reply, now())
	s.broadcast("message.updated", map[string]any{"sessionID": sessionID, "info": final.Info})
	s.finishRun(sessionID)
}

// finishRun flips the session back to idle, the terminal signal opencode
// 1.19.4 really emits (session.status with a nested status object).
func (s *fakeServer) finishRun(sessionID string) {
	s.mu.Lock()
	run := s.runs[sessionID]
	if run != nil {
		run.done = true
	}
	delete(s.runs, sessionID)
	ses := s.sessions[sessionID]
	if ses != nil {
		ses.Time.Updated = now()
	}
	s.mu.Unlock()
	if ses != nil {
		s.broadcast("session.status", map[string]any{
			"sessionID": sessionID,
			"status":    map[string]any{"type": "idle"},
		})
	}
}

func (s *fakeServer) handleAbort(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	run := s.runs[id]
	_, sessionOK := s.sessions[id]
	s.mu.Unlock()
	if !sessionOK {
		writeJSON(w, http.StatusNotFound, notFound("Session", id))
		return
	}
	if run != nil {
		select {
		case <-run.cancel: // already cancelled
		default:
			close(run.cancel)
		}
	}
	writeJSON(w, http.StatusOK, true) // real API returns a boolean body
}

func (s *fakeServer) handleConfigGet(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	writeJSON(w, http.StatusOK, s.config)
}

func (s *fakeServer) handleConfigPatch(w http.ResponseWriter, r *http.Request) {
	var patch map[string]any
	if err := decodeBody(r, &patch); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	s.mu.Lock()
	for k, v := range patch {
		s.config[k] = v
	}
	merged := s.config
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, merged)
}

func (s *fakeServer) handleProviders(w http.ResponseWriter, r *http.Request) {
	// Real wire shape: {"providers": [...], "default": {...}} where each
	// provider's models is a map keyed by model id (see docs/CONTRACT.md).
	writeJSON(w, http.StatusOK, map[string]any{
		"providers": []map[string]any{
			{
				"id":   "anthropic",
				"name": "Anthropic",
				"env":  "",
				"models": map[string]any{
					"claude-sonnet-4-5": map[string]any{"name": "Claude Sonnet 4.5", "reasoning": true, "tool_call": true, "attachment": true, "temperature": true, "cost": map[string]int{}},
					"claude-haiku-4-5":  map[string]any{"name": "Claude Haiku 4.5", "reasoning": true, "tool_call": true, "attachment": true, "temperature": true, "cost": map[string]int{}},
				},
			},
			{
				"id":   "openai",
				"name": "OpenAI",
				"env":  "",
				"models": map[string]any{
					"gpt-5.2": map[string]any{"name": "GPT-5.2", "reasoning": true, "tool_call": true, "attachment": true, "temperature": false, "cost": map[string]int{}},
				},
			},
		},
		"default": map[string]any{"anthropic": "claude-sonnet-4-5"},
	})
}

func (s *fakeServer) handleAgents(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.agents)
}

func (s *fakeServer) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	send := func(ev sseEvent) bool {
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.name, ev.data); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	connected, _ := json.Marshal(map[string]any{"id": "evt_connected", "type": "server.connected", "properties": map[string]any{"version": "fake-v1"}})
	if !send(sseEvent{name: "server.connected", data: connected}) {
		return
	}
	// The real server keeps the bus warm with a server.heartbeat event every
	// ~15s (an event frame, not an SSE comment line).
	heartbeat, _ := json.Marshal(map[string]any{"type": "server.heartbeat", "properties": map[string]any{}})
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	ch := s.subscribe()
	defer s.unsubscribe(ch)
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if !send(sseEvent{name: "server.heartbeat", data: heartbeat}) {
				return
			}
		case ev := <-ch:
			if !send(ev) {
				return
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Plumbing.

// notFound builds the real opencode error shape
// {"name":"NotFoundError","data":{"message":...}}.
func notFound(kind, id string) map[string]any {
	return map[string]any{
		"name": "NotFoundError",
		"data": map[string]any{"message": kind + " not found: " + id},
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func decodeBody(r *http.Request, out any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	return dec.Decode(out)
}

func basicAuth(password string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if password != "" {
			user, pass, ok := r.BasicAuth()
			if !ok || user != "opencode" || pass != password {
				w.Header().Set("WWW-Authenticate", `Basic realm="opencode"`)
				writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func main() {
	port := flag.Int("port", 0, "listen port (0 = pick a free port)")
	delayMs := flag.Int("prompt-delay-ms", 400, "artificial prompt_async event pacing in milliseconds")
	password := flag.String("password", "", "when set, require basic auth user opencode with this password")
	flag.Parse()

	s := newFakeServer(time.Duration(*delayMs) * time.Millisecond)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleRoot)
	// The v1.x health endpoint the client's discovery probe hits first.
	mux.HandleFunc("GET /global/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"healthy": true, "version": "fake-v1"})
	})
	mux.HandleFunc("GET /session", s.handleSessionList)
	mux.HandleFunc("POST /session", s.handleSessionCreate)
	mux.HandleFunc("GET /session/{id}", s.handleSessionGet)
	mux.HandleFunc("PATCH /session/{id}", s.handleSessionPatch)
	mux.HandleFunc("DELETE /session/{id}", s.handleSessionDelete)
	mux.HandleFunc("GET /session/{id}/message", s.handleMessages)
	mux.HandleFunc("POST /session/{id}/prompt_async", s.handlePromptAsync)
	mux.HandleFunc("POST /session/{id}/abort", s.handleAbort)
	mux.HandleFunc("GET /config", s.handleConfigGet)
	mux.HandleFunc("PATCH /config", s.handleConfigPatch)
	mux.HandleFunc("GET /config/providers", s.handleProviders)
	mux.HandleFunc("GET /agent", s.handleAgents)
	mux.HandleFunc("GET /event", s.handleEvents)

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		log.Fatalf("fakeserver: listen: %v", err)
	}
	// The single machine-readable stdout line; everything else goes to stderr.
	fmt.Printf("LISTEN http://%s\n", ln.Addr().String())
	fmt.Fprintf(os.Stderr, "fakeserver: serving opencode API on http://%s (prompt delay %dms, auth %s)\n", ln.Addr().String(), *delayMs, authMode(*password))
	log.SetPrefix("fakeserver: ")
	if err := http.Serve(ln, basicAuth(*password, mux)); err != nil {
		log.Fatalf("fakeserver: serve: %v", err)
	}
}

func authMode(password string) string {
	if password == "" {
		return "off"
	}
	return "basic (user opencode)"
}
