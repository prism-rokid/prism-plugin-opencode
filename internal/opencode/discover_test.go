package opencode

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// deadPort is a port nothing userland can listen on; probes to it fail fast
// with connection refused.
const deadPort = 1

// overrideDefaultPort repoints the well-known port for one test and restores
// it afterwards.
func overrideDefaultPort(t *testing.T, port int) {
	t.Helper()
	old := defaultServerPort
	defaultServerPort = port
	t.Cleanup(func() { defaultServerPort = old })
}

// healthServer is a stand-in opencode server answering GET /global/health.
func healthServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, int) {
	t.Helper()
	if handler == nil {
		handler = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"healthy":true,"version":"1.19.4"}`)
		}
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	var port int
	fmt.Sscanf(u.Port(), "%d", &port)
	return srv, port
}

// isolatedDataDir returns an empty data dir and points XDG_DATA_HOME at it
// so the real user's opencode dir is never scanned.
func isolatedDataDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "xdg"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	t.Setenv("OPENCODE_PASSWORD", "")
	return dir
}

func TestDiscoverExplicitURL(t *testing.T) {
	srv, _ := healthServer(t, nil)
	t.Setenv("OPENCODE_SERVER_PASSWORD", "pw1")
	overrideDefaultPort(t, deadPort)
	isolatedDataDir(t)

	info, err := Discover(context.Background(), DiscoverOptions{ExplicitURL: srv.URL + "/"})
	if err != nil {
		t.Fatal(err)
	}
	if info.BaseURL != srv.URL {
		t.Errorf("BaseURL = %q, want %q (trailing slash trimmed)", info.BaseURL, srv.URL)
	}
	if info.Password != "pw1" {
		t.Errorf("Password = %q, want pw1 from env", info.Password)
	}
}

func TestDiscoverExplicitURLUsesAuth(t *testing.T) {
	srv, _ := healthServer(t, func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "opencode" || pass != "pw2" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"healthy":true}`)
	})
	t.Setenv("OPENCODE_SERVER_PASSWORD", "pw2")
	overrideDefaultPort(t, deadPort)
	isolatedDataDir(t)

	info, err := Discover(context.Background(), DiscoverOptions{ExplicitURL: srv.URL})
	if err != nil || info == nil {
		t.Fatalf("Discover = (%v, %v), want the authed server", info, err)
	}
}

func TestDiscoverFallsThroughToDefaultPort(t *testing.T) {
	_, livePort := healthServer(t, nil)
	overrideDefaultPort(t, livePort)
	isolatedDataDir(t)

	// The explicit URL is dead; discovery must continue to the well-known
	// port and find the live server there.
	deadURL := fmt.Sprintf("http://127.0.0.1:%d", deadPort)
	info, err := Discover(context.Background(), DiscoverOptions{ExplicitURL: deadURL})
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("http://127.0.0.1:%d", livePort); info.BaseURL != want {
		t.Errorf("BaseURL = %q, want %q", info.BaseURL, want)
	}
}

func TestDiscoverDefaultPort(t *testing.T) {
	_, livePort := healthServer(t, nil)
	overrideDefaultPort(t, livePort)
	isolatedDataDir(t)

	info, err := Discover(context.Background(), DiscoverOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("http://127.0.0.1:%d", livePort); info.BaseURL != want {
		t.Errorf("BaseURL = %q, want %q", info.BaseURL, want)
	}
}

func TestDiscoverViaInstanceFilePort(t *testing.T) {
	_, livePort := healthServer(t, nil)
	overrideDefaultPort(t, deadPort)
	dir := isolatedDataDir(t)

	instanceDir := filepath.Join(dir, "instance")
	if err := os.MkdirAll(instanceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(instanceDir, "12345.json"),
		[]byte(fmt.Sprintf(`{"pid":12345,"port":%d}`, livePort)), 0o644); err != nil {
		t.Fatal(err)
	}

	info, err := Discover(context.Background(), DiscoverOptions{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("http://127.0.0.1:%d", livePort); info.BaseURL != want {
		t.Errorf("BaseURL = %q, want %q from instance file", info.BaseURL, want)
	}
}

func TestDiscoverViaInstanceFileURL(t *testing.T) {
	srv, _ := healthServer(t, nil)
	overrideDefaultPort(t, deadPort)
	dir := isolatedDataDir(t)

	instanceDir := filepath.Join(dir, "process")
	if err := os.MkdirAll(instanceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(instanceDir, "a.json"),
		[]byte(fmt.Sprintf(`{"url": "%s"}`, srv.URL)), 0o644); err != nil {
		t.Fatal(err)
	}

	info, err := Discover(context.Background(), DiscoverOptions{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if info.BaseURL != srv.URL {
		t.Errorf("BaseURL = %q, want %q from instance file url", info.BaseURL, srv.URL)
	}
}

func TestDiscoverIgnoresNonPortInstanceFiles(t *testing.T) {
	overrideDefaultPort(t, deadPort)
	dir := isolatedDataDir(t)

	instanceDir := filepath.Join(dir, "instance")
	if err := os.MkdirAll(instanceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{`{"pid": 1}`, `not json`, `{"port": 0}`, `{"port": 99999}`} {
		if err := os.WriteFile(filepath.Join(instanceDir, "x.json"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	_, err := Discover(context.Background(), DiscoverOptions{})
	if !errors.Is(err, ErrServerOffline) {
		t.Fatalf("err = %v, want ErrServerOffline", err)
	}
}

func TestDiscoverOfflineListsCandidates(t *testing.T) {
	overrideDefaultPort(t, deadPort)
	dir := isolatedDataDir(t)

	deadURL := fmt.Sprintf("http://127.0.0.1:%d", deadPort)
	_, err := Discover(context.Background(), DiscoverOptions{ExplicitURL: deadURL, DataDir: dir})
	if !errors.Is(err, ErrServerOffline) {
		t.Fatalf("err = %v, want ErrServerOffline", err)
	}
	for _, want := range []string{deadURL, "127.0.0.1:1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention candidate %q", err, want)
		}
	}
}

func TestDiscoverAuthRequiredCountsAsAlive(t *testing.T) {
	srv, _ := healthServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	overrideDefaultPort(t, deadPort)
	isolatedDataDir(t)

	info, err := Discover(context.Background(), DiscoverOptions{ExplicitURL: srv.URL})
	if err != nil || info == nil {
		t.Fatalf("Discover = (%v, %v); 401 proves the server is alive", info, err)
	}
}

func TestDiscoverRejectsNonJSONResponder(t *testing.T) {
	srv, _ := healthServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html><body>hello</body></html>")
	})
	overrideDefaultPort(t, deadPort)
	isolatedDataDir(t)

	_, err := Discover(context.Background(), DiscoverOptions{ExplicitURL: srv.URL})
	if !errors.Is(err, ErrServerOffline) {
		t.Fatalf("err = %v, want ErrServerOffline (an HTML page is not opencode)", err)
	}
}

func TestDiscoverRespectsCancelledContext(t *testing.T) {
	_, livePort := healthServer(t, nil)
	overrideDefaultPort(t, livePort)
	isolatedDataDir(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Discover(ctx, DiscoverOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
