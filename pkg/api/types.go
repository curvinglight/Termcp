package api

// SessionStatus represents the current state of a session.
type SessionStatus string

const (
	SessionRunning SessionStatus = "running"
	SessionExited  SessionStatus = "exited"
	SessionError   SessionStatus = "error"
)

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
// session card, and session_info reports it. It carries no credentials — a
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
	// Mode is a per-shell property: each shell channel picks its own ("pty" for
	// an interactive terminal, "pipe" for a line-oriented run-to-exit command).
	// It is empty on session records — a session is a connection container and
	// owns no mode. The session_start mode is merely the mode of the first shell.
	Mode      SessionMode   `json:"mode,omitempty"`
	Status    SessionStatus `json:"status"` // running | exited | error
	ExitCode  *int          `json:"exit_code"`
	PID       int           `json:"pid"`
	CreatedAt int64         `json:"created_at"` // Unix ms
	UpdatedAt int64         `json:"updated_at"` // Unix ms
	Rows      int           `json:"rows"`
	Cols      int           `json:"cols"`
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
	// Encoding is how this session's output bytes are presented for display:
	// "auto", "text", "hex" or "b64", taken from the serial profile when the
	// session started.
	//
	// It is a presentation property, not a storage one — the transcript keeps
	// raw bytes whichever value is set — and it is session-level because the
	// profile it comes from is. Empty means no profile chose one, which the
	// readers treat as "text": bytes out, unaltered, the behaviour every
	// session had before the field existed. It is carried on the manifest so a
	// replayed transcript is presented the way it was captured, across a
	// restart. Like Kind it is omitted on shell records.
	Encoding string `json:"encoding,omitempty"`
	// Serial names the device a serial session is attached to. Nil for every
	// other kind, which is what keeps the absence of a host unambiguous: a
	// session with no SSHEndpoint is serial only if Serial says so.
	//
	// It is set from the profile at creation and never changes, so a
	// restart-restored session still knows what it was talking to.
	Serial *SerialInfo `json:"serial,omitempty"`
	// ApprovalMode reports whether this session gates input behind N-person
	// approval. It is a session-level policy, not a per-shell one: turning it on
	// covers every shell channel in the session. The Web UI reads it to show that
	// typing is refused and to offer the approval composer instead.
	ApprovalMode bool `json:"approval_mode,omitempty"`
	// ApprovalNeed is the number of distinct approvers required while
	// ApprovalMode is on.
	ApprovalNeed int `json:"approval_need,omitempty"`

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
