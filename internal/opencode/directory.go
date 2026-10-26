package opencode

import (
	"context"
	"strings"
)

type directoryKey struct{}

// WithDirectory scopes config and event requests to the native session's
// project instance. OpenCode's /event bus is local to this directory.
func WithDirectory(ctx context.Context, directory string) context.Context {
	if directory = strings.TrimSpace(directory); directory != "" {
		return context.WithValue(ctx, directoryKey{}, directory)
	}
	return ctx
}

func directoryFromContext(ctx context.Context) string {
	directory, _ := ctx.Value(directoryKey{}).(string)
	return directory
}
