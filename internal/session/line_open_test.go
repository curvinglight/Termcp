package session

import (
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/storage"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// newLineOpenShell starts an interactive session and returns its primary
// shell, the same fixture the activity tests use.
func newLineOpenShell(t *testing.T) *ChildShell {
	t.Helper()
	srv := startTestServer(t)
	store := storage.New(t.TempDir())
	mm := message.NewManager(store)
	mgr := NewManager(mm, store, srv)
	t.Cleanup(func() {
		for _, info := range mgr.ListAll() {
			_ = mgr.Delete(info.ID)
		}
	})
	s, err := mgr.Create(testConfig(testShell(), testInteractiveShellArgs(), api.ModePTY, ""))
	if err != nil {
		t.Fatal(err)
	}
	cs := s.PrimaryShell()
	if cs == nil {
		t.Fatal("session has no primary shell")
	}
	return cs
}

// TestLineOpenTracksOpenLine pins the flag that shell_input reports, and the
// failure it exists to prevent: two shell_input calls with no enter between them
// reach the target as ONE line, and nothing in the output distinguishes that
// from one deliberate command.
//
// The flag is termcp's own bookkeeping, not something read back from the
// device, so it is asserted directly rather than through a session's output.
func TestLineOpenTracksOpenLine(t *testing.T) {
	cs := newLineOpenShell(t)

	if cs.LineOpen() {
		t.Error("a fresh shell reports an open line; nothing has been typed yet")
	}

	// shell_input with pressEnter=false: bytes written, line still open.
	if err := cs.SendTerminalBytesFrom([]byte("echo hi"), false, InputFromAI); err != nil {
		t.Fatal(err)
	}
	if !cs.LineOpen() {
		t.Error("after typing without enter, LineOpen() = false, want true")
	}

	// A second call with no enter in between is the S-6 case. The flag cannot
	// prevent the concatenation — only an enter can — but it is the signal that
	// lets the caller see it happened.
	if err := cs.SendTerminalBytesFrom([]byte("rm -rf /tmp/x"), false, InputFromAI); err != nil {
		t.Fatal(err)
	}
	if !cs.LineOpen() {
		t.Error("after a second unterminated input, LineOpen() = false, want true")
	}

	// shell_key(enter) is what closes the line.
	if err := cs.PressKeyFrom("enter", 1, InputFromAI); err != nil {
		t.Fatal(err)
	}
	if cs.LineOpen() {
		t.Error("after enter, LineOpen() = true, want false")
	}
}

// TestLineOpenClearedByAbandoningKeys covers the control keys that a line
// editor treats as ending a line even though they carry no newline: ctrl+c
// aborts the typed line, and a chip or an agent that only ever left "typing" on
// an enter would sit on "typing" for the rest of the session after an
// interrupt. submitsLine already classifies them; this pins that the flag
// follows that classification rather than a newline check.
func TestLineOpenClearedByAbandoningKeys(t *testing.T) {
	for _, key := range []string{"ctrl+c", "ctrl+d", "ctrl+z"} {
		t.Run(key, func(t *testing.T) {
			cs := newLineOpenShell(t)
			if err := cs.SendTerminalBytesFrom([]byte("half-typed command"), false, InputFromAI); err != nil {
				t.Fatal(err)
			}
			if !cs.LineOpen() {
				t.Fatal("setup: the typed line should be open")
			}
			if err := cs.PressKeyFrom(key, 1, InputFromAI); err != nil {
				t.Fatal(err)
			}
			if cs.LineOpen() {
				t.Errorf("%s did not clear the open line; LineOpen() = true, want false", key)
			}
		})
	}
}

// TestLineOpenSetByKeyThatEndsLineWithoutText guards the other direction: a
// bare enter with nothing typed is still a submit, so it must not report a line
// left open. An agent that pressed enter to get a fresh prompt should not be
// told it now has a half-written command.
func TestLineOpenSetByKeyThatEndsLineWithoutText(t *testing.T) {
	cs := newLineOpenShell(t)
	if err := cs.PressKeyFrom("enter", 1, InputFromAI); err != nil {
		t.Fatal(err)
	}
	if cs.LineOpen() {
		t.Error("a bare enter reported an open line, want false")
	}
}

// TestLineOpenFollowsRepeatedEnter pins that a submit does not itself
// re-open a line: enter ends a line, and the flag must not flip back on each
// press. A flag that toggled per call would be worse than none.
func TestLineOpenFollowsRepeatedEnter(t *testing.T) {
	cs := newLineOpenShell(t)
	for i := 0; i < 3; i++ {
		if err := cs.PressKeyFrom("enter", 1, InputFromAI); err != nil {
			t.Fatal(err)
		}
		if cs.LineOpen() {
			t.Fatalf("after enter #%d, LineOpen() = true, want false", i+1)
		}
	}
}

// TestLineOpenNotReportedForExitedShell checks the boundary that matters for
// the flag: an exited shell refuses further input, so the MCP layer never gets
// as far as reading line_open, and a stale true can never be reported for a
// channel that no longer accepts bytes.
//
// The flag is deliberately NOT cleared on exit. Nothing reads it afterwards —
// the write guard refuses first — and clearing it would mean the exit watcher
// had to reach into input bookkeeping for no observable gain. What is asserted
// here is the guard that makes the flag unobservable.
func TestLineOpenNotReportedForExitedShell(t *testing.T) {
	cs := newLineOpenShell(t)
	if err := cs.SendTerminalBytesFrom([]byte("echo hi"), false, InputFromAI); err != nil {
		t.Fatal(err)
	}
	if !cs.LineOpen() {
		t.Fatal("setup: the typed line should be open")
	}

	cs.mu.Lock()
	cs.Status = api.SessionExited
	cs.mu.Unlock()

	if err := cs.SendTerminalBytesFrom([]byte("more"), false, InputFromAI); err == nil {
		t.Error("input to an exited shell was accepted, want an error")
	}
}

// TestLineOpenConcurrentAccess runs the flag through concurrent readers and
// the write path. It is here to be run under -race: the flag is atomic because
// LineOpen is read from the MCP reply while a Web UI keystroke may be
// writing, and a plain bool there would be a data race rather than a wrong
// answer.
func TestLineOpenConcurrentAccess(t *testing.T) {
	cs := newLineOpenShell(t)
	done := make(chan struct{})

	go func() {
		defer close(done)
		deadline := time.Now().Add(200 * time.Millisecond)
		for time.Now().Before(deadline) {
			_ = cs.LineOpen()
		}
	}()

	for i := 0; i < 50; i++ {
		if err := cs.SendTerminalBytesFrom([]byte("x"), false, InputFromAI); err != nil {
			t.Fatal(err)
		}
		if err := cs.PressKeyFrom("enter", 1, InputFromAI); err != nil {
			t.Fatal(err)
		}
	}
	<-done
}
