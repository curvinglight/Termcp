package mcp

import (
	"strings"
	"testing"
	"time"
)

// TestPtyEmptyReadReportsWaitedAndSampled pins 误读 3 & 桩 5:
//
// A 204 that arrives in 0.03ms is a crash/socket death; a 204 in 2000ms is legitimate
// silence. They used to look identical.
//
// And an empty read of a pty is a SAMPLE of a live shell that outlives every command;
// it does not mean the command exited. Both facts must be in the reply.
func TestPtyEmptyReadReportsWaitedAndSampled(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	// DRAIN UNTIL QUIET: a pty shell prints its prompt in several bursts (theme,
	// then the cursor escape, then the trailing newline), so one read is not enough
	// to reach the state this test is about. The 204 under test is what a read
	// answers ONCE THE STREAM IS SILENT.
	var got string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		got = termText(t, s, map[string]any{"cmd": "read " + h, "timeout": float64(1)})
		if strings.HasPrefix(got, "TERM/1 204") {
			break
		}
	}

	if !strings.Contains(got, "Sampled: true") {
		t.Errorf("empty pty read must carry Sampled: true so caller knows shell is alive:\n%s", got)
	}
	if !strings.Contains(got, "Waited:") {
		t.Errorf("reply must report actual elapsed time (Waited header):\n%s", got)
	}
}

// TestSemicolonBetweenRunAndShellCommandExplainsItself pins 误读 1:
//
// A naive caller writing `run h:1 cd /tmp; ls -la` must not get a bare
// `unknown directive: ls`. The reply must explain that `;` is termcp syntax, not
// shell syntax, and point at args: [...].
func TestSemicolonBetweenRunAndShellCommandExplainsItself(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	// Two statements, the second is a shell command that was accidentally split.
	got := termText(t, s, map[string]any{"cmd": "run " + h + " true; make -j4"})

	if !strings.Contains(got, "unknown directive: make") {
		t.Fatalf("expected unknown directive for make:\n%s", got)
	}
	// The reply must name the SEMICOLON specifically. "unknown directive: ls" alone
	// reads as "termcp has no make", and the caller's next attempt is another guess.
	if !strings.Contains(got, "termcp syntax") && !strings.Contains(got, "`;`") {
		t.Errorf("error must explain that `;` is termcp syntax, not shell syntax:\n%s", got)
	}
	if !strings.Contains(got, "args:") {
		t.Errorf("error must suggest args: [...] as the escape:\n%s", got)
	}
	if !strings.Contains(got, "nothing was executed") {
		t.Errorf("error must say whether anything ran:\n%s", got)
	}
}
