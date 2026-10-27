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
