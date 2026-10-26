// Package adapter implements the PluginBridge protocol surface (protocol
// version 4) on top of one opencode server: PluginBridge methods translate
// onto the opencode HTTP API, and native SSE events translate back into
// PluginEvent streams and history.stream events.
//
// The adapter never talks HTTP itself. It depends on the Client interface
// below, whose method set mirrors (*opencode.Client) verbatim; main resolves
// the server (flag/env, or opencode.Discover) and injects the real client.
package adapter

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	pluginbridge "github.com/Rokid-Prism/prism-plugin-sdk"

	"github.com/Rokid-Prism/prism-plugin-opencode/internal/opencode"
)

// Client is the opencode server surface this adapter depends on. The method
// set is copied verbatim from the exported methods of *opencode.Client so the
// real client satisfies Client directly and tests can substitute a local
// mock. Discover is deliberately absent: server discovery happens in main
// (opencode.Discover + opencode.New) before the client is injected.
type Client interface {
	ListSessions(ctx context.Context) ([]opencode.Session, error)
	GetSession(ctx context.Context, id string) (*opencode.Session, error)
	CreateSession(ctx context.Context, req opencode.CreateSessionRequest) (*opencode.Session, error)
	PatchSession(ctx context.Context, id string, patch opencode.PatchSessionRequest) (*opencode.Session, error)
	DeleteSession(ctx context.Context, id string) error
	SwitchSessionModel(ctx context.Context, id string, model opencode.ModelRef) error
	ListMessages(ctx context.Context, sessionID string) ([]opencode.Message, error)
	Prompt(ctx context.Context, sessionID, text string) error
	PromptWithParts(ctx context.Context, sessionID string, parts []opencode.InputPart) error
	Abort(ctx context.Context, sessionID string) error
	GetConfig(ctx context.Context) (*opencode.Config, error)
	PatchConfig(ctx context.Context, patch opencode.PatchConfigRequest) (*opencode.Config, error)
	ListAgents(ctx context.Context) ([]opencode.Agent, error)
	ListModels(ctx context.Context) ([]opencode.ModelInfo, error)
	StreamEvents(ctx context.Context, fn func(opencode.Event) error) error
}

// Compile-time proof that the real client satisfies the interface this
// adapter programs against. If opencode.Client's public surface changes,
// this assertion fails the build.
var _ Client = (*opencode.Client)(nil)

const (
	defaultProbeTimeout      = 5 * time.Second
	defaultVisibilityTimeout = 15 * time.Second
	defaultWaitRunTimeout    = 10 * time.Minute

	maxHistoryTurnLimit = 20

	// Surface identifies the native surface type in Capability and
	// DiscoveryResult. It is a stable Prism-facing key, not a native API name.
	Surface = "server"
)

// Adapter implements the PluginBridge protocol over one opencode server.
type Adapter struct {
	client   Client
	endpoint string

	probeTimeout      time.Duration
	visibilityTimeout time.Duration
	waitRunTimeout    time.Duration
	draftMu           sync.Mutex
	drafts            map[string]*draftState
}

// Config constructs an Adapter. Endpoint is the opencode server base URL
// (used for DiscoveryResult/NativeSession.Endpoint); timeouts are optional
// and default to sane values for tests and production alike.
type Config struct {
	Client   Client
	Endpoint string

	ProbeTimeout      time.Duration
	VisibilityTimeout time.Duration
	WaitRunTimeout    time.Duration
}

// New builds an Adapter around the injected client.
func New(cfg Config) *Adapter {
	probeTimeout := cfg.ProbeTimeout
	if probeTimeout <= 0 {
		probeTimeout = defaultProbeTimeout
	}
	visibilityTimeout := cfg.VisibilityTimeout
	if visibilityTimeout <= 0 {
		visibilityTimeout = defaultVisibilityTimeout
	}
	waitRunTimeout := cfg.WaitRunTimeout
	if waitRunTimeout <= 0 {
		waitRunTimeout = defaultWaitRunTimeout
	}
	endpoint := strings.TrimRight(strings.TrimSpace(cfg.Endpoint), "/")
	return &Adapter{
		client:            cfg.Client,
		endpoint:          endpoint,
		probeTimeout:      probeTimeout,
		visibilityTimeout: visibilityTimeout,
		waitRunTimeout:    waitRunTimeout,
	}
}

// Compile-time interface assertions: the adapter satisfies the base
// PluginAdapter plus exactly the optional capabilities it declares in Probe.
var (
	_ pluginbridge.PluginAdapter     = (*Adapter)(nil)
	_ pluginbridge.RunWaiter         = (*Adapter)(nil)
	_ pluginbridge.SessionLister     = (*Adapter)(nil)
	_ pluginbridge.SessionAttacher   = (*Adapter)(nil)
	_ pluginbridge.SessionStarter    = (*Adapter)(nil)
	_ pluginbridge.HistoryStreamer   = (*Adapter)(nil)
	_ pluginbridge.DetailReader      = (*Adapter)(nil)
	_ pluginbridge.SessionController = (*Adapter)(nil)
)

// ID returns the plugin id.
func (a *Adapter) ID() string { return "opencode" }

// Probe reports the adapter capability. Declared capabilities are kept
// strictly in sync with the interfaces implemented above: model switching is
// implemented (CanControlSession), permission.asked events are surfaced as
// approval panels but cannot be resolved remotely (CanApproval false), and
// there is no plugin-wide watcher, status reader or terminal support.
func (a *Adapter) Probe(ctx context.Context) (pluginbridge.Capability, error) {
	available, reason := a.probeServer(ctx)
	capability := pluginbridge.Capability{
		PluginID:                   a.ID(),
		Available:                  available,
		NativeVisibleInput:         available,
		NativeVisibleOutput:        available,
		CanAttachSession:           true,
		CanStartSessionWithMessage: true,
		CanOpenDraft:               true,
		CanListSessions:            true,
		CanReadHistory:             true,
		CanInterrupt:               true,
		CanApproval:                false,
		CanForwardSync:             available,
		CanReverseSync:             true,
		CanPluginWideWatch:         false,
		CanWaitRun:                 true,
		CanReadStatus:              false,
		CanControlSession:          true,
		IntegrationMode:            pluginbridge.IntegrationModeProtocolNative,
		VisibilitySurface:          Surface,
		UnavailableReason:          reason,
	}
	return capability, nil
}

// probeServer performs one live liveness check against the injected client.
func (a *Adapter) probeServer(ctx context.Context) (bool, string) {
	probeCtx, cancel := context.WithTimeout(ctx, a.probeTimeout)
	defer cancel()
	if _, err := a.client.ListSessions(probeCtx); err != nil {
		if errors.Is(err, opencode.ErrServerOffline) {
			return false, fmt.Sprintf("opencode server offline: no server reachable at %s", a.endpoint)
		}
		return false, err.Error()
	}
	return true, ""
}

// Discover reports the server endpoint and the current native session
// directory. An unreachable server is a discovery result with Verified=false,
// not an error, so Probe and Discover stay consistent.
func (a *Adapter) Discover(ctx context.Context) (pluginbridge.DiscoveryResult, error) {
	probeCtx, cancel := context.WithTimeout(ctx, a.probeTimeout)
	defer cancel()
	result := pluginbridge.DiscoveryResult{
		PluginID: a.ID(),
		Surface:  Surface,
		Endpoint: a.endpoint,
	}
	sessions, err := a.client.ListSessions(probeCtx)
	if err != nil {
		if errors.Is(err, opencode.ErrServerOffline) {
			result.Detail = fmt.Sprintf("opencode server offline: no server reachable at %s", a.endpoint)
		} else {
			result.Detail = "opencode server unreachable: " + err.Error()
		}
		return result, nil
	}
	hints := map[string]string{}
	for _, session := range sessions {
		if session.ID == "" {
			continue
		}
		hints[session.ID] = session.Title
	}
	result.SessionHints = hints
	result.Verified = true
	result.Detail = fmt.Sprintf("opencode server verified at %s (%d sessions)", a.endpoint, len(sessions))
	return result, nil
}

// Close releases adapter-owned resources. Every native call and stream is
// request-scoped (the StdioServer cancels their contexts), so there is
// nothing persistent to tear down here.
func (a *Adapter) Close(context.Context) error { return nil }
