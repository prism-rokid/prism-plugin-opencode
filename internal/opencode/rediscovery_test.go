package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func writeRegistration(t *testing.T, dir, url, password string) {
	t.Helper()
	raw, _ := json.Marshal(map[string]string{"url": url, "password": password})
	if err := os.WriteFile(filepath.Join(dir, "service.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestStreamRediscoveryUsesNewRegisteredAddressAndCredentials(t *testing.T) {
	t.Setenv("OPENCODE_PASSWORD", "")
	t.Setenv("OPENCODE_SERVER_PASSWORD", "")
	dir := t.TempDir()
	disconnected := make(chan struct{})
	events := make(chan string, 10)
	handler := func(pass, marker string, hold <-chan struct{}) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			_, p, _ := r.BasicAuth()
			if p != pass {
				http.Error(w, "denied", 401)
				return
			}
			if r.URL.Path == "/api/info" {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"version":"2.2.0"}`)
				return
			}
			if r.URL.Path == "/api/event" {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "data: {\"type\":\"%s\"}\n\n", marker)
				w.(http.Flusher).Flush()
				select {
				case <-hold:
				case <-r.Context().Done():
				}
				return
			}
			http.NotFound(w, r)
		}
	}
	old := httptest.NewServer(handler("old-secret", "old", disconnected))
	nextHold := make(chan struct{})
	next := httptest.NewServer(handler("new-secret", "new", nextHold))
	defer next.Close()
	defer close(nextHold)
	writeRegistration(t, dir, old.URL, "old-secret")
	c := New(old.URL, "old-secret")
	c.EnableRediscovery(DiscoverOptions{StateDir: dir, DataDir: t.TempDir()})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.StreamEvents(ctx, func(e Event) error { events <- e.Type; return nil })
	select {
	case e := <-events:
		if e != "old" {
			t.Fatal(e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("old stream did not connect")
	}
	writeRegistration(t, dir, next.URL, "new-secret")
	close(disconnected)
	old.Close()
	select {
	case e := <-events:
		if e != "new" {
			t.Fatal(e)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("new stream did not connect")
	}
	base, pass := c.connection()
	if base != next.URL || pass != "new-secret" {
		t.Fatal("address or credentials not updated")
	}
}

func TestFailedWriteIsNotReplayedAndExplicitEndpointStaysFixed(t *testing.T) {
	t.Setenv("OPENCODE_PASSWORD", "")
	t.Setenv("OPENCODE_SERVER_PASSWORD", "")
	dir := t.TempDir()
	var writes atomic.Int32
	next := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			writes.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"version":"2.2.0"}`)
	}))
	defer next.Close()
	writeRegistration(t, dir, next.URL, "")
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	oldURL := dead.URL
	dead.Close()
	c := New(oldURL, "")
	c.EnableRediscovery(DiscoverOptions{StateDir: dir, DataDir: t.TempDir()})
	err := c.doJSON(context.Background(), "POST", "/api/session", nil, map[string]string{"x": "y"}, nil)
	if err == nil || writes.Load() != 0 {
		t.Fatal("failed write must not be replayed")
	}
	if base, _ := c.connection(); base != next.URL {
		t.Fatal("future requests did not use discovered endpoint")
	}
	fixed := New(oldURL, "")
	_ = fixed.doJSON(context.Background(), "POST", "/api/session", nil, nil, nil)
	if base, _ := fixed.connection(); base != oldURL || writes.Load() != 0 {
		t.Fatal("explicit endpoint drifted")
	}
}
