package opencode

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A real server routes /event and /config by directory. Without this query,
// a session in another project completes while its watcher sees no events.
func TestDirectoryScopedRequests(t *testing.T) {
	const directory = "/projects/smoke 中文 & test"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := directory
		if r.URL.Path == "/session" {
			want = "/explicit/project"
		}
		if got := r.URL.Query().Get("directory"); got != want {
			t.Errorf("%s %s directory=%q, want %q", r.Method, r.URL.Path, got, want)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch r.URL.Path {
		case "/event":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"type\":\"session.status\",\"properties\":{\"sessionID\":\"ses_other_project\",\"status\":{\"type\":\"idle\"}}}\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case "/session":
			fmt.Fprint(w, `{"id":"ses_other_project"}`)
		default:
			fmt.Fprint(w, `{"model":"provider/model"}`)
		}
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(WithDirectory(context.Background(), directory), 2*time.Second)
	defer cancel()
	c := New(srv.URL, "")
	if _, err := c.GetConfig(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.PatchConfig(ctx, PatchConfigRequest{Model: "provider/model"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateSession(ctx, CreateSessionRequest{Directory: "/explicit/project"}); err != nil {
		t.Fatal(err)
	}
	stop := errors.New("received session idle")
	err := c.StreamEvents(ctx, func(event Event) error {
		if event.Type != "session.status" {
			t.Errorf("unexpected event: %+v", event)
		}
		return stop
	})
	if !errors.Is(err, stop) {
		t.Fatalf("expected idle event, got %v", err)
	}
}
