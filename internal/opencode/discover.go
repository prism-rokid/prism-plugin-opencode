package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// defaultServerPort is the port `opencode serve` binds by default. A var so
// tests can point discovery at a throwaway listener.
var defaultServerPort = 4096

// probeTimeout bounds each individual health probe.
const probeTimeout = 1500 * time.Millisecond

// discover implements Discover.
//
// Research note (opencode v1 up to 1.19.4): the stock server never writes an
// instance/port registry file — the TUI's random port only goes to stdout and
// IDE integrations spawn `opencode serve` themselves. The data-dir scan below
// is therefore best-effort: it recognises {port|url} JSON files under the
// data dir (instance/*.json, process/*.json, server.json, port.json) so that
// future versions or third-party launchers that do publish a port are picked
// up, but stock installs rely on the explicit URL or port 4096.
func discover(ctx context.Context, opts DiscoverOptions) (*ServerInfo, error) {
	password := ServerPassword()

	candidates := []string{}
	addCandidate := func(raws ...string) {
		for _, raw := range raws {
			u := strings.TrimSuffix(strings.TrimSpace(raw), "/")
			if u == "" {
				continue
			}
			duplicate := false
			for _, existing := range candidates {
				if existing == u {
					duplicate = true
					break
				}
			}
			if !duplicate {
				candidates = append(candidates, u)
			}
		}
	}

	if opts.ExplicitURL != "" {
		addCandidate(opts.ExplicitURL)
	}
	registered := registeredService(opts.StateDir)
	if registered != nil {
		addCandidate(registered.BaseURL)
	}
	addCandidate(fmt.Sprintf("http://127.0.0.1:%d", defaultServerPort))
	addCandidate(dataDirCandidates(opts.DataDir)...)

	var failures []string
	for _, base := range candidates {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		candidatePassword := password
		if registered != nil && base == registered.BaseURL && candidatePassword == "" {
			candidatePassword = registered.Password
		}
		if probeServer(ctx, base, candidatePassword) {
			return &ServerInfo{BaseURL: base, Password: candidatePassword}, nil
		}
		failures = append(failures, base)
	}
	return nil, fmt.Errorf("%w: no opencode server responded (tried %s)",
		ErrServerOffline, strings.Join(failures, ", "))
}

// probeServer reports whether an opencode server answers at base. It uses
// GET /global/health (v1.x) and falls back to GET /config for older builds.
// A 401 proves a server is alive (password-protected).
func probeServer(ctx context.Context, base, password string) bool {
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	for _, path := range []string{"/api/info", "/global/health", "/config"} {
		req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, strings.TrimSuffix(base, "/")+path, nil)
		if err != nil {
			return false
		}
		if password != "" {
			req.SetBasicAuth(basicAuthUsername, password)
		}
		resp, err := (&http.Client{Timeout: probeTimeout}).Do(req)
		if err != nil {
			continue
		}
		status := resp.StatusCode
		ct := resp.Header.Get("Content-Type")
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
		resp.Body.Close()
		switch status {
		case http.StatusOK:
			// Accept a JSON-ish body: opencode always answers JSON here, so
			// an HTML page on the same port does not count as a hit.
			if strings.Contains(ct, "json") {
				return true
			}
			trimmed := bytes.TrimSpace(body)
			if len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') {
				return true
			}
			// V1 serves its HTML UI for unknown /api/info. Continue to its
			// real health endpoint instead of treating that UI as offline.
			continue
		case http.StatusUnauthorized, http.StatusForbidden:
			return true // alive, just locked down
		}
	}
	return false
}

// dataDirCandidates returns candidate base URLs published as instance files
// under the opencode data directory.
func dataDirCandidates(dataDir string) []string {
	dirs := dataDirPaths(dataDir)
	var urls []string
	for _, dir := range dirs {
		urls = append(urls, scanInstanceFiles(dir)...)
	}
	return urls
}

// dataDirPaths resolves the opencode data directory: an explicit override
// first, then XDG. opencode uses XDG-style paths on every platform
// (~/.local/share/opencode even on macOS; verified against 1.19.4 —
// ~/Library/Application Support is only used system-wide for managed
// enterprise config, never for user data).
func dataDirPaths(dataDir string) []string {
	if dataDir != "" {
		return []string{dataDir}
	}
	var dirs []string
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		dirs = append(dirs, filepath.Join(xdg, "opencode"))
	} else if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".local", "share", "opencode"))
	}
	if runtime.GOOS == "windows" {
		if appData := os.Getenv("AppData"); appData != "" {
			dirs = append(dirs, filepath.Join(appData, "opencode"))
		}
	}
	return dirs
}

// scanInstanceFiles looks for JSON files that may publish a server port:
// instance/*.json and process/*.json subdirectories plus top-level
// server.json / port.json. Newest files win.
func scanInstanceFiles(dir string) []string {
	var files []string
	for _, name := range []string{"instance", "process"} {
		entries, err := os.ReadDir(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			files = append(files, filepath.Join(dir, name, e.Name()))
		}
	}
	for _, name := range []string{"server.json", "port.json"} {
		if info, err := os.Stat(filepath.Join(dir, name)); err == nil && !info.IsDir() {
			files = append(files, filepath.Join(dir, name))
		}
	}
	if len(files) == 0 {
		return nil
	}
	sort.Slice(files, func(i, j int) bool {
		fi, erri := os.Stat(files[i])
		fj, errj := os.Stat(files[j])
		if erri != nil || errj != nil {
			return files[i] < files[j]
		}
		return fi.ModTime().After(fj.ModTime())
	})

	var urls []string
	for _, file := range files {
		if url := instanceFileURL(file); url != "" {
			urls = append(urls, url)
		}
	}
	return urls
}

// instanceFileURL extracts a base URL from one candidate instance file. It
// accepts {"url": "..."} or {"port": 1234[, "hostname": "…"]}.
func instanceFileURL(file string) string {
	data, err := os.ReadFile(file)
	if err != nil || len(data) > 16<<10 {
		return ""
	}
	var doc map[string]any
	if json.Unmarshal(data, &doc) != nil {
		return ""
	}
	if u, ok := doc["url"].(string); ok && u != "" {
		if !strings.Contains(u, "://") {
			u = "http://" + u
		}
		return strings.TrimSuffix(u, "/")
	}
	port, ok := doc["port"].(float64)
	if !ok || port < 1 || port > 65535 {
		return ""
	}
	host := "127.0.0.1"
	if h, ok := doc["hostname"].(string); ok && h != "" {
		host = h
	}
	return fmt.Sprintf("http://%s:%d", host, int(port))
}
