package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAutoProtocolAndV2Contract(t *testing.T) {
	probes := 0
	prompts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != "opencode" || p != "secret" {
			http.Error(w, "auth", 401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		session := `{"id":"ses_v2","title":"V2","location":{"directory":"/project"},"time":{"created":1,"updated":2}}`
		switch r.URL.Path {
		case "/api/info":
			probes++
			fmt.Fprint(w, `{"version":"2.2.0","pid":1}`)
		case "/api/session":
			if r.Method == "POST" {
				var b map[string]any
				json.NewDecoder(r.Body).Decode(&b)
				if b["location"].(map[string]any)["directory"] != "/project" || b["model"].(map[string]any)["id"] != "m" {
					t.Errorf("create body: %#v", b)
				}
				fmt.Fprintf(w, `{"data":%s}`, session)
				return
			}
			if r.URL.Query().Get("cursor") == "" {
				if r.URL.Query().Get("order") != "asc" {
					t.Error("missing initial order")
				}
				fmt.Fprintf(w, `{"data":[%s],"cursor":{"next":"page2"}}`, session)
			} else {
				if r.URL.Query().Get("order") != "" {
					t.Error("order must not accompany cursor")
				}
				fmt.Fprint(w, `{"data":[],"cursor":{}}`)
			}
		case "/api/session/ses_v2":
			if r.Method == "GET" {
				fmt.Fprintf(w, `{"data":%s}`, session)
			} else {
				w.WriteHeader(204)
			}
		case "/api/session/ses_v2/message":
			fmt.Fprint(w, `{"data":[{"id":"u","type":"user","time":{"created":1},"text":"hi","files":[{"name":"a.png","mime":"image/png","data":"AA==","source":{"type":"uri","uri":"data:image/png;base64,AA=="}}]},{"id":"a","type":"assistant","time":{"created":2},"model":{"id":"m","providerID":"p"},"content":[{"type":"text","text":"hello"},{"type":"tool","name":"shell","id":"tool1","state":{"status":"completed"}}]},{"id":"idle","type":"idle","time":{"created":3},"outcome":"succeeded"}],"cursor":{}}`)
		case "/api/session/ses_v2/prompt":
			prompts++
			var b map[string]any
			json.NewDecoder(r.Body).Decode(&b)
			if b["text"] != "hi" || b["parts"] != nil {
				t.Errorf("prompt %#v", b)
			}
			f := b["files"].([]any)[0].(map[string]any)
			if f["uri"] != "data:image/png;base64,AA==" {
				t.Errorf("file %#v", f)
			}
			fmt.Fprint(w, `{"data":{"id":"inbox1"}}`)
		case "/api/session/ses_v2/interrupt", "/api/session/ses_v2/model":
			w.WriteHeader(204)
		case "/api/model/default":
			if r.URL.Query().Get("location[directory]") != "/project" {
				t.Error("wrong location query")
			}
			fmt.Fprint(w, `{"data":{"id":"m","providerID":"p"}}`)
		case "/api/model":
			fmt.Fprint(w, `{"data":[{"id":"m","providerID":"p","name":"Model","enabled":true},{"id":"disabled","enabled":false}]}`)
		case "/api/agent":
			fmt.Fprint(w, `{"data":[{"name":"build","mode":"primary"},{"name":"title","hidden":true}]}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "secret")
	ctx := WithDirectory(context.Background(), "/project")
	s, err := c.CreateSession(ctx, CreateSessionRequest{Directory: "/project", Model: &ModelRef{ID: "m", ProviderID: "p"}})
	if err != nil || s.Directory != "/project" {
		t.Fatalf("create %v %v", s, err)
	}
	sessions, err := c.ListSessions(ctx)
	if err != nil || len(sessions) != 1 {
		t.Fatalf("list %v %v", sessions, err)
	}
	messages, err := c.ListMessages(ctx, s.ID)
	if err != nil || len(messages) != 2 || messages[1].Parts[0].Text != "hello" || messages[1].ModelID != "m" {
		t.Fatalf("messages %#v %v", messages, err)
	}
	if err = c.PromptWithParts(ctx, s.ID, []InputPart{TextInput("hi"), FileInput("image/png", "a.png", "data:image/png;base64,AA==")}); err != nil {
		t.Fatal(err)
	}
	cfg, err := c.GetConfig(ctx)
	if err != nil || cfg.Model != "p/m" {
		t.Fatalf("config %v %v", cfg, err)
	}
	models, err := c.ListModels(ctx)
	if err != nil || len(models) != 1 {
		t.Fatalf("models %v %v", models, err)
	}
	agents, err := c.ListAgents(ctx)
	if err != nil || len(agents) != 1 {
		t.Fatalf("agents %v %v", agents, err)
	}
	if _, err = c.PatchSession(ctx, s.ID, PatchSessionRequest{Title: "renamed"}); err != nil {
		t.Fatal(err)
	}
	if err = c.SwitchSessionModel(ctx, s.ID, ModelRef{ID: "m", ProviderID: "p"}); err != nil {
		t.Fatal(err)
	}
	if err = c.Abort(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	if err = c.DeleteSession(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	if probes != 1 || prompts != 1 {
		t.Fatalf("probes=%d prompts=%d", probes, prompts)
	}
}

func TestAutoProtocolLegacyAndAuthFailure(t *testing.T) {
	for _, status := range []int{404, 401, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			legacy := 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/info" {
					w.WriteHeader(status)
					return
				}
				legacy++
				fmt.Fprint(w, "[]")
			}))
			defer s.Close()
			_, err := New(s.URL, "").ListSessions(context.Background())
			if status == 404 {
				if err != nil || legacy != 1 {
					t.Fatal(err, legacy)
				}
			} else if err == nil || legacy != 0 {
				t.Fatalf("must not fall back on auth/server failure: %v %d", err, legacy)
			}
		})
	}
}

func TestV2RepeatedCursorFails(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"data":[],"cursor":{"next":"same"}}`) }))
	defer s.Close()
	c := New(s.URL, "")
	c.protocol = 2
	if _, err := c.ListMessages(context.Background(), "s"); err == nil || !strings.Contains(err.Error(), "cursor") {
		t.Fatal(err)
	}
}

func TestV2EncodedEventsAndDirectory(t *testing.T) {
	sentinel := errors.New("done")
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/event" {
			t.Errorf("path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, d := range []string{"/other", "/project"} {
			raw := fmt.Sprintf(`{"type":"session.text.delta","location":{"directory":%q},"data":{"sessionID":"s","assistantMessageID":"a","delta":"hi"}}`, d)
			encoded, _ := json.Marshal(raw)
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", encoded)
		}
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.protocol = 2
	count := 0
	err := c.StreamEvents(WithDirectory(context.Background(), "/project"), func(e Event) error {
		count++
		if e.Type != "message.part.updated" || !strings.Contains(string(e.Properties), `"text":"hi"`) {
			t.Errorf("payload %s", e.Properties)
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) || count != 1 {
		t.Fatal(err, count)
	}
}

func TestV2ServiceRegistrationPassword(t *testing.T) {
	t.Setenv("OPENCODE_PASSWORD", "")
	t.Setenv("OPENCODE_SERVER_PASSWORD", "")
	dir := t.TempDir()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, p, _ := r.BasicAuth()
		if p != "registered-secret" {
			t.Error("missing registry auth")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"version":"2.2.0","pid":1}`)
	}))
	defer s.Close()
	raw, _ := json.Marshal(map[string]any{"url": s.URL, "password": "registered-secret", "pid": 1})
	os.WriteFile(filepath.Join(dir, "service.json"), raw, 0600)
	info, err := Discover(context.Background(), DiscoverOptions{StateDir: dir})
	if err != nil || info.BaseURL != s.URL || info.Password != "registered-secret" {
		t.Fatalf("discovery %v %v", info, err)
	}
	t.Setenv("OPENCODE_PASSWORD", "preferred")
	t.Setenv("OPENCODE_SERVER_PASSWORD", "legacy")
	if ServerPassword() != "preferred" {
		t.Fatal("V2 password precedence")
	}
}

func TestV2RunOutcomeRequiresAdmittedUserBoundary(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		done       bool
		reason     string
	}{
		{"old idle only", `[{"id":"idle-old","type":"idle","outcome":"succeeded"},{"id":"wanted","type":"user"}]`, false, ""},
		{"missed success", `[{"id":"wanted","type":"user"},{"id":"idle","type":"idle","outcome":"succeeded"}]`, true, ""},
		{"missed failure", `[{"id":"wanted","type":"user"},{"id":"idle","type":"idle","outcome":"failed"}]`, true, "failed"},
		{"unrelated user", `[{"id":"other","type":"user"},{"id":"idle","type":"idle","outcome":"succeeded"}]`, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprintf(w, `{"data":%s,"cursor":{}}`, tc.body) }))
			defer s.Close()
			c := New(s.URL, "")
			c.protocol = 2
			c.lastSubmitted = map[string]string{"s": "wanted"}
			reason, done, err := c.RunOutcome(context.Background(), "s")
			if err != nil || done != tc.done || reason != tc.reason {
				t.Fatalf("outcome %q %v %v", reason, done, err)
			}
		})
	}
}

func TestLegacyHTMLInfoFallsBackToHealth(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/info" {
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, "<!doctype html><html></html>")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/global/health" {
			fmt.Fprint(w, `{"healthy":true,"version":"1.19.4"}`)
		} else {
			fmt.Fprint(w, "[]")
		}
	}))
	defer s.Close()
	if !probeServer(context.Background(), s.URL, "") {
		t.Fatal("V1 UI fallback masked real health")
	}
	rows, err := New(s.URL, "").ListSessions(context.Background())
	if err != nil || len(rows) != 0 {
		t.Fatal(rows, err)
	}
}
