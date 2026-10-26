package opencode

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// shortBackoff collapses the reconnect ladder so tests run fast; the helper
// restores the previous value on cleanup.
func shortBackoff(t *testing.T) {
	t.Helper()
	oldStart, oldMax := sseBackoffStart, sseBackoffMax
	sseBackoffStart, sseBackoffMax = time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { sseBackoffStart, sseBackoffMax = oldStart, oldMax })
}

// streamFixture is an SSE source that writes the given raw frames, flushes,
// then holds the connection open until the client goes away. Holding instead
// of returning is what a real opencode server does.
func streamFixture(frames ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for _, f := range frames {
			fmt.Fprint(w, f)
		}
		flusher.Flush()
		<-r.Context().Done()
	}
}

func collect(t *testing.T, events chan Event, want int, timeout time.Duration) []Event {
	t.Helper()
	deadline := time.After(timeout)
	var got []Event
	for len(got) < want {
		select {
		case ev := <-events:
			got = append(got, ev)
		case <-deadline:
			t.Fatalf("timed out waiting for %d events, got %d: %+v", want, len(got), got)
			return nil
		}
	}
	return got
}

func TestStreamEventsDelivery(t *testing.T) {
	shortBackoff(t)
	srv := httptest.NewServer(streamFixture(
		"data: {\"id\":\"evt_1\",\"type\":\"server.connected\",\"properties\":{}}\n\n",
		": heartbeat comment\n\n",
		"data: not-json-at-all\n\n",
		"data: {\"type\":\"session.updated\",\"properties\":{\"sessionID\":\"ses_1\",\"info\":{\"id\":\"ses_1\"}}}\n\n",
	))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan Event, 8)
	go func() {
		_ = New(srv.URL, "").StreamEvents(ctx, func(ev Event) error {
			events <- ev
			return nil
		})
	}()

	got := collect(t, events, 2, 2*time.Second)
	if got[0].Type != "server.connected" {
		t.Errorf("first event = %q, want server.connected", got[0].Type)
	}
	if got[1].Type != "session.updated" {
		t.Errorf("second event = %q, want session.updated", got[1].Type)
	}
	if properties := string(got[1].Properties); properties != `{"sessionID":"ses_1","info":{"id":"ses_1"}}` {
		t.Errorf("properties = %s", properties)
	}

	cancel()
	// no further deliveries after cancellation
	select {
	case ev := <-events:
		t.Errorf("event delivered after ctx done: %+v", ev)
	default:
	}
}

func TestStreamEventsMultilineData(t *testing.T) {
	shortBackoff(t)
	// One JSON payload split across two data: lines, per the SSE spec joined
	// by \n before parsing.
	srv := httptest.NewServer(streamFixture(
		"data: {\"type\":\"message.part.updated\",\n",
		"data:\"properties\":{\"part\":{\"type\":\"text\",\"text\":\"a\\nb\"}}}\n\n",
	))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan Event, 4)
	go func() {
		_ = New(srv.URL, "").StreamEvents(ctx, func(ev Event) error {
			events <- ev
			return nil
		})
	}()

	got := collect(t, events, 1, 2*time.Second)
	if got[0].Type != "message.part.updated" {
		t.Fatalf("event = %+v", got[0])
	}
	if !strings.Contains(string(got[0].Properties), `"a\nb"`) {
		t.Errorf("properties = %s, want embedded newline text", got[0].Properties)
	}
	cancel()
}

func TestStreamEventsContextCancelReturns(t *testing.T) {
	shortBackoff(t)
	srv := httptest.NewServer(streamFixture(
		"data: {\"type\":\"server.connected\",\"properties\":{}}\n\n",
	))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan Event, 4)
	var calls int32
	streamDone := make(chan error, 1)
	go func() {
		streamDone <- New(srv.URL, "").StreamEvents(ctx, func(ev Event) error {
			atomic.AddInt32(&calls, 1)
			events <- ev
			cancel() // stop as soon as the first event arrives
			return nil
		})
	}()

	select {
	case err := <-streamDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("StreamEvents returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("StreamEvents did not return after cancel")
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("fn called %d times after cancellation, want 1", n)
	}
}

func TestStreamEventsHandlerErrorStops(t *testing.T) {
	shortBackoff(t)
	srv := httptest.NewServer(streamFixture(
		"data: {\"type\":\"server.connected\",\"properties\":{}}\n\n",
		"data: {\"type\":\"session.idle\",\"properties\":{}}\n\n",
	))
	t.Cleanup(srv.Close)

	sentinel := errors.New("stop from handler")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := New(srv.URL, "").StreamEvents(ctx, func(ev Event) error {
		if ev.Type == "session.idle" {
			return sentinel
		}
		return nil
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("StreamEvents returned %v, want handler sentinel", err)
	}
}

func TestStreamEventsReconnectsAfterServerClose(t *testing.T) {
	shortBackoff(t)
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&requests, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		switch n {
		case 1:
			fmt.Fprint(w, "data: {\"type\":\"server.connected\",\"properties\":{}}\n\n")
			fmt.Fprint(w, "data: {\"type\":\"message.updated\",\"properties\":{\"n\":1}}\n\n")
			flusher.Flush()
			// return => server closes the stream; client must reconnect
		default:
			fmt.Fprint(w, "data: {\"type\":\"message.updated\",\"properties\":{\"n\":2}}\n\n")
			flusher.Flush()
			<-r.Context().Done()
		}
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan Event, 8)
	var delivered int32
	streamDone := make(chan error, 1)
	go func() {
		streamDone <- New(srv.URL, "").StreamEvents(ctx, func(ev Event) error {
			events <- ev
			if atomic.AddInt32(&delivered, 1) == 3 {
				cancel()
			}
			return nil
		})
	}()

	got := collect(t, events, 3, 3*time.Second)
	if got[2].Type != "message.updated" || string(got[2].Properties) != `{"n":2}` {
		t.Errorf("post-reconnect event = %+v", got[2])
	}
	select {
	case err := <-streamDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("StreamEvents returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("StreamEvents did not return after cancel")
	}
	if n := atomic.LoadInt32(&requests); n < 2 {
		t.Errorf("server saw %d requests, want >= 2 (reconnect happened)", n)
	}
}

func TestStreamEventsRetriesThroughUnavailableServer(t *testing.T) {
	shortBackoff(t)
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	err := New(srv.URL, "").StreamEvents(ctx, func(Event) error { return nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("StreamEvents returned %v, want context.DeadlineExceeded", err)
	}
	if n := atomic.LoadInt32(&requests); n < 2 {
		t.Errorf("server saw %d requests, want >= 2 (kept retrying)", n)
	}
}

func TestStreamEventsBasicAuth(t *testing.T) {
	shortBackoff(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "opencode" || pass != "pw" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		streamFixture("data: {\"type\":\"server.connected\",\"properties\":{}}\n\n")(w, r)
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan Event, 4)
	streamDone := make(chan error, 1)
	go func() {
		streamDone <- New(srv.URL, "pw").StreamEvents(ctx, func(ev Event) error {
			events <- ev
			cancel()
			return nil
		})
	}()
	collect(t, events, 1, 2*time.Second)
	select {
	case err := <-streamDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("StreamEvents returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("StreamEvents did not return")
	}
}
