package session

import (
	"errors"
	"io"
	"time"

	"github.com/open-mcp-ai/termcp/internal/sshclient"
	"golang.org/x/crypto/ssh"
)

// ErrUnsupported reports that a channel does not implement an optional
// capability. Callers branch on it with errors.Is; the MCP layer maps it to a
// transport-specific error code rather than surfacing this text.
var ErrUnsupported = errors.New("operation not supported by this channel")

// ErrSerialSingleChannel reports an attempt to open a second shell channel on a
// serial session. A serial device carries one stream, so there is no second
// channel to open.
var ErrSerialSingleChannel = errors.New("a serial session has a single shell channel")

// shellChannel is the byte-stream seam between a ChildShell and whatever carries
// its bytes.
//
// It describes exactly one channel's stream. That scope is the point: the project
// used to have a TerminalShell interface that a *Session could satisfy, and it
// was deleted because a session standing in for a shell channel would reopen a
// second write path around the approval gate (see the ChildShell type comment).
// This interface stays one channel wide so that cannot come back.
//
// Serial implements this alongside sshclient.ExecSession. What the two share is
// only the channel's byte stream and end-of-life signal; anything SSH-specific
// (SSHClient) or optional (ResizePty) is deliberately absent and is probed as a
// separate capability.
type shellChannel interface {
	// WriteStdin writes bytes to the channel's input stream.
	WriteStdin([]byte) (int, error)

	// StdoutReader/StderrReader return the channel's output streams. A serial
	// channel has no stderr: it must return a non-nil reader that reports EOF
	// immediately, never nil (the pipe goroutines require a real reader).
	StdoutReader() io.Reader
	StderrReader() io.Reader

	// Done closes when the channel has ended for any reason.
	Done() <-chan struct{}

	// ExitCode returns the process exit code after Done closes. A serial channel
	// has no process and returns -1.
	ExitCode() int

	// Aborted reports whether the channel ended due to transport loss rather than
	// a clean exit. Serial returns true when the device disappears.
	Aborted() bool

	// CloseReaders releases the read sides so a goroutine stuck on a transport
	// that never reports EOF can be unblocked.
	CloseReaders()

	// CloseSessionOnly closes this channel without tearing down a shared
	// connection.
	CloseSessionOnly() error

	// Close releases the channel and any resources it owns.
	Close() error
}

// ptyResizer is an optional capability, probed by type assertion at the call
// site. An SSH channel satisfies it; a serial channel does not, and a resize
// request against one reports ErrUnsupported rather than silently doing nothing.
type ptyResizer interface {
	ResizePty(rows, cols int) error
}

// transport carries the lifecycle capabilities EVERY session needs, whatever
// carries its bytes. A Session's own execSession field is concrete and SSH-only;
// this is the seam that lets a session with no SSH at all still be closed,
// waited on, and marked dead through the same code.
//
// It deliberately does not carry Signal: signalling a process is an SSH concept
// a serial device cannot honour, so it is probed separately (see signaler).
type transport interface {
	// Close releases the transport and everything it owns.
	//
	// Implementations MUST be idempotent: Disconnect, finalize, and the root
	// shell's TerminateShell can each reach the transport, and a serial port is
	// not safe to close twice.
	Close() error

	// CloseSessionOnly closes this session's channel without tearing down a
	// shared connection. For a transport with no multiplexing (serial) it is
	// equivalent to Close.
	CloseSessionOnly() error

	// Done closes when the transport is finished: process exit, port close, read
	// failure, or device loss.
	Done() <-chan struct{}
}

// signaler is an optional capability, probed by type assertion like ptyResizer.
// Only the SSH transport implements it. When it is absent, Terminate skips the
// grace period and closes directly — a serial device cannot be asked politely
// to stop.
type signaler interface {
	Signal(sig ssh.Signal) error
}

// transportError is the optional way a transport explains WHY it ended. The SSH
// ExecSession reports it through WaitTransport, which blocks until the
// transport is down; a serial channel keeps the last read error, which the
// watcher reads after Done closes. The watcher adapts to whichever is present.
type transportError interface {
	LastError() error
}

// serialController is an optional capability: the modem-line and buffer
// operations a serial device offers.
//
// Like ptyResizer and signaler, it is probed rather than declared on the
// channel interface, so an SSH channel never has to implement methods that mean
// nothing for it. The MCP `set` directive reaches it through ChildShell.Controller.
type serialController interface {
	SetDTR(on bool) error
	SetRTS(on bool) error
	Break(d time.Duration) error
	FlushInput() error
	FlushOutput() error
	ModemStatus() (CTS, DSR, RI, DCD bool, err error)
}

// The SSH transport satisfies every seam. These assertions live in the session
// package (not sshclient) so a change to any interface breaks the build here,
// where the interfaces are declared, rather than in a far-away call site.
var (
	_ shellChannel = (*sshclient.ExecSession)(nil)
	_ transport    = (*sshclient.ExecSession)(nil)
	_ ptyResizer   = (*sshclient.ExecSession)(nil)
	_ signaler     = (*sshclient.ExecSession)(nil)
)
