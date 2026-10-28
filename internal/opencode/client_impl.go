package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// basicAuthUsername is the default username opencode expects alongside
// OPENCODE_SERVER_PASSWORD (overridable server-side via
// OPENCODE_SERVER_USERNAME, which is rare enough not to model here).
const basicAuthUsername = "opencode"

// maxErrorBody bounds how much of an error response we read into messages.
const maxErrorBody = 4 << 10

// httpClientState lazily provisions the shared *http.Client. It has no
// global timeout: per-request deadlines come from the caller's context
// (StreamEvents holds connections open indefinitely).
type httpClientState struct {
	once   sync.Once
	client *http.Client
}

func (s *httpClientState) get() *http.Client {
	s.once.Do(func() {
		s.client = &http.Client{
			Transport: &http.Transport{
				DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
				MaxIdleConns:          8,
				MaxIdleConnsPerHost:   4,
				IdleConnTimeout:       60 * time.Second,
				TLSHandshakeTimeout:   5 * time.Second,
				ExpectContinueTimeout: time.Second,
			},
		}
	})
	return s.client
}

// endpoint joins the client base URL with an absolute path.
func (c *Client) endpoint(path string) string {
	base, _ := c.connection()
	return strings.TrimSuffix(base, "/") + path
}

// doJSON performs one JSON request. A nil respOut only checks the status
// code (used for 204/prompt endpoints). Transport-level failures are wrapped
// with ErrServerOffline; HTTP error statuses become descriptive errors that
// embed the server's message when the body is JSON.
func (c *Client) doJSON(ctx context.Context, method, path string, query url.Values, reqOut, respOut any) error {
	var body io.Reader
	if reqOut != nil {
		encoded, err := json.Marshal(reqOut)
		if err != nil {
			return fmt.Errorf("opencode: encode %s %s request: %w", method, path, err)
		}
		body = bytes.NewReader(encoded)
	}
	base, password := c.connection()
	endpoint := strings.TrimSuffix(base, "/") + path
	if directory := directoryFromContext(ctx); directory != "" && !strings.HasPrefix(path, "/api/") && query.Get("directory") == "" {
		scoped := make(url.Values, len(query)+1)
		for key, values := range query {
			scoped[key] = values
		}
		query = scoped
		query.Set("directory", directory)
	}
	if strings.HasPrefix(path, "/api/") && (path == "/api/model" || path == "/api/model/default" || path == "/api/agent" || path == "/api/config") {
		if directory := directoryFromContext(ctx); directory != "" {
			if query == nil {
				query = url.Values{}
			}
			query.Set("location[directory]", directory)
		}
	}
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return fmt.Errorf("opencode: build %s %s request: %w", method, path, err)
	}
	if reqOut != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if password != "" {
		req.SetBasicAuth(basicAuthUsername, password)
	}

	resp, err := c.http.get().Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("opencode: %s %s: %w", method, path, ctxErr)
		}
		c.rediscover(ctx)
		return fmt.Errorf("%w: %s %s: %v", ErrServerOffline, method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return c.responseError(method, path, resp)
	}
	if respOut == nil {
		io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
		return nil
	}
	encoded, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return fmt.Errorf("opencode: read %s %s response: %w", method, path, err)
	}
	if len(bytes.TrimSpace(encoded)) == 0 {
		return fmt.Errorf("opencode: %s %s: empty response body", method, path)
	}
	if err := json.Unmarshal(encoded, respOut); err != nil {
		return fmt.Errorf("opencode: decode %s %s response: %w", method, path, err)
	}
	return nil
}

// responseError turns a non-2xx response into an error, extracting the
// server's message from the {"message": ...} or
// {"name": ..., "data": {"message": ...}} error shapes opencode uses.
func (c *Client) responseError(method, path string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	message := strings.TrimSpace(string(body))
	var structured struct {
		Name    string `json:"name"`
		Message string `json:"message"`
		Data    struct {
			Message string `json:"message"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &structured) == nil {
		switch {
		case structured.Data.Message != "":
			message = structured.Data.Message
		case structured.Message != "":
			message = structured.Message
		case structured.Name != "":
			message = structured.Name
		}
	}
	if message == "" {
		return fmt.Errorf("opencode: %s %s: unexpected status %d", method, path, resp.StatusCode)
	}
	return fmt.Errorf("opencode: %s %s: status %d: %s", method, path, resp.StatusCode, message)
}

// connectSSE opens the GET /event stream. It is separate from doJSON because
// the body is streamed, not buffered.
func (c *Client) connectSSE(ctx context.Context) (*http.Response, error) {
	v2, err := c.isV2(ctx)
	if err != nil {
		return nil, err
	}
	path := "/event"
	if v2 {
		path = "/api/event"
	}
	base, password := c.connection()
	endpoint := strings.TrimSuffix(base, "/") + path
	if directory := directoryFromContext(ctx); directory != "" && !v2 {
		endpoint += "?" + url.Values{"directory": {directory}}.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	if password != "" {
		req.SetBasicAuth(basicAuthUsername, password)
	}
	resp, err := c.http.get().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, fmt.Errorf("event stream status %d", resp.StatusCode)
	}
	return resp, nil
}

// isContextError reports whether err stems from context cancellation.
func isContextError(ctx context.Context, err error) bool {
	return err != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil)
}
