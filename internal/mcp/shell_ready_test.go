package mcp

import (
	"strings"
	"testing"
	"time"
)

// TestAPtyOpenWaitsForTheShellToSpeak is issue #156.
//
// The handle used to be handed out as soon as the process existed, and the shell's line
// editor — PSReadLine, readline, zle — takes over the terminal a moment later. Input that
// arrives in between is DISCARDED rather than buffered, and nothing says so: `run` answers
// 200 and the command never happens, on every channel of that session, forever. On a
// machine whose PowerShell draws a themed prompt this was reproducible roughly one time in
// three.
//
// The fix is the smallest one that can be correct: a pty open waits for the shell to say
// something before it answers. A shell that has printed its prompt has installed its editor,
// and that is the only signal it gives.
func TestAPtyOpenWaitsForTheShellToSpeak(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	// THE WRITE HAPPENS IMMEDIATELY, which is the whole point: no sleep, no wait for a
	// prompt, because the open already waited.
	termText(t, s, map[string]any{"cmd": "run " + h + " $1", "args": []any{"echo ready-marker"}})

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(termText(t, s, map[string]any{"cmd": "read " + h + " --from-line 1", "timeout": 0}), "ready-marker") {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("a command written immediately after the open never reached the shell")
}

// TestAPipeOpenDoesNotWait pins the other half of the rule: a pipe's bytes go into the
// process's own stdin, which the OS buffers, so there is nothing to wait for and waiting
// would be a lie — `--argv sleep 30` prints nothing for thirty seconds by design.
func TestAPipeOpenDoesNotWait(t *testing.T) {
	s := newTestServer(t)
	start := time.Now()
	h := openPipe(t, s, "cat")
	defer termText(t, s, map[string]any{"cmd": "close " + h})
	if elapsed := time.Since(start); elapsed > shellReadyBudget {
		t.Fatalf("a pipe open waited %v for output that a pipe does not need to produce", elapsed)
	}

	// And its input still arrives: a pipe buffers, so the bytes are there when the
	// process reads them.
	termText(t, s, map[string]any{"cmd": "run " + h + " $1", "args": []any{"hello-pipe"}})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		// A zero timeout makes the read a peek: without it every poll would wait out the
		// tool's blocking window, and the loop would take three seconds per look.
		if strings.Contains(termText(t, s, map[string]any{"cmd": "read " + h + " --from-line 1", "timeout": 0}), "hello-pipe") {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("input written to a pipe channel never appeared in its echo")
}

// TestAChildChannelIsAlsoReadyWhenItAnswers pins that the wait covers `open` as well as
// `connect`: a second channel starts a second process, with its own editor to install.
func TestAChildChannelIsAlsoReadyWhenItAnswers(t *testing.T) {
	s := newTestServer(t)
	first := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + first})
	sid, _, _ := strings.Cut(first, ":")

	second := handleFrom(t, termText(t, s, map[string]any{"cmd": "open " + sid + " --name s2"}))
	defer termText(t, s, map[string]any{"cmd": "close " + second})

	termText(t, s, map[string]any{"cmd": "run " + second + " $1", "args": []any{"echo second-marker"}})
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(termText(t, s, map[string]any{"cmd": "read " + second + " --from-line 1", "timeout": 0}), "second-marker") {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("a command written immediately after a child channel opened never reached its shell")
}
