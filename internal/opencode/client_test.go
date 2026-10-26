package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Wire fixtures below are captured verbatim from a live opencode 1.19.4
// server (field names, casing and nesting intact).

const fixtureSession = `{
  "id": "ses_e888e8811ffeTUjB48JKyDMLzB",
  "slug": "crisp-pixel",
  "projectID": "global",
  "directory": "/private/tmp",
  "path": "private/tmp",
  "cost": 0,
  "tokens": {"input": 0, "output": 0, "reasoning": 0, "cache": {"read": 0, "write": 0}},
  "share": {"url": "https://opencode.ai/s/x"},
  "title": "prism-test",
  "version": "1.19.4",
  "time": {"created": 1793005287407, "updated": 1793005287500}
}`

const fixtureSession2 = `{
  "id": "ses_e888e2fb8ffeAarL45jEYo7eN4",
  "slug": "curious-squid",
  "title": "older",
  "directory": "/private/tmp",
  "time": {"created": 1793005285900, "updated": 1793005286000}
}`

// One user message (nested model object) and one assistant message
// (top-level modelID/providerID/cost), each wrapped in the {info, parts}
// envelope the list endpoint really returns.
const fixtureMessages = `[
  {
    "info": {
      "id": "msg_17771d0c9001VP5TYyD22CoLk8",
      "sessionID": "ses_e888e8811ffeTUjB48JKyDMLzB",
      "role": "user",
      "time": {"created": 1793005310153},
      "agent": "build",
      "model": {"providerID": "amd", "modelID": "Qwen3.8-Flash-Next", "variant": ""}
    },
    "parts": [
      {"id": "prt_1", "messageID": "msg_17771d0c9001VP5TYyD22CoLk8", "sessionID": "ses_e888e8811ffeTUjB48JKyDMLzB", "type": "text", "text": "Reply with exactly: pong"}
    ]
  },
  {
    "info": {
      "id": "msg_17771d0dc001G63RXKxgwOdlZ6",
      "sessionID": "ses_e888e8811ffeTUjB48JKyDMLzB",
      "role": "assistant",
      "time": {"created": 1793005310172, "completed": 1793005311900},
      "modelID": "Qwen3.8-Flash-Next",
      "providerID": "amd",
      "cost": 0.0012,
      "tokens": {"total": 900, "input": 100, "output": 800}
    },
    "parts": [
      {"id": "prt_2", "type": "step-start"},
      {"id": "prt_3", "type": "text", "text": "pong"},
      {"id": "prt_4", "type": "tool", "callID": "call_1", "tool": "read", "state": {"status": "completed", "output": "..."}}
    ]
  }
]`

const fixtureAgents = `[
  {"name": "build", "description": "The default agent. Executes tools based on configured permissions.", "mode": "primary", "native": true},
  {"name": "plan", "description": "Plan mode. Disallows all edit tools.", "mode": "primary", "native": true},
  {"name": "general", "description": "General-purpose agent.", "mode": "subagent", "native": true},
  {"name": "title", "mode": "primary", "native": true, "hidden": true},
  {"name": "summary", "mode": "primary", "native": true, "hidden": true}
]`

const fixtureProviders = `{
  "providers": [
    {
      "id": "minimax-cn-coding-plan",
      "name": "MiniMax Token Plan (minimax.cn)",
      "source": "api",
      "models": {
        "MiniMax-M3.1-Flash": {"id": "MiniMax-M3.1-Flash", "providerID": "minimax-cn-coding-plan", "name": "MiniMax-M3.1-Flash"},
        "MiniMax-M3": {"id": "MiniMax-M3", "providerID": "minimax-cn-coding-plan", "name": "MiniMax M3"}
      }
    },
    {
      "id": "amd",
      "name": "AMD",
      "source": "api",
      "models": {
        "Qwen3.8-Flash-Next": {"id": "Qwen3.8-Flash-Next", "providerID": "amd"}
      }
    }
  ],
  "default": {"amd": "Qwen3.8-Flash-Next"}
}`

const fixtureConfig = `{
  "model": "amd/Qwen3.8-Flash-Next",
  "logLevel": "INFO",
  "username": "tyy",
  "agent": {"build": {"mode": "primary"}}
}`

// fakeServer spins an httptest server whose handler also serves as the
// assertion point for the exact wire request the client produced.
func fakeServer(t *testing.T, handle func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, *Client) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(handle))
	t.Cleanup(srv.Close)
	return srv, New(srv.URL, "")
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprint(w, body)
}

func TestListSessions(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		status   int
		wantIDs  []string
		wantErr  string
		checkRaw func(*testing.T, []Session)
	}{
		{
			name:    "sorted newest-updated first, raw preserved",
			body:    "[" + fixtureSession2 + "," + fixtureSession + "]",
			status:  http.StatusOK,
			wantIDs: []string{"ses_e888e8811ffeTUjB48JKyDMLzB", "ses_e888e2fb8ffeAarL45jEYo7eN4"},
			checkRaw: func(t *testing.T, sessions []Session) {
				if v, ok := sessions[0].Raw["slug"].(string); !ok || v != "crisp-pixel" {
					t.Errorf("Raw[slug] = %v, want crisp-pixel", sessions[0].Raw["slug"])
				}
				share, ok := sessions[0].Raw["share"].(map[string]any)
				if !ok || share["url"] != "https://opencode.ai/s/x" {
					t.Errorf("Raw[share] = %v, want share.url", sessions[0].Raw["share"])
				}
				if _, exists := sessions[0].Raw["id"]; exists {
					t.Errorf("Raw should not contain modelled field id")
				}
			},
		},
		{name: "empty list", body: "[]", status: http.StatusOK, wantIDs: []string{}},
		{
			name:    "server error surfaces status and message",
			body:    `{"message": "boom"}`,
			status:  http.StatusInternalServerError,
			wantErr: "500: boom",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, client := fakeServer(t, func(w http.ResponseWriter, r *http.Request) {
				writeJSON(w, tt.status, tt.body)
			})
			sessions, err := client.ListSessions(context.Background())
			switch {
			case tt.wantErr != "":
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ListSessions error = %v, want containing %q", err, tt.wantErr)
				}
			case err != nil:
				t.Fatalf("ListSessions: %v", err)
			default:
				if len(sessions) != len(tt.wantIDs) {
					t.Fatalf("got %d sessions, want %d", len(sessions), len(tt.wantIDs))
				}
				for i, want := range tt.wantIDs {
					if sessions[i].ID != want {
						t.Errorf("sessions[%d].ID = %s, want %s", i, sessions[i].ID, want)
					}
				}
				if len(sessions) > 0 {
					if sessions[0].Time.Updated != 1793005287500 {
						t.Errorf("Time.Updated = %d, want 1793005287500", sessions[0].Time.Updated)
					}
				}
				if tt.checkRaw != nil {
					tt.checkRaw(t, sessions)
				}
			}
		})
	}
}

func TestGetSession(t *testing.T) {
	t.Run("found", func(t *testing.T) {
		_, client := fakeServer(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/session/ses_x" {
				t.Errorf("path = %s, want /session/ses_x", r.URL.Path)
			}
			writeJSON(w, http.StatusOK, fixtureSession)
		})
		s, err := client.GetSession(context.Background(), "ses_x")
		if err != nil {
			t.Fatal(err)
		}
		if s.Title != "prism-test" || s.Directory != "/private/tmp" || s.Time.Created != 1793005287407 {
			t.Errorf("session decoded wrong: %+v", s)
		}
	})

	t.Run("not found surfaces server message", func(t *testing.T) {
		_, client := fakeServer(t, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusNotFound, `{"name":"NotFoundError","data":{"message":"Session not found: nope"}}`)
		})
		_, err := client.GetSession(context.Background(), "nope")
		if err == nil || !strings.Contains(err.Error(), "Session not found: nope") || !strings.Contains(err.Error(), "404") {
			t.Fatalf("error = %v, want 404 + server message", err)
		}
	})
}

func TestCreateSession(t *testing.T) {
	t.Run("body fields plus directory query param", func(t *testing.T) {
		_, client := fakeServer(t, func(w http.ResponseWriter, r *http.Request) {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode body: %v", err)
			}
			// directory must NOT be in the body: the v1 API takes it as a
			// query parameter.
			if _, ok := body["directory"]; ok {
				t.Errorf("directory leaked into JSON body: %v", body)
			}
			if body["title"] != "t" || body["parentID"] != "p" {
				t.Errorf("body = %v, want title=t parentID=p", body)
			}
			if r.URL.Query().Get("directory") != "/work" {
				t.Errorf("query directory = %q, want /work", r.URL.Query().Get("directory"))
			}
			writeJSON(w, http.StatusOK, fixtureSession)
		})
		s, err := client.CreateSession(context.Background(), CreateSessionRequest{Title: "t", ParentID: "p", Directory: "/work"})
		if err != nil {
			t.Fatal(err)
		}
		if s.ID != "ses_e888e8811ffeTUjB48JKyDMLzB" {
			t.Errorf("session id = %s", s.ID)
		}
	})
	t.Run("no directory means no query", func(t *testing.T) {
		_, client := fakeServer(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.RawQuery != "" {
				t.Errorf("unexpected query %q", r.URL.RawQuery)
			}
			writeJSON(w, http.StatusOK, fixtureSession)
		})
		if _, err := client.CreateSession(context.Background(), CreateSessionRequest{Title: "t"}); err != nil {
			t.Fatal(err)
		}
	})
}

func TestPatchSession(t *testing.T) {
	_, client := fakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch || r.URL.Path != "/session/ses_x" {
			t.Errorf("request %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["title"] != "renamed" || len(body) != 1 {
			t.Errorf("body = %v, want only title", body)
		}
		writeJSON(w, http.StatusOK, fixtureSession)
	})
	s, err := client.PatchSession(context.Background(), "ses_x", PatchSessionRequest{Title: "renamed"})
	if err != nil || s.Title != "prism-test" {
		t.Fatalf("PatchSession = (%v, %v)", s, err)
	}
}

func TestDeleteSession(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		_, client := fakeServer(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodDelete || r.URL.Path != "/session/ses_x" {
				t.Errorf("request %s %s", r.Method, r.URL.Path)
			}
			writeJSON(w, http.StatusOK, "true")
		})
		if err := client.DeleteSession(context.Background(), "ses_x"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("gone", func(t *testing.T) {
		_, client := fakeServer(t, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusNotFound, `{"name":"NotFoundError","data":{"message":"Session not found: ses_x"}}`)
		})
		if err := client.DeleteSession(context.Background(), "ses_x"); err == nil {
			t.Fatal("want error for 404")
		}
	})
}

func TestListMessages(t *testing.T) {
	t.Run("envelope decode, model lift, part raw", func(t *testing.T) {
		_, client := fakeServer(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/session/ses_x/message" {
				t.Errorf("path = %s", r.URL.Path)
			}
			writeJSON(w, http.StatusOK, fixtureMessages)
		})
		msgs, err := client.ListMessages(context.Background(), "ses_x")
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 2 {
			t.Fatalf("got %d messages", len(msgs))
		}
		user := msgs[0]
		if user.Role != "user" || user.Time.Created != 1793005310153 {
			t.Errorf("user message = %+v", user)
		}
		// user messages nest the model; the client lifts it
		if user.ModelID != "Qwen3.8-Flash-Next" || user.ProviderID != "amd" {
			t.Errorf("lifted model = %s/%s", user.ProviderID, user.ModelID)
		}
		if _, ok := user.Raw["model"]; ok {
			t.Errorf("model should not stay in Raw once lifted")
		}
		if user.Raw["agent"] != "build" {
			t.Errorf("Raw[agent] = %v, want build", user.Raw["agent"])
		}
		if len(user.Parts) != 1 || user.Parts[0].Type != "text" || user.Parts[0].Text != "Reply with exactly: pong" {
			t.Errorf("user parts = %+v", user.Parts)
		}
		if user.Parts[0].Raw["messageID"] != "msg_17771d0c9001VP5TYyD22CoLk8" {
			t.Errorf("part raw lost messageID: %v", user.Parts[0].Raw)
		}

		assistant := msgs[1]
		if assistant.Role != "assistant" || assistant.Cost != 0.0012 {
			t.Errorf("assistant = %+v", assistant)
		}
		if assistant.ModelID != "Qwen3.8-Flash-Next" || assistant.ProviderID != "amd" {
			t.Errorf("assistant model = %s/%s", assistant.ProviderID, assistant.ModelID)
		}
		if len(assistant.Parts) != 3 {
			t.Fatalf("assistant parts = %d, want 3 (order preserved)", len(assistant.Parts))
		}
		if assistant.Parts[0].Type != "step-start" || assistant.Parts[1].Text != "pong" || assistant.Parts[2].Type != "tool" {
			t.Errorf("part order wrong: %+v", assistant.Parts)
		}
		tool := assistant.Parts[2].Raw
		if tool["callID"] != "call_1" || tool["tool"] != "read" {
			t.Errorf("tool part raw = %v", tool)
		}
		// time.completed is unmodelled, so the whole time object stays in Raw
		timeObj, ok := assistant.Raw["time"].(map[string]any)
		if !ok || timeObj["completed"] != float64(1793005311900) {
			t.Errorf("Raw[time] = %v, want completed timestamp preserved", assistant.Raw["time"])
		}
	})

	t.Run("newest-first payload is reversed to chronological", func(t *testing.T) {
		_, client := fakeServer(t, func(w http.ResponseWriter, r *http.Request) {
			var raw []json.RawMessage
			if err := json.Unmarshal([]byte(fixtureMessages), &raw); err != nil {
				t.Fatalf("fixture: %v", err)
			}
			for i, j := 0, len(raw)-1; i < j; i, j = i+1, j-1 {
				raw[i], raw[j] = raw[j], raw[i]
			}
			out, _ := json.Marshal(raw)
			writeJSON(w, http.StatusOK, string(out))
		})
		msgs, err := client.ListMessages(context.Background(), "ses_x")
		if err != nil {
			t.Fatal(err)
		}
		if msgs[0].Role != "user" || msgs[1].Role != "assistant" {
			t.Errorf("order = %s, %s; want user first", msgs[0].Role, msgs[1].Role)
		}
	})
}

func TestPrompt(t *testing.T) {
	t.Run("fire and forget expects 204", func(t *testing.T) {
		_, client := fakeServer(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || r.URL.Path != "/session/ses_x/prompt_async" {
				t.Errorf("request %s %s", r.Method, r.URL.Path)
			}
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode: %v", err)
			}
			want := map[string]any{
				"parts": []any{map[string]any{"type": "text", "text": "hello world"}},
			}
			got, _ := json.Marshal(body)
			wantJSON, _ := json.Marshal(want)
			if string(got) != string(wantJSON) {
				t.Errorf("body = %s, want %s", got, wantJSON)
			}
			w.WriteHeader(http.StatusNoContent)
		})
		if err := client.Prompt(context.Background(), "ses_x", "hello world"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("unknown session", func(t *testing.T) {
		_, client := fakeServer(t, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusNotFound, `{"name":"NotFoundError","data":{"message":"Session not found: gone"}}`)
		})
		if err := client.Prompt(context.Background(), "gone", "hi"); err == nil || !strings.Contains(err.Error(), "Session not found") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestAbort(t *testing.T) {
	_, client := fakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/session/ses_x/abort" {
			t.Errorf("request %s %s", r.Method, r.URL.Path)
		}
		writeJSON(w, http.StatusOK, "true")
	})
	if err := client.Abort(context.Background(), "ses_x"); err != nil {
		t.Fatal(err)
	}
}

func TestConfig(t *testing.T) {
	t.Run("get", func(t *testing.T) {
		_, client := fakeServer(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/config" {
				t.Errorf("path = %s", r.URL.Path)
			}
			writeJSON(w, http.StatusOK, fixtureConfig)
		})
		cfg, err := client.GetConfig(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Model != "amd/Qwen3.8-Flash-Next" {
			t.Errorf("model = %q", cfg.Model)
		}
		if cfg.Raw["logLevel"] != "INFO" {
			t.Errorf("Raw[logLevel] = %v", cfg.Raw["logLevel"])
		}
	})
	t.Run("patch sends provider/model form", func(t *testing.T) {
		_, client := fakeServer(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPatch {
				t.Errorf("method = %s", r.Method)
			}
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if body["model"] != "minimax-cn-coding-plan/MiniMax-M3" || len(body) != 1 {
				t.Errorf("body = %v", body)
			}
			writeJSON(w, http.StatusOK, `{"model": "minimax-cn-coding-plan/MiniMax-M3"}`)
		})
		cfg, err := client.PatchConfig(context.Background(), PatchConfigRequest{Model: "minimax-cn-coding-plan/MiniMax-M3"})
		if err != nil || cfg.Model != "minimax-cn-coding-plan/MiniMax-M3" {
			t.Fatalf("PatchConfig = (%v, %v)", cfg, err)
		}
	})
}

func TestListAgents(t *testing.T) {
	_, client := fakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agent" {
			t.Errorf("path = %s", r.URL.Path)
		}
		writeJSON(w, http.StatusOK, fixtureAgents)
	})
	agents, err := client.ListAgents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, a := range agents {
		names = append(names, a.Name)
	}
	want := []string{"build", "plan", "general"}
	if len(agents) != 3 || strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("agents = %v, want %v (hidden agents filtered)", names, want)
	}
	if agents[0].Mode != "primary" || !strings.Contains(agents[0].Description, "default agent") {
		t.Errorf("agent fields wrong: %+v", agents[0])
	}
}

func TestListModels(t *testing.T) {
	_, client := fakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/config/providers" {
			t.Errorf("path = %s", r.URL.Path)
		}
		writeJSON(w, http.StatusOK, fixtureProviders)
	})
	models, err := client.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []ModelInfo{
		{ProviderID: "amd", ModelID: "Qwen3.8-Flash-Next", Name: "Qwen3.8-Flash-Next"},
		{ProviderID: "minimax-cn-coding-plan", ModelID: "MiniMax-M3", Name: "MiniMax M3"},
		{ProviderID: "minimax-cn-coding-plan", ModelID: "MiniMax-M3.1-Flash", Name: "MiniMax-M3.1-Flash"},
	}
	if len(models) != len(want) {
		t.Fatalf("models = %+v, want %+v", models, want)
	}
	for i := range want {
		if models[i] != want[i] {
			t.Errorf("models[%d] = %+v, want %+v", i, models[i], want[i])
		}
	}
}

func TestBasicAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "opencode" || pass != "s3cret" {
			w.Header().Set("WWW-Authenticate", `Basic realm="opencode"`)
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"message":"Unauthorized"}`)
			return
		}
		writeJSON(w, http.StatusOK, `[`+fixtureSession+`]`)
	}))
	t.Cleanup(srv.Close)

	t.Run("no password rejected", func(t *testing.T) {
		client := New(srv.URL, "")
		_, err := client.ListSessions(context.Background())
		if err == nil || !strings.Contains(err.Error(), "401") {
			t.Fatalf("err = %v, want 401", err)
		}
	})
	t.Run("wrong password rejected", func(t *testing.T) {
		client := New(srv.URL, "wrong")
		_, err := client.ListSessions(context.Background())
		if err == nil || !strings.Contains(err.Error(), "401") {
			t.Fatalf("err = %v, want 401", err)
		}
	})
	t.Run("user opencode + password accepted", func(t *testing.T) {
		client := New(srv.URL, "s3cret")
		sessions, err := client.ListSessions(context.Background())
		if err != nil || len(sessions) != 1 {
			t.Fatalf("ListSessions = (%v, %v)", sessions, err)
		}
	})
}

func TestConnectionFailuresWrapErrServerOffline(t *testing.T) {
	// Grab a port that is guaranteed closed.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no free port: %v", err)
	}
	deadURL := "http://" + listener.Addr().String()
	listener.Close()

	client := New(deadURL, "")
	_, err = client.ListSessions(context.Background())
	if !errors.Is(err, ErrServerOffline) {
		t.Fatalf("err = %v, want ErrServerOffline", err)
	}
	if err != nil && !strings.Contains(err.Error(), "/session") {
		t.Errorf("error lacks context: %v", err)
	}
}

func TestContextCancellationPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	client := New(srv.URL, "")
	_, err := client.GetSession(ctx, "ses_x")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
