package api

// SessionStatus represents the current state of a session.
type SessionStatus string

const (
	SessionRunning SessionStatus = "running"
	SessionExited  SessionStatus = "exited"
	SessionError   SessionStatus = "error"
)

// LessShellCreationOrder reports whether a precedes b in creation order: by
// CreatedAt, then by ID so the order is total and stable.
//
// This is the order a shell's channel index counted before the index was stored
// (see session.SnapshotShells), so it decides which shell a legacy manifest's
// termcp://#<session>:N resolves to. It lives on the shared type rather than in
// each package that sorts shells — storage sorts the persisted snapshot, session
// sorts its in-memory one — because the two must agree: a tie broken by directory
// or map order would let the same locator resolve to different channels.
func LessShellCreationOrder(a, b Session) bool {
	if a.CreatedAt == b.CreatedAt {
		return a.ID < b.ID
	}
	return a.CreatedAt < b.CreatedAt
}

// SessionMode represents the execution mode for one shell channel.
type SessionMode string

const (
	ModePTY  SessionMode = "pty"
	ModePipe SessionMode = "pipe"
)

// SessionKind names what carries a session's bytes.
//
// It is a container-level property, like SSHEndpoint, and is set only on the
// session record: a shell channel is a shell channel whatever the transport
// underneath, so ChildShell.Info() leaves it empty rather than claiming the
// channel is "a serial".
type SessionKind string

const (
	// KindInternal is the built-in loopback SSH server.
	KindInternal SessionKind = "internal"
	// KindRemote is an outbound SSH connection.
	KindRemote SessionKind = "remote"
	// KindSerial is a local serial device.
	KindSerial SessionKind = "serial"
)

// SerialInfo describes the device a serial session is attached to, for display.
//
// It lives here rather than beside the session implementation because it is
// session metadata that clients read: the Web UI prints "device @baud" on the
// session card, and session info reports it. It carries no credentials — a
// serial profile has none — which is why it is safe to put on a record that is
// listed, persisted and restored.
type SerialInfo struct {
	// Profile is the serial profile the session was started from, empty when
	// the profile was deleted after the session started.
	Profile string `json:"profile,omitempty"`
	Device  string `json:"device"`
	Baud    int    `json:"baud"`
}

// Session holds metadata for an interactive process session.
//
// Timestamps are Unix milliseconds, the one time representation this project
// stores and serves: log.jsonl marks, manifests, and every JSON API response
// use it. Formatting for display is the client's job (see the Web UI's
// fmtTime), so a value never has to be parsed back out of a string.
type Session struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Command string   `json:"command"`
	Args    []string `json:"args"`
	// Index is a shell's channel number inside its session: the N of
	// termcp://#<session>:N and the number in the shell-N tab label. It is
	// assigned when the channel is created and never reused or renumbered, so a
	// locator copied from the UI keeps naming the same shell after other channels
	// are closed. A session record is not a channel, so it carries no index.
	Index int `json:"index,omitempty"`
	// Mode is a per-shell property: each shell channel picks its own ("pty" for
	// an interactive terminal, "pipe" for a line-oriented run-to-exit command).
	// It is empty on session records — a session is a connection container and
	// owns no mode. The session_start mode is merely the mode of the first shell.
	Mode     SessionMode   `json:"mode,omitempty"`
	Status   SessionStatus `json:"status"` // running | exited | error
	ExitCode *int          `json:"exit_code"`
	// ExitReason is WHY the session left `running`, in the words the transcript
	// used: a transport that died under it, a transport that ended cleanly, or a
	// close the caller asked for. Empty means nothing recorded it.
	//
	// It is persisted with the manifest because the caller who needs it is asking
	// precisely when the session is no longer live: a restarted process restores the
	// session as DEAD, and "why" is the one fact it cannot reconstruct from anything
	// else on disk (issue #176). `get` prints it.
	ExitReason string `json:"exit_reason,omitempty"`
	CreatedAt  int64  `json:"created_at"` // Unix ms
	UpdatedAt  int64  `json:"updated_at"` // Unix ms
	Rows       int    `json:"rows"`
	Cols       int    `json:"cols"`
	// SSHConfig is the canonical, non-secret profile name used to create this session.
	// It is separate from Name because callers may rename a session after it starts.
	SSHConfig string `json:"ssh_config,omitempty"`
	// SSHEndpoint is a coarse hint for clients: "internal" (built-in loopback SSH) or "remote" (no host/user/port exposed).
	//
	// It is SSH-specific and empty for a serial session; prefer Kind, which is
	// meaningful for every transport. SSHEndpoint is kept because existing
	// clients read it, and because "internal" versus "remote" still says
	// something Kind does not.
	SSHEndpoint string `json:"ssh_endpoint,omitempty"`
	// Kind says what carries this session's bytes: "internal", "remote" or
	// "serial". It is omitted on shell records (see SessionKind).
	Kind SessionKind `json:"kind,omitempty"`
	// Display is how this session's output bytes are presented for display:
	// "auto", "text", "hex" or "b64", taken from the serial profile when the
	// session started. Empty means "text": bytes out, unaltered, the behaviour
	// every session had before the field existed. Like Kind it is omitted on
	// shell records.
	Display string `json:"display,omitempty"`
	// Charset is the character set the session's bytes are decoded from before
	// any presentation: "auto" (sniff each window), "utf-8", or a name the
	// encoding table maps (gbk, big5, …). It comes from the serial profile and
	// is empty for every other kind — an SSH session decodes as UTF-8 unless a
	// caller names a charset explicitly, which is the contract the UTF-8
	// refusal owes its callers.
	Charset string `json:"charset,omitempty"`
	// Serial names the device a serial session is attached to. Nil for every
	// other kind, which is what keeps the absence of a host unambiguous: a
	// session with no SSHEndpoint is serial only if Serial says so.
	Serial *SerialInfo `json:"serial,omitempty"`
	// ApprovalMode reports whether this session gates input behind N-person
	// approval. It is a session-level policy, not a per-shell one: turning it on
	// covers every shell channel in the session. The Web UI reads it to show that
	// typing is refused and to offer the approval composer instead.
	ApprovalMode bool `json:"approval_mode,omitempty"`
	// ApprovalNeed is the number of distinct approvers required while
	// ApprovalMode is on.
	ApprovalNeed int `json:"approval_need,omitempty"`
	// ApprovalScopes is what review holds: empty means every gated kind (the
	// default), otherwise the narrowed set (issue #75). Derived from live state
	// like the two fields above, so a manifest cannot claim a scope the session
	// does not enforce.
	ApprovalScopes []string `json:"approval_scopes,omitempty"`
	// Namespace marks a session as infrastructure rather than something a user
	// asked for: the extension layer keeps one session warm per host and
	// capability set, and those belong to the extension column rather than on the
	// session and archive cards (see internal/ns). Empty is the default and means
	// an ordinary session, so every session that predates namespaces stays listed
	// exactly as it was. It is persisted with the manifest because a restored
	// session must come back hidden: the tag is the only thing that tells the next
	// process this connection was not the user's.
	Namespace string `json:"namespace,omitempty"`

	// Shells is a per-shell metadata snapshot, populated only when the session is
	// persisted/restored so a DEAD session can still render its tabs after a
	// restart. Never set on a live running session's Info().
	Shells []Session `json:"shells,omitempty"`
}

// LogStatus classifies a span of a shell's log.bin.
//
// The set is open: adding a status only means introducing a new value, the
// on-disk schema does not change.
type LogStatus string

const (
	// LogOutput is bytes produced by the shell.
	LogOutput LogStatus = "o"
	// LogAIInput is bytes entered by an AI agent through MCP.
	LogAIInput LogStatus = "a"
	// LogAPIInput is bytes entered through the HTTP/WebSocket API (the human at
	// the browser).
	LogAPIInput LogStatus = "i"
	// LogApprovalRequest marks the point where an input was queued for approval
	// instead of being written. The bytes do not exist yet at this offset: the
	// span says "a decision was requested here", and the bytes that eventually
	// appear carry their own input status once an approver releases them.
	LogApprovalRequest LogStatus = "q"
	// LogApprovalGranted marks the point where an approved input was written.
	// A reader replaying the log can therefore tell reviewed input from input
	// that never passed a gate.
	LogApprovalGranted LogStatus = "A"
)

// LogMark is one line of log.jsonl: a transition to `Status` at byte `Offset`
// in the shell's log.bin.
//
// Marks carry no payload — log.bin is the only copy of the bytes. A mark only
// records where a span starts and what produced it. The span runs from its own
// Offset to the next mark's Offset (the last one runs to the end of the file),
// so there is no end field to keep in sync.
//
// Keys are single characters because every session appends these lines for its
// whole life; the file is an index, not a document.
type LogMark struct {
	Status LogStatus `json:"s"`
	Time   int64     `json:"t"` // Unix milliseconds; the moment the span started
	Offset int64     `json:"i"` // byte offset in log.bin where the span starts
}
