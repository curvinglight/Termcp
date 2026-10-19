package session

import (
	"bytes"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/open-mcp-ai/termcp/internal/buffer"
	"github.com/open-mcp-ai/termcp/internal/clock"
	"github.com/open-mcp-ai/termcp/internal/sshclient"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// Lock ordering: shellStateMu -> stdinMu. Never acquire in reverse order.
// shellStateMu is only taken by session-level transitions (close/DEAD/terminate,
// new shells), and stdinMu serializes writes only. Per-shell state that changes
// after creation is atomic, so there is no per-shell lock to order: a reader
// never blocks the exit watcher (see the field docs below).

// ChildShell is a lightweight shell channel sharing the parent Session's SSH connection.
//
// It is the only terminal the API surface addresses: a session is a connection
// container, and every read or write of terminal bytes goes to one of its shell
// channels. The dead TerminalShell interface used to suggest a *Session could
// stand in for one; nothing ever did that, and widening it back would reopen a
// second write path around the approval gate.
type ChildShell struct {
	ID string
	// Index is this channel's 1-based number inside the parent session (1 = the
	// first shell). It is assigned once at creation and never renumbered, so
	// termcp://#<session>:<index> keeps naming the same channel after other
	// channels are closed. 0 means "not assigned" (a shell built directly in a
	// test, or one restored from a manifest written before Index existed).
	Index       int
	Name        string
	parent      atomic.Pointer[Session] // nil if not yet attached to a session
	execSession *sshclient.ExecSession
	buf         *buffer.Buffer
	done        chan struct{}
	closeOnce   sync.Once
	cleanupOnce sync.Once // guards explicit removal from the parent shell map
	stdinMu     sync.Mutex
	// status is stored through atomic.Value so the zero value stays "" (a shell
	// built directly in a test), matching the old plain field. Load returns nil
	// until the constructor publishes running.
	status    atomic.Value // api.SessionStatus
	CreatedAt int64        // Unix ms
	exitCode  atomic.Pointer[int]
	rows      atomic.Int32
	cols      atomic.Int32
	// enterCRLF selects the byte sequence for pipe-mode enter.
	// It reflects the target shell family (unix vs cmd/powershell), not the
	// termcp host OS, so cross-OS SSH sessions send the right line ending.
	enterCRLF bool
	mode      api.SessionMode // pty or pipe; affects press_key("enter")
	// pipeWG tracks this shell's stdout/stderr reader goroutines so the final
	// bytes can be drained into the buffer before it is sealed. See drainPipes.
	pipeWG sync.WaitGroup
	// deliberateClose is set by TerminateShell/CloseChildShell so the exit
	// watcher does not treat an intentional channel close as SSH disconnect.
	deliberateClose atomic.Bool
	// closed marks a shell the user explicitly closed (CloseChildShell). A
	// closed shell is a DELETE, not a DEAD transition: it is removed from the
	// live map AND the per-shell history snapshot. Atomic so the exit watcher
	// and TerminateShell can check it without extra locking; see
	// CloseChildShell and the watcher's store-then-recheck in startReaders.
	closed atomic.Bool
}

// statusValue reads the lifecycle status. The zero ChildShell reports "", the
// same value the old field held before a constructor ran.
func (cs *ChildShell) statusValue() api.SessionStatus {
	if v := cs.status.Load(); v != nil {
		return v.(api.SessionStatus)
	}
	return ""
}

// setStatus publishes a lifecycle transition.
func (cs *ChildShell) setStatus(st api.SessionStatus) { cs.status.Store(st) }

// exitCodeValue returns a copy of the process exit code, or nil while running.
func (cs *ChildShell) exitCodeValue() *int {
	p := cs.exitCode.Load()
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// setExitCode publishes the process exit code.
func (cs *ChildShell) setExitCode(code int) { cs.exitCode.Store(&code) }

// ParentSessionID returns the parent Session ID for this child shell.
func (cs *ChildShell) ParentSessionID() string {
	if p := cs.parent.Load(); p != nil {
		return p.ID
	}
	return ""
}

// Info returns a snapshot of the child shell's public metadata.
func (cs *ChildShell) Info() api.Session {
	s := api.Session{
		ID:        cs.ID,
		Index:     cs.Index,
		Name:      cs.Name,
		Mode:      cs.mode,
		Status:    cs.statusValue(),
		Rows:      int(cs.rows.Load()),
		Cols:      int(cs.cols.Load()),
		CreatedAt: cs.CreatedAt,
		UpdatedAt: clock.Now(),
	}
	s.ExitCode = cs.exitCodeValue()
	return s
}

// Done returns a channel that closes once the shell is finalized: the process
// has exited, its output is drained and sealed, and Status/ExitCode are
// published. A waiter that wakes on it may read the final transcript and exit
// code without racing the exit watcher.
func (cs *ChildShell) Done() <-chan struct{} {
	return cs.done
}

// SendTerminalBytes writes raw keystrokes to the child shell's stdin.
// pressEnter is kept for WebUI NL flag; MCP should use PressKey instead.
func (cs *ChildShell) SendTerminalBytes(data []byte, pressEnter bool) error {
	return cs.SendTerminalBytesFrom(data, pressEnter, InputFromAPI)
}

// SendTerminalBytesFrom writes raw keystrokes and records the bytes in the
// shell's log with the status of the given source.
//
// The bytes are logged whether or not the remote terminal echoes them: the log
// records what was sent, which is the only way to explain a transcript where the
// screen does not show the input (password prompts, full-screen programs).
func (cs *ChildShell) SendTerminalBytesFrom(data []byte, pressEnter bool, src InputSource) error {
	status := cs.statusValue()
	if status != api.SessionRunning {
		return fmt.Errorf("process has %s, cannot send input", status)
	}
	var toWrite []byte
	if pressEnter {
		toWrite = appendEnter(data, cs.enterCRLF)
	} else {
		toWrite = data
	}
	// Whether this input submits the line. Reported rather than inferred from the
	// output: the terminal's byte stream does not delimit lines (a redraw emits a
	// line break and a cursor-move sequence without ending anything), so a
	// consumer watching newlines sees a line end when the terminal only repainted.
	// The input side knows the truth — the user pressed enter.
	submit := submitsLine(toWrite)
	// Record the input *before* the bytes go out, and only when it submits the
	// line: the status display is live feedback that must not wait on the write,
	// and a keystroke that leaves the line open is not a span of the transcript.
	// See logInput for why a partial line writes nothing.
	cs.logInput(src, submit)
	cs.stdinMu.Lock()
	_, err := cs.execSession.WriteStdin(toWrite)
	cs.stdinMu.Unlock()
	if err != nil {
		return err
	}
	return nil
}

// PressKey writes a named key sequence (enter, ctrl+c, arrows, …) repeat times.
func (cs *ChildShell) PressKey(key string, repeat int) error {
	return cs.PressKeyFrom(key, repeat, InputFromAPI)
}

// PressKeyFrom writes a named key sequence and records it with the status of the
// given source.
func (cs *ChildShell) PressKeyFrom(key string, repeat int, src InputSource) error {
	if repeat < 1 {
		repeat = 1
	}
	if repeat > 20 {
		return fmt.Errorf("repeat must be between 1 and 20, got %d", repeat)
	}
	seq, err := KeyBytes(key, cs.mode == api.ModePTY, cs.enterCRLF)
	if err != nil {
		return err
	}
	status := cs.statusValue()
	if status != api.SessionRunning {
		return fmt.Errorf("process has %s, cannot send input", status)
	}
	payload := bytes.Repeat(seq, repeat)
	// A named key submits a line too (enter, and the ctrl sequences a line editor
	// treats as one), so the signal comes from the bytes actually sent rather than
	// from the key's name: the name is a label, the sequence is what the terminal
	// receives.
	submit := submitsLine(payload)
	cs.logInput(src, submit)
	cs.stdinMu.Lock()
	_, err = cs.execSession.WriteStdin(payload)
	cs.stdinMu.Unlock()
	if err != nil {
		return err
	}
	return nil
}

// ResizePty adjusts the child shell's terminal dimensions. Only a pty shell has
// a terminal: a pipe shell has no TTY, so this reports an error instead of
// forwarding a meaningless window-change to the transport.
func (cs *ChildShell) ResizePty(rows, cols int) error {
	if cs.mode != api.ModePTY {
		return fmt.Errorf("PTY resize only available in pty mode (shell mode %q)", cs.mode)
	}
	if cs.statusValue() != api.SessionRunning {
		return fmt.Errorf("process not running")
	}
	if err := cs.execSession.ResizePty(rows, cols); err != nil {
		return err
	}
	cs.rows.Store(int32(rows))
	cs.cols.Store(int32(cols))
	return nil
}

// TerminateShell closes the child shell's exec session channel without touching the
// shared SSH client (CloseSessionOnly). The client lifetime is managed by the parent Session.
func (cs *ChildShell) TerminateShell() {
	if cs.statusValue() != api.SessionRunning {
		return // already terminated
	}
	cs.deliberateClose.Store(true)

	cs.execSession.CloseSessionOnly()
	select {
	case <-cs.execSession.Done():
	case <-time.After(2 * time.Second):
	}
	// Flush output already handed to the process before sealing the buffer,
	// otherwise the tail of the transcript is dropped. The exit state is
	// published only after the bytes are final (same rule as the watcher).
	cs.drainPipes()
	cs.buf.Close()
	cs.setExitCode(-1)
	cs.setStatus(api.SessionExited)
	if parent := cs.parent.Load(); parent != nil {
		cs.retainHistory(parent)
	}
	// Ensure Done() channel is closed for any waiters (closeOnce prevents races with the exit watcher goroutine).
	cs.closeOnce.Do(func() { close(cs.done) })
}
