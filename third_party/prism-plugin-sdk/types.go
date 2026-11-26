// Package pluginbridge is the official Go SDK for PluginBridge plugins.
//
// A plugin implements the PluginAdapter interface and registers it with Serve.
// The SDK handles the JSON-over-stdio protocol (request dispatch, response
// encoding, event forwarding) so the plugin author only writes native-product-specific
// logic.
//
// JSON field names are fixed explicitly to lock the on-wire protocol. They must
// stay in sync with the Prism-side runtime types (internal/pluginruntime/types.go) and the
// cross-language SDKs (Node/Python). Changing a Go field name without updating
// its json tag would silently break the Hub<->plugin contract.
package pluginbridge

import (
	"context"
	"time"
)

// ProtocolVersion is the PluginBridge plugin protocol version this SDK speaks.
// The Hub passes it to the plugin via the PRISM_PLUGIN_PROTOCOL_VERSION
// environment variable. Plugins should check it on startup.
const ProtocolVersion = "4"

// PluginAdapter is the raw protocol surface every plugin speaks over stdio.
// Finished mobile/glasses remote-conversation plugins should satisfy the
// stricter RemoteConversationAdapter contract below.
type PluginAdapter interface {
	ID() string
	Probe(ctx context.Context) (Capability, error)
	Discover(ctx context.Context) (DiscoveryResult, error)
	Send(ctx context.Context, session NativeSession, msg InboundMessage) (SendReceipt, error)
	Subscribe(ctx context.Context, session NativeSession) (<-chan PluginEvent, error)
	Interrupt(ctx context.Context, session NativeSession, taskID string) error
	VerifyVisibility(ctx context.Context, session NativeSession, marker string) (VisibilityResult, error)
	Close(ctx context.Context) error
}

// AgentUsageReader optionally exposes an in-memory account usage projection
// for an Agent card. It must not persist the raw account response.
type AgentUsageReader interface {
	ReadAgentUsage(ctx context.Context) (*AgentUsage, error)
}

// PluginWideSubscriber observes one Plugin-wide native event source. It is
// optional because protocol-native plugins may expose real per-session
// subscriptions instead. Desktop automation plugins commonly use this single
// foreground watcher rather than duplicating a watcher for every session.
type PluginWideSubscriber interface {
	SubscribePlugin(ctx context.Context) (<-chan PluginEvent, error)
}

type AgentUsage struct {
	FiveHour       *AgentUsageFiveHour  `json:"five_hour,omitempty"`
	Weekly         *AgentUsageWeekly    `json:"weekly,omitempty"`
	AccountSummary *AccountUsageSummary `json:"account_summary,omitempty"`
	UpdatedAt      int64                `json:"updated_at,omitempty"`
}

type AgentUsageFiveHour struct {
	UsedPercent   int64 `json:"used_percent"`
	WindowMinutes int64 `json:"window_minutes"`
	ResetsAt      int64 `json:"resets_at"`
}

type AgentUsageWeekly struct {
	UsedPercent   int64 `json:"used_percent"`
	WindowMinutes int64 `json:"window_minutes"`
	ResetsAt      int64 `json:"resets_at"`
}

type AccountUsageSummary struct {
	LifetimeTokens    int64 `json:"lifetime_tokens"`
	PeakDailyTokens   int64 `json:"peak_daily_tokens"`
	CurrentStreakDays int64 `json:"current_streak_days"`
	LongestStreakDays int64 `json:"longest_streak_days"`
}

// RunWaiter is optional only at the raw protocol layer. For any plugin that is
// meant to power mobile/glasses remote conversations, this is effectively
// required: without waitForRun, Prism cannot return the final body/terminal
// state for one turn.
type RunWaiter interface {
	WaitForRun(ctx context.Context, session NativeSession, runID string) (PluginEvent, error)
}

// SessionLister is optional only for partial/local-only plugins. Any plugin
// that claims full remote-conversation support for mobile/glasses must
// implement it so Prism can show the user's existing sessions.
type SessionLister interface {
	ListSessions(ctx context.Context) ([]NativeSessionHint, error)
}

// SessionAttacher is optional only for partial/local-only plugins. Any plugin
// that claims full remote-conversation support for mobile/glasses must
// implement it so Prism can continue a chosen native session.
type SessionAttacher interface {
	AttachSession(ctx context.Context, req AttachSessionRequest) (NativeSession, error)
}

// SessionStarter starts a new native session by delivering the first user
// message. A successful result must identify the real native session and
// confirm the message is visible there.
type SessionStarter interface {
	StartSessionWithMessage(ctx context.Context, req StartSessionWithMessageRequest) (StartSessionWithMessageResult, error)
}

type DraftOpener interface {
	OpenDraft(ctx context.Context, req DraftOpenRequest) (DraftOpenResult, error)
}

type DraftController interface {
	ControlDraft(ctx context.Context, req DraftControlRequest) (DraftControlResult, error)
}

type DraftStarter interface {
	StartDraftWithMessage(ctx context.Context, req StartDraftWithMessageRequest) (StartSessionWithMessageResult, error)
}

// HistoryStreamRequest selects the fixed latest-turn body window.
type HistoryStreamRequest struct {
	StreamID string `json:"stream_id"`
	Limit    int    `json:"limit"`
	Live     bool   `json:"live,omitempty"`
}

type HistoryTurn struct {
	TurnID   string           `json:"turn_id"`
	OrderKey string           `json:"order_key"`
	Revision int64            `json:"revision"`
	Messages []HistoryMessage `json:"messages"`
}

type HistoryStreamEvent struct {
	StreamID  string       `json:"stream_id"`
	Type      string       `json:"type"`
	Source    string       `json:"source,omitempty"`
	Operation string       `json:"operation,omitempty"`
	Turn      *HistoryTurn `json:"turn,omitempty"`
	Error     string       `json:"error,omitempty"`
	Retryable bool         `json:"retryable,omitempty"`
}

// HistoryStreamer is required for full remote-conversation plugins.
type HistoryStreamer interface {
	ReadHistoryStream(ctx context.Context, session NativeSession, req HistoryStreamRequest) (<-chan HistoryStreamEvent, error)
}

// DetailReader returns the current canonical detail snapshot without selecting
// or taking ownership of the native conversation.
type DetailReader interface {
	ReadDetail(ctx context.Context, session NativeSession) (map[string]any, error)
}

// HistoryReader is retained only for a pre-stream API compatibility route.
// New remote body implementations must use HistoryStreamer.
type HistoryReader interface {
	ReadHistory(ctx context.Context, session NativeSession, limit int) ([]HistoryMessage, error)
}

type ApprovalResolver interface {
	ResolveApproval(ctx context.Context, req ApprovalResolutionRequest) error
}

// StatusReader exposes the run-process view of a session (phase, steps,
// preview, context). It complements detail_snapshot, which only carries the
// control-console view (model/options/approval). See docs/06 §5.4.
type StatusReader interface {
	ReadStatus(ctx context.Context, session NativeSession, runID string) (RunStatus, error)
}

// SessionController executes a session-level control action (switch model /
// reasoning / permission, compact, rename, pin, archive, ...) and returns the
// refreshed detail. See docs/06 §5.5. This is the writable half of the unified
// capability contract; detail_snapshot is the readable half.
type SessionController interface {
	ControlSession(ctx context.Context, req ControlSessionRequest) (ControlSessionResult, error)
}

// ManagedTerminalLauncher opens a local TUI backed by the Plugin's existing
// native transport. It is a Desktop-only action, never a remote client API.
type ManagedTerminalLauncher interface {
	OpenManagedTerminal(ctx context.Context, req ManagedTerminalRequest) (ManagedTerminalResult, error)
}

// RemoteConversationAdapter describes a finished remote-conversation plugin for
// mobile/glasses style clients. It is stricter than the raw protocol surface:
// besides the base PluginAdapter, it must support listing sessions, attaching to
// an existing session, and waiting for the terminal result of one run.
//
// Approval handling is also required at the product level whenever the native
// application has approval semantics; it is represented by ApprovalResolver.
type RemoteConversationAdapter interface {
	PluginAdapter
	RunWaiter
	SessionLister
	SessionAttacher
	SessionStarter
}

type IntegrationMode string

const (
	IntegrationModeProtocolNative    IntegrationMode = "protocol-native"
	IntegrationModeDesktopAutomation IntegrationMode = "desktop-automation"
)

type Diagnostic struct {
	State           string            `json:"State,omitempty"`
	Code            string            `json:"Code,omitempty"`
	Summary         string            `json:"Summary,omitempty"`
	Detail          string            `json:"Detail,omitempty"`
	Component       string            `json:"Component,omitempty"`
	ActualVersion   string            `json:"ActualVersion,omitempty"`
	RequiredVersion string            `json:"RequiredVersion,omitempty"`
	Actions         []string          `json:"Actions,omitempty"`
	Facts           map[string]any    `json:"Facts,omitempty"`
	Stack           string            `json:"Stack,omitempty"`
	Metadata        map[string]string `json:"Metadata,omitempty"`
}

type Capability struct {
	PluginID                   string `json:"PluginID"`
	Available                  bool   `json:"Available"`
	NativeVisibleInput         bool   `json:"NativeVisibleInput"`
	NativeVisibleOutput        bool   `json:"NativeVisibleOutput"`
	CanAttachSession           bool   `json:"CanAttachSession"`
	CanStartSessionWithMessage bool   `json:"CanStartSessionWithMessage"`
	CanOpenDraft               bool   `json:"CanOpenDraft"`
	// CanListSessions declares the plugin can enumerate real native sessions
	// that a remote client may choose and continue.
	CanListSessions bool `json:"CanListSessions"`
	// CanReadHistory declares the plugin can read native conversation bodies /
	// history for a chosen session.
	CanReadHistory bool `json:"CanReadHistory"`
	CanInterrupt   bool `json:"CanInterrupt"`
	CanApproval    bool `json:"CanApproval"`
	// CanForwardSync declares the forward path (Prism -> native application): the plugin can
	// accept an inbound Prism message, drive the native application, and project run
	// progress back. Nearly every plugin sets this true.
	CanForwardSync bool `json:"CanForwardSync"`
	// CanReverseSync declares the reverse path (native application -> Prism): the plugin can
	// surface spontaneous native-application activity back into Prism. This is useful,
	// but not the hard gate for a full remote-conversation plugin.
	CanReverseSync bool `json:"CanReverseSync"`
	// CanPluginWideWatch declares that reverse sync is one Plugin-wide watcher,
	// not an independent subscription for each native session.
	CanPluginWideWatch bool `json:"CanPluginWideWatch"`
	// CanWaitRun declares that the adapter implements RunWaiter (the
	// adapter.waitForRun method). The Hub checks this flag before calling
	// waitForRun; the interface assertion is only a second-line guard.
	CanWaitRun bool `json:"CanWaitRun"`
	// CanReadStatus declares the plugin implements StatusReader (the
	// adapter.readStatus method), exposing the run-process view (phase/steps/
	// preview/context) that complements detail_snapshot. See docs/06 §5.4.
	CanReadStatus bool `json:"CanReadStatus"`
	// CanControlSession declares the plugin implements SessionController (the
	// adapter.controlSession method), the writable half of the unified
	// capability contract. See docs/06 §5.5.
	CanControlSession bool `json:"CanControlSession"`
	// CanOpenManagedTerminal declares a local TUI can share the Plugin-owned
	// native transport.
	CanOpenManagedTerminal bool `json:"CanOpenManagedTerminal"`
	// IntegrationMode declares whether the plugin talks to the native application through a
	// formal protocol or through desktop automation.
	IntegrationMode   IntegrationMode `json:"IntegrationMode"`
	VisibilitySurface string          `json:"VisibilitySurface"`
	UnavailableReason string          `json:"UnavailableReason"`

	Diagnostics *Diagnostic `json:"Diagnostics,omitempty"`
}

type DiscoveryResult struct {
	PluginID     string            `json:"PluginID"`
	Surface      string            `json:"Surface"`
	Endpoint     string            `json:"Endpoint"`
	ProcessID    int               `json:"ProcessID"`
	SessionHints map[string]string `json:"SessionHints"`
	Verified     bool              `json:"Verified"`
	Detail       string            `json:"Detail"`
}

type StartSessionWithMessageRequest struct {
	PluginID     string            `json:"PluginID"`
	Cwd          string            `json:"Cwd"`
	Message      InboundMessage    `json:"Message"`
	SourceDevice string            `json:"SourceDevice"`
	Metadata     map[string]string `json:"Metadata"`
}

type StartSessionWithMessageResult struct {
	Session    NativeSession    `json:"Session"`
	Receipt    SendReceipt      `json:"Receipt"`
	Visibility VisibilityResult `json:"Visibility"`
}

type DraftOpenRequest struct {
	DraftID      string            `json:"DraftID"`
	PluginID     string            `json:"PluginID"`
	Cwd          string            `json:"Cwd"`
	SourceDevice string            `json:"SourceDevice"`
	Metadata     map[string]string `json:"Metadata"`
}

type DraftOpenResult struct {
	DraftID          string         `json:"DraftID"`
	Cwd              string         `json:"Cwd"`
	Controls         map[string]any `json:"Controls"`
	DraftFingerprint string         `json:"draft_fingerprint"`
}

type DraftControlRequest struct {
	DraftID  string         `json:"DraftID"`
	PluginID string         `json:"PluginID"`
	Action   string         `json:"Action"`
	Target   any            `json:"Target,omitempty"`
	Name     string         `json:"Name,omitempty"`
	Metadata map[string]any `json:"Metadata,omitempty"`
}

type DraftControlResult struct {
	DraftID  string         `json:"DraftID"`
	Cwd      string         `json:"Cwd"`
	Controls map[string]any `json:"Controls"`
}

type StartDraftWithMessageRequest struct {
	DraftID      string            `json:"DraftID"`
	PluginID     string            `json:"PluginID"`
	Cwd          string            `json:"Cwd"`
	Message      InboundMessage    `json:"Message"`
	SourceDevice string            `json:"SourceDevice"`
	Metadata     map[string]string `json:"Metadata"`
}

type AttachSessionRequest struct {
	PrismConversationID string            `json:"PrismConversationID"`
	PluginID            string            `json:"PluginID"`
	NativeSessionID     string            `json:"NativeSessionID"`
	NativeThreadID      string            `json:"NativeThreadID"`
	Cwd                 string            `json:"Cwd"`
	SourceDevice        string            `json:"SourceDevice"`
	Metadata            map[string]string `json:"Metadata"`
}

type ApprovalResolutionRequest struct {
	PrismConversationID string            `json:"PrismConversationID"`
	PluginID            string            `json:"PluginID"`
	Session             NativeSession     `json:"Session"`
	ApprovalRequestID   string            `json:"ApprovalRequestID"`
	ActionID            string            `json:"ActionID"`
	Input               string            `json:"Input,omitempty"`
	SourceDevice        string            `json:"SourceDevice"`
	Metadata            map[string]string `json:"Metadata"`
}

// ControlSessionRequest carries one session-level control action. Action is
// case-insensitive and accepts both "model.switch" and "model_switch" forms.
// See docs/06 §5.5 for the standard action-key table.
type ControlSessionRequest struct {
	PrismConversationID string         `json:"-"`
	PluginID            string         `json:"-"`
	Session             NativeSession  `json:"session"`
	Action              string         `json:"action"`
	Target              any            `json:"target,omitempty"`
	Name                string         `json:"name,omitempty"`
	SourceDevice        string         `json:"-"`
	Metadata            map[string]any `json:"metadata,omitempty"`
}

// ControlSessionResult is the refreshed detail after a control action. Details
// carries the full detail_snapshot (same shape as a desktop.state.changed event
// payload). DetailsConfirmed is true only for an authoritative native RPC
// result that Hub may publish immediately; mutations set it only after a
// successful write. Asynchronous UI controls still converge through subscribe.
type ControlSessionResult struct {
	OK               bool           `json:"ok"`
	Action           string         `json:"action,omitempty"`
	ThreadID         string         `json:"thread_id,omitempty"`
	ConversationID   string         `json:"-"`
	Message          string         `json:"message,omitempty"`
	Details          map[string]any `json:"details,omitempty"`
	DetailsConfirmed bool           `json:"details_confirmed,omitempty"`
}

type ManagedTerminalRequest struct {
	PluginID        string `json:"plugin_id"`
	Cwd             string `json:"cwd,omitempty"`
	NativeSessionID string `json:"native_session_id,omitempty"`
	NativeThreadID  string `json:"native_thread_id,omitempty"`
}

type ManagedTerminalResult struct {
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
}

// RunStatus is the run-process view returned by ReadStatus. It carries fields
// that detail_snapshot deliberately omits (phase, steps, preview). See docs/06
// §5.4. Fields not applicable to a plugin may be zero-valued. Wire fields are
// snake_case to match internal/pluginruntime.RuntimeStatus.
type RunStatus struct {
	Status          string           `json:"status,omitempty"`
	Phase           map[string]any   `json:"phase,omitempty"`
	Preview         string           `json:"preview,omitempty"`
	Steps           []map[string]any `json:"steps,omitempty"`
	Context         map[string]any   `json:"context,omitempty"`
	Model           any              `json:"model,omitempty"`
	ReasoningMode   any              `json:"reasoning_mode,omitempty"`
	Interruptible   bool             `json:"interruptible,omitempty"`
	PrimaryAction   string           `json:"primary_action,omitempty"`
	ApprovalBlocked bool             `json:"approval_blocked,omitempty"`
	TurnID          string           `json:"turn_id,omitempty"`
	StartedAt       string           `json:"started_at,omitempty"`
	CompletedAt     string           `json:"completed_at,omitempty"`
	DurationMS      int64            `json:"duration_ms,omitempty"`
}

type NativeSession struct {
	PluginID        string `json:"PluginID"`
	NativeSessionID string `json:"NativeSessionID"`
	NativeThreadID  string `json:"NativeThreadID"`
	Surface         string `json:"Surface"`
	Endpoint        string `json:"Endpoint"`
	Cwd             string `json:"Cwd"`
	Visible         bool   `json:"Visible"`
}

type NativeSessionHint struct {
	PluginID            string            `json:"PluginID"`
	NativeSessionID     string            `json:"NativeSessionID"`
	NativeThreadID      string            `json:"NativeThreadID"`
	Surface             string            `json:"Surface"`
	Endpoint            string            `json:"Endpoint"`
	Cwd                 string            `json:"Cwd"`
	Title               string            `json:"Title"`
	PrismConversationID string            `json:"PrismConversationID"`
	Active              bool              `json:"Active"`
	Visible             bool              `json:"Visible"`
	LastActivityAt      time.Time         `json:"LastActivityAt"`
	Metadata            map[string]string `json:"Metadata"`
}

type InboundMessage struct {
	PrismMessageID string            `json:"PrismMessageID"`
	Text           string            `json:"Text"`
	Attachments    []Attachment      `json:"Attachments,omitempty"`
	SourceDevice   string            `json:"SourceDevice"`
	Timestamp      time.Time         `json:"Timestamp"`
	Metadata       map[string]string `json:"Metadata"`
}

// Attachment is a command-only local input. Hub materializes it before the
// Plugin RPC; SDK adapters must never expose it in history or Plugin events.
type Attachment struct {
	Name      string `json:"Name"`
	MIMEType  string `json:"MIMEType"`
	LocalPath string `json:"LocalPath"`
}

type SendReceipt struct {
	NativeMessageID          string `json:"NativeMessageID"`
	CanonicalNativeSessionID string `json:"CanonicalNativeSessionID,omitempty"`
	CanonicalNativeThreadID  string `json:"CanonicalNativeThreadID,omitempty"`
	Accepted                 bool   `json:"Accepted"`
	Visible                  bool   `json:"Visible"`
	PendingConfirmation      bool   `json:"PendingConfirmation,omitempty"`
	QueuePending             bool   `json:"QueuePending,omitempty"`
	Detail                   string `json:"Detail"`
}

type VisibilityResult struct {
	Visible       bool      `json:"Visible"`
	Marker        string    `json:"Marker"`
	Evidence      string    `json:"Evidence"`
	CheckedAt     time.Time `json:"CheckedAt"`
	FailureReason string    `json:"FailureReason"`
}

type PluginEvent struct {
	ID      string `json:"ID"`
	Type    string `json:"Type"`
	Status  string `json:"Status"`
	Summary string `json:"Summary"`
	// Attachment-bearing events should prefer lightweight metadata in Payload,
	// for example attachments[].name. Remote clients currently require file-name
	// visibility, not binary image sync.
	Payload   map[string]any `json:"Payload"`
	CreatedAt time.Time      `json:"CreatedAt"`
}

type HistoryMessage struct {
	ID        string    `json:"ID"`
	Role      string    `json:"Role"`
	Type      string    `json:"Type"`
	Content   string    `json:"Content"`
	Status    string    `json:"Status"`
	CreatedAt time.Time `json:"CreatedAt"`
	UpdatedAt time.Time `json:"UpdatedAt"`
	// Progress is the structured assistant execution unit for this message.
	// It is part of the body wire contract, not opaque metadata.
	Progress *HistoryProgress `json:"Progress,omitempty"`
	// Attachments carry only attachments[].name, mime_type and kind. Remote
	// clients never receive a local path, URI, preview URL or image binary.
	Metadata map[string]any `json:"Metadata,omitempty"`
}

type HistoryProgress struct {
	Status      string                `json:"Status"`
	StartedAt   time.Time             `json:"StartedAt"`
	CompletedAt time.Time             `json:"CompletedAt,omitempty"`
	Steps       []HistoryProgressStep `json:"Steps,omitempty"`
}

type HistoryProgressStep struct {
	ID        string    `json:"ID"`
	Kind      string    `json:"Kind"`
	CallID    string    `json:"CallID,omitempty"`
	Title     string    `json:"Title"`
	Detail    string    `json:"Detail"`
	Status    string    `json:"Status"`
	CreatedAt time.Time `json:"CreatedAt"`
}

// PluginStatus is a local-only status view (not part of the wire protocol).
type PluginStatus struct {
	ID         string     `json:"id"`
	Available  bool       `json:"available"`
	Surface    string     `json:"surface,omitempty"`
	Endpoint   string     `json:"endpoint,omitempty"`
	Visible    bool       `json:"visible"`
	LastError  string     `json:"last_error,omitempty"`
	Capability Capability `json:"-"`
}
