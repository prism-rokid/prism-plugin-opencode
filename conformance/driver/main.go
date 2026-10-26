// Command driver is the end-to-end conformance driver for the prism-plugin-
// opencode PluginBridge adapter. It spawns the adapter binary, speaks the
// PluginBridge v4 stdio line protocol at it (newline-delimited JSON, snake_case
// payload fields — see ../prism-plugin-sdk/stdio_server.go and wire_case.go),
// and asserts the full remote-conversation flow against a fake opencode server:
//
//	adapter.probe        -> available
//	adapter.discover     -> verified, >= 2 preset session hints
//	adapter.listSessions -> >= 2 sessions, discover hints present
//	adapter.subscribe    -> subscribed
//	adapter.send         -> accepted receipt
//	adapter.waitForRun   -> returns run.completed via the live idle event
//	(plugin.event frames -> at least one event observed on the subscription)
//	adapter.close        -> ok
//
// waitForRun runs before the event assertion on purpose: the fake run finishes
// in ~prompt-delay-ms, so waiting first guarantees the call is answered by the
// real terminal event (session.status type=idle on the wire) instead of
// racing the run to completion and timing out; the subscription keeps
// collecting events meanwhile and is asserted right after.
//
// The v4 "handshake" is environmental, not a request: the Hub passes
// PRISM_PLUGIN_PROTOCOL_VERSION=4 (checked by pluginbridge.CheckProtocolVersion
// on adapter startup), so the driver sets it before spawning. The opencode
// server URL is handed to the adapter both as the --server-url flag (per
// docs/CONTRACT.md) and as OPENCODE_SERVER_URL in the environment; disable the
// flag with -adapter-flag=false if the adapter's flag parser is strict.
//
// RunID note: the SDK leaves run IDs to the adapter's own event vocabulary.
// The driver derives the run id for waitForRun from the first plugin event
// payload carrying a "run_id" key, falling back to the send receipt's
// native_message_id, then to "" (current run). Which candidate worked is
// reported in the output.
//
// Exit code 0 = PASS, non-zero = FAIL.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Wire types (PluginBridge v4 stdio protocol).

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

func (e *rpcError) String() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("code=%s message=%s", e.Code, e.Message)
}

type pluginEventFrame struct {
	Event   string          `json:"event"`
	Payload json.RawMessage `json:"payload"`
}

type eventEnvelope struct {
	SubscriptionID string          `json:"subscription_id"`
	Event          eventPayload    `json:"event"`
	Raw            json.RawMessage `json:"-"`
}

type eventPayload struct {
	ID      string         `json:"id"`
	Type    string         `json:"type"`
	Status  string         `json:"status"`
	Summary string         `json:"summary"`
	Payload map[string]any `json:"payload"`
}

// ---------------------------------------------------------------------------

type driver struct {
	adapterBin string
	serverURL  string
	password   string
	useFlag    bool
	timeout    time.Duration

	cmd        *exec.Cmd
	stdin      io.WriteCloser
	mu         sync.Mutex
	pending    map[string]chan *rpcResponse
	events     []eventEnvelope
	eventReady chan struct{}
	nextID     int
	stderrTail bytes.Buffer
}

func main() {
	adapterBin := flag.String("adapter", envOr("ADAPTER_BIN", ""), "path to the adapter binary (env ADAPTER_BIN)")
	serverURL := flag.String("server-url", envOr("OPENCODE_SERVER_URL", ""), "opencode server URL the adapter should talk to (env OPENCODE_SERVER_URL)")
	password := flag.String("password", envOr("OPENCODE_SERVER_PASSWORD", ""), "opencode server basic-auth password, forwarded to the adapter (env OPENCODE_SERVER_PASSWORD)")
	useFlag := flag.Bool("adapter-flag", true, "pass --server-url <url> to the adapter in addition to OPENCODE_SERVER_URL env")
	timeout := flag.Duration("timeout", 30*time.Second, "overall deadline for the whole sequence")
	flag.Parse()

	if *adapterBin == "" {
		failf("-adapter (or ADAPTER_BIN) is required")
	}
	if *serverURL == "" {
		failf("-server-url (or OPENCODE_SERVER_URL) is required")
	}
	if _, err := os.Stat(*adapterBin); err != nil {
		failf("adapter binary %s not usable: %v", *adapterBin, err)
	}

	d := &driver{
		adapterBin: *adapterBin,
		serverURL:  *serverURL,
		password:   *password,
		useFlag:    *useFlag,
		timeout:    *timeout,
		pending:    map[string]chan *rpcResponse{},
		eventReady: make(chan struct{}, 256),
	}
	defer d.shutdown()

	if err := d.spawn(); err != nil {
		failf("spawn adapter: %v", err)
	}

	overall := time.AfterFunc(d.timeout, func() {
		d.dump("overall timeout after " + d.timeout.String())
		os.Exit(1)
	})
	defer overall.Stop()

	// Step 1: probe.
	var probe map[string]any
	step(1, "adapter.probe", func() error {
		payload, err := d.call("adapter.probe", nil, 10*time.Second)
		if err != nil {
			return err
		}
		probe = asObject(payload)
		if probe == nil {
			return fmt.Errorf("probe payload is not an object: %s", trunc(string(payload)))
		}
		if probe["available"] != true {
			return fmt.Errorf("probe reports unavailable: %s", trunc(string(payload)))
		}
		return nil
	})

	// Step 2: discover.
	var hints map[string]any
	step(2, "adapter.discover", func() error {
		payload, err := d.call("adapter.discover", nil, 15*time.Second)
		if err != nil {
			return err
		}
		disc := asObject(payload)
		if disc == nil {
			return fmt.Errorf("discover payload is not an object: %s", trunc(string(payload)))
		}
		if disc["verified"] != true {
			return fmt.Errorf("discover not verified against the opencode server: %s", trunc(string(payload)))
		}
		hints, _ = disc["session_hints"].(map[string]any)
		if len(hints) < 2 {
			return fmt.Errorf("discover returned %d session hint(s), want >= 2 (fake server presets ses_conformance_001/002): %s", len(hints), trunc(string(payload)))
		}
		return nil
	})

	// Step 3: listSessions.
	var target map[string]any
	step(3, "adapter.listSessions", func() error {
		payload, err := d.call("adapter.listSessions", nil, 15*time.Second)
		if err != nil {
			return err
		}
		list, ok := jsonToMaps(payload)
		if !ok {
			return fmt.Errorf("listSessions payload is not an array: %s", trunc(string(payload)))
		}
		if len(list) < 2 {
			return fmt.Errorf("listSessions returned %d session(s), want >= 2", len(list))
		}
		byID := map[string]bool{}
		for _, ses := range list {
			if id, _ := ses["native_session_id"].(string); id != "" {
				byID[id] = true
			}
		}
		for id := range hints {
			if !byID[id] {
				return fmt.Errorf("discover hint %q missing from listSessions result (%v)", id, keys(byID))
			}
		}
		for _, ses := range list {
			if id, _ := ses["native_session_id"].(string); id != "" {
				target = ses
				break
			}
		}
		return nil
	})
	if target == nil {
		d.dump("no session with a native_session_id to drive")
		os.Exit(1)
	}
	targetID, _ := target["native_session_id"].(string)
	logf("      driving session %s (%s)", targetID, strOr(target["title"], "<untitled>"))

	pluginID, _ := probe["plugin_id"].(string)
	nativeSession := map[string]any{
		"plugin_id":         pluginID,
		"native_session_id": targetID,
		"native_thread_id":  strOr(target["native_thread_id"], ""),
		"surface":           strOr(target["surface"], ""),
		"endpoint":          strOr(target["endpoint"], ""),
		"cwd":               strOr(target["cwd"], ""),
		"visible":           false,
	}

	// Step 4: Panel selects a native conversation before reading history.
	step(4, "adapter.controlSession(conversation.select)", func() error {
		payload, err := d.call("adapter.controlSession", map[string]any{
			"session": nativeSession, "action": "conversation.select",
		}, 10*time.Second)
		if err != nil {
			return err
		}
		selected := asObject(payload)
		details, _ := selected["details"].(map[string]any)
		if selected["ok"] != true || selected["thread_id"] != targetID || details["selected"] != true {
			return fmt.Errorf("selection not confirmed for %s: %s", targetID, trunc(string(payload)))
		}
		return nil
	})

	// Step 5: subscribe.
	step(5, "adapter.subscribe", func() error {
		payload, err := d.call("adapter.subscribe", map[string]any{
			"subscription_id": "conformance-sub-1",
			"session":         nativeSession,
		}, 10*time.Second)
		if err != nil {
			return err
		}
		sub := asObject(payload)
		if sub == nil || sub["subscribed"] != true {
			return fmt.Errorf("subscribe not confirmed: %s", trunc(string(payload)))
		}
		return nil
	})

	// Step 6: send a message.
	messageText := fmt.Sprintf("conformance hello %d", time.Now().UnixMilli())
	var receipt map[string]any
	step(6, "adapter.send", func() error {
		payload, err := d.call("adapter.send", map[string]any{
			"session": nativeSession,
			"message": map[string]any{
				"prism_message_id": "conformance-msg-1",
				"text":             messageText,
				"source_device":    "conformance-driver",
			},
		}, 15*time.Second)
		if err != nil {
			return err
		}
		receipt = asObject(payload)
		if receipt == nil {
			return fmt.Errorf("send payload is not an object: %s", trunc(string(payload)))
		}
		if receipt["accepted"] != true {
			return fmt.Errorf("send not accepted: %s", trunc(string(payload)))
		}
		return nil
	})
	nativeMessageID, _ := receipt["native_message_id"].(string)
	logf("      receipt native_message_id=%s detail=%q", nativeMessageID, strOr(receipt["detail"], ""))

	// Step 7: waitForRun returns after the run ends, answered by the real
	// terminal event (never a timeout — the fake run is still in flight here).
	step(7, "adapter.waitForRun", func() error {
		runID := d.eventDerivedRunID()
		candidates := []string{}
		if runID != "" {
			candidates = append(candidates, runID)
		}
		if nativeMessageID != "" && !contains(candidates, nativeMessageID) {
			candidates = append(candidates, nativeMessageID)
		}
		candidates = append(candidates, "") // "current run" convention
		var lastErr error
		for i, cand := range candidates {
			label := fmt.Sprintf("run_id=%q", cand)
			params := map[string]any{"session": nativeSession}
			if cand != "" {
				params["run_id"] = cand
			} else {
				label = `run_id="" (current run)`
			}
			started := time.Now()
			payload, err := d.call("adapter.waitForRun", params, 25*time.Second)
			if err == nil {
				ev := asObject(payload)
				terminalType := strOr(ev["type"], "?")
				if terminalType != "run.completed" {
					return fmt.Errorf("waitForRun (%s) returned terminal event type=%s status=%s, want run.completed (no abort happened)", label, terminalType, strOr(ev["status"], "?"))
				}
				logf("      run finished via live terminal event (%s, %.0fs wait); terminal type=%s status=%s", label, time.Since(started).Seconds(), terminalType, strOr(ev["status"], "?"))
				return nil
			}
			lastErr = fmt.Errorf("%s: %v", label, err)
			logf("      waitForRun %s failed (%v), trying next candidate", label, err)
			if i == len(candidates)-1 {
				break
			}
		}
		return lastErr
	})

	// Step 8: expect plugin.event frames on the subscription (reverse sync
	// plumbing): at least one event overall and at least one message.* event
	// carrying remote-visible content. The run has finished, so the frames
	// have landed by now (only stdio delivery latency remains).
	var eventTypes []string
	step(8, "plugin.event observed", func() error {
		deadline := time.Now().Add(20 * time.Second)
		for {
			d.mu.Lock()
			eventTypes = make([]string, 0, len(d.events))
			for _, ev := range d.events {
				eventTypes = append(eventTypes, ev.Event.Type)
			}
			count := len(d.events)
			d.mu.Unlock()
			hasMessageEvent := false
			for _, t := range eventTypes {
				if strings.HasPrefix(t, "message.") {
					hasMessageEvent = true
					break
				}
			}
			if count > 0 && hasMessageEvent {
				return nil
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("subscription events %v within 20s: want >= 1 event and >= 1 message.* event", eventTypes)
			}
			select {
			case <-d.eventReady:
			case <-time.After(100 * time.Millisecond):
			}
		}
	})
	logf("      observed event types: %s", strings.Join(eventTypes, ", "))

	// Step 9: close.
	step(9, "adapter.close", func() error {
		_, err := d.call("adapter.close", nil, 10*time.Second)
		return err
	})

	fmt.Println("CONFORMANCE PASS: probe/discover/listSessions/select/subscribe/send/events/waitForRun/close all OK")
	d.shutdown()
	os.Exit(0)
}

// ---------------------------------------------------------------------------

func (d *driver) spawn() error {
	args := []string{}
	if d.useFlag {
		args = append(args, "--server-url", d.serverURL)
	}
	d.cmd = exec.Command(d.adapterBin, args...)
	d.cmd.Env = append(os.Environ(),
		"PRISM_PLUGIN_PROTOCOL_VERSION=4",
		"OPENCODE_SERVER_URL="+d.serverURL,
	)
	if d.password != "" {
		d.cmd.Env = append(d.cmd.Env, "OPENCODE_SERVER_PASSWORD="+d.password)
	}
	stdin, err := d.cmd.StdinPipe()
	if err != nil {
		return err
	}
	d.stdin = stdin
	stdout, err := d.cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := d.cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := d.cmd.Start(); err != nil {
		return err
	}
	go d.readFrames(stdout)
	go func() {
		_, _ = io.Copy(&d.stderrTail, stderr) // bounded below by dump()
	}()
	return nil
}

func (d *driver) readFrames(stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		var resp rpcResponse
		if err := json.Unmarshal(line, &resp); err == nil && resp.ID != "" {
			d.mu.Lock()
			ch := d.pending[resp.ID]
			d.mu.Unlock()
			if ch != nil {
				cp := resp
				ch <- &cp
			}
			continue
		}
		var frame pluginEventFrame
		if err := json.Unmarshal(line, &frame); err == nil && frame.Event != "" {
			var env eventEnvelope
			_ = json.Unmarshal(frame.Payload, &env)
			env.Raw = frame.Payload
			d.mu.Lock()
			d.events = append(d.events, env)
			d.mu.Unlock()
			select {
			case d.eventReady <- struct{}{}:
			default:
			}
			continue
		}
		logf("      [adapter stdout, unparsed] %s", trunc(string(line)))
	}
	// Adapter exited: fail every pending call so the driver reports the exit
	// instead of hanging until the overall timeout.
	d.mu.Lock()
	for _, ch := range d.pending {
		select {
		case ch <- &rpcResponse{ID: "", OK: false, Error: &rpcError{Code: "adapter_exited", Message: "adapter stdout closed (process exited)"}}:
		default:
		}
	}
	d.mu.Unlock()
}

func (d *driver) call(method string, params any, timeout time.Duration) (json.RawMessage, error) {
	d.mu.Lock()
	d.nextID++
	id := fmt.Sprintf("d%d", d.nextID)
	ch := make(chan *rpcResponse, 1)
	d.pending[id] = ch
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.pending, id)
		d.mu.Unlock()
	}()

	req := rpcRequest{ID: id, Method: method, Params: params}
	line, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	_, werr := d.stdin.Write(append(line, '\n'))
	d.mu.Unlock()
	if werr != nil {
		return nil, fmt.Errorf("write %s: %w", method, werr)
	}

	select {
	case resp := <-ch:
		if !resp.OK {
			return nil, fmt.Errorf("%s -> error %s", method, resp.Error.String())
		}
		return resp.Payload, nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("%s timed out after %s", method, timeout)
	}
}

func (d *driver) eventDerivedRunID() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, env := range d.events {
		if v, ok := env.Event.Payload["run_id"].(string); ok && v != "" {
			return v
		}
		if v, ok := env.Event.Payload["runId"].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func (d *driver) shutdown() {
	if d.cmd != nil && d.cmd.Process != nil {
		done := make(chan struct{})
		go func() { _ = d.cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			_ = d.cmd.Process.Kill()
			<-done
		}
	}
}

func (d *driver) dump(reason string) {
	fmt.Printf("CONFORMANCE FAIL: %s\n", reason)
	d.mu.Lock()
	events := append([]eventEnvelope(nil), d.events...)
	d.mu.Unlock()
	fmt.Println("--- observed plugin events ---")
	if len(events) == 0 {
		fmt.Println("(none)")
	}
	for _, ev := range events {
		fmt.Printf("  type=%s status=%s summary=%q\n", ev.Event.Type, ev.Event.Status, ev.Event.Summary)
	}
	tail := d.stderrTail.Bytes()
	if len(tail) > 4096 {
		tail = tail[len(tail)-4096:]
	}
	fmt.Println("--- adapter stderr tail ---")
	fmt.Println(string(tail))
}

// step runs one numbered assertion and, on failure, dumps context and fails.
func step(n int, name string, fn func() error) {
	logf("[step %d] %s", n, name)
	if err := fn(); err != nil {
		fmt.Printf("CONFORMANCE FAIL: step %d (%s): %v\n", n, name, err)
		os.Exit(1)
	}
	fmt.Printf("        ok\n")
}

// ---------------------------------------------------------------------------
// Small helpers.

func asObject(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	return m
}

func jsonToMaps(raw json.RawMessage) ([]map[string]any, bool) {
	var list []map[string]any
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, false
	}
	return list, true
}

func strOr(v any, fallback string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return fallback
}

func contains(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func trunc(s string) string {
	if len(s) > 400 {
		return s[:400] + "..."
	}
	return s
}

func logf(format string, args ...any) {
	fmt.Printf(format+"\n", args...)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func failf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "driver: "+format+"\n", args...)
	os.Exit(2)
}
