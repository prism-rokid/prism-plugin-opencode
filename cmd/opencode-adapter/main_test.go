package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveServerUsesConfiguredStateDirectory(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"healthy":true}`)
	}))
	defer server.Close()
	stateDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(stateDir, "service.json"), []byte(fmt.Sprintf(`{"url":%q}`, server.URL)), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PRISM_OPENCODE_STATE_DIR", stateDir)
	t.Setenv("PRISM_OPENCODE_DATA_DIR", t.TempDir())
	t.Setenv("OPENCODE_PASSWORD", "")
	t.Setenv("OPENCODE_SERVER_PASSWORD", "")
	info := resolveServer("auto")
	if info.BaseURL != server.URL {
		t.Fatalf("BaseURL = %q, want %q", info.BaseURL, server.URL)
	}
}

func TestExplicitURLReusesOnlyMatchingRegisteredCredential(t *testing.T) {
	state := t.TempDir()
	if err := os.WriteFile(filepath.Join(state, "service.json"), []byte(`{"url":"http://127.0.0.1:28894","password":"test-password"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PRISM_OPENCODE_STATE_DIR", state)
	t.Setenv("OPENCODE_PASSWORD", "")
	t.Setenv("OPENCODE_SERVER_PASSWORD", "")
	if got := resolveServer("http://127.0.0.1:28894/"); got.Password != "test-password" {
		t.Fatal("matching service credential not used")
	}
	if got := resolveServer("http://127.0.0.1:28895"); got.Password != "" {
		t.Fatal("credential leaked to a different endpoint")
	}
	t.Setenv("OPENCODE_PASSWORD", "explicit")
	if got := resolveServer("http://127.0.0.1:28894"); got.Password != "explicit" {
		t.Fatal("explicit password did not take precedence")
	}
}
