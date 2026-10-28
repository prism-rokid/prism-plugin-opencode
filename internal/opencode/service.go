package opencode

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

func ServerPassword() string {
	if p := os.Getenv("OPENCODE_PASSWORD"); p != "" {
		return p
	}
	return os.Getenv("OPENCODE_SERVER_PASSWORD")
}

// RegisteredPasswordForURL supplies a V2 service credential only for the
// exact registered loopback URL. Never send it to a different explicit host.
func RegisteredPasswordForURL(rawURL, stateDir string) string {
	registered := registeredService(stateDir)
	if registered == nil || strings.TrimRight(strings.TrimSpace(rawURL), "/") != registered.BaseURL {
		return ""
	}
	return registered.Password
}

func registeredService(stateDir string) *ServerInfo {
	if stateDir == "" {
		root := os.Getenv("XDG_STATE_HOME")
		if root == "" {
			h, err := os.UserHomeDir()
			if err != nil {
				return nil
			}
			root = filepath.Join(h, ".local", "state")
		}
		stateDir = filepath.Join(root, "opencode")
	}
	raw, err := os.ReadFile(filepath.Join(stateDir, "service.json"))
	if err != nil || len(raw) > 16<<10 {
		return nil
	}
	var r struct {
		URL      string `json:"url"`
		Password string `json:"password"`
	}
	if json.Unmarshal(raw, &r) != nil {
		return nil
	}
	u, err := url.Parse(r.URL)
	if err != nil || u.Scheme != "http" || u.User != nil {
		return nil
	}
	host := u.Hostname()
	if host != "127.0.0.1" && host != "localhost" && host != "::1" {
		return nil
	}
	return &ServerInfo{BaseURL: strings.TrimRight(r.URL, "/"), Password: r.Password}
}
