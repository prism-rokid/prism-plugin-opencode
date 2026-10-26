package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	pluginbridge "github.com/Rokid-Prism/prism-plugin-sdk"

	"github.com/Rokid-Prism/prism-plugin-opencode/internal/adapter"
	"github.com/Rokid-Prism/prism-plugin-opencode/internal/opencode"
)

// defaultServerURL is used when discovery finds nothing at startup so the
// adapter still probes (and picks the server up later) instead of dying with
// an unresolvable endpoint.
const defaultServerURL = "http://127.0.0.1:4096"

func main() {
	fs := flag.NewFlagSet("opencode-adapter", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	serverURL := fs.String("server-url", envOr("PRISM_OPENCODE_SERVER_URL", envOr("OPENCODE_SERVER_URL", "auto")),
		"opencode server base URL, or auto to discover a running server (env PRISM_OPENCODE_SERVER_URL, then OPENCODE_SERVER_URL)")
	if err := fs.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		os.Exit(2)
	}
	if err := pluginbridge.CheckProtocolVersion(); err != nil {
		fmt.Fprintf(os.Stderr, "opencode-adapter: %v\n", err)
		os.Exit(1)
	}
	baseURL := resolveServerURL(*serverURL)
	password := strings.TrimSpace(os.Getenv("OPENCODE_SERVER_PASSWORD"))
	server := &pluginbridge.StdioServer{
		Adapter: adapter.New(adapter.Config{
			Client:   opencode.New(baseURL, password),
			Endpoint: baseURL,
		}),
		In:  os.Stdin,
		Out: os.Stdout,
	}
	if err := server.Run(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "opencode-adapter: pluginbridge server failed: %v\n", err)
		os.Exit(1)
	}
}

// resolveServerURL resolves an explicit URL or discovers a running server
// (explicit flag/env wins, then opencode.Discover, then the well-known
// default port as a late-binding fallback).
func resolveServerURL(flagged string) string {
	url := strings.TrimSpace(flagged)
	if url != "" && url != "auto" {
		return strings.TrimRight(url, "/")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if info, err := opencode.Discover(ctx, opencode.DiscoverOptions{}); err == nil && info != nil && strings.TrimSpace(info.BaseURL) != "" {
		return strings.TrimRight(strings.TrimSpace(info.BaseURL), "/")
	}
	return defaultServerURL
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
