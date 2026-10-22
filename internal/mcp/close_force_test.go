package mcp

import (
	"strings"
	"testing"
)

// TestCloseSessionTerminatesEntireSession pins that `close <session>` (with NO
// --force, because stopping is the default and keeping the record is what happens)
// terminates the entire session: every channel ends and the connection drops.
func TestCloseSessionTerminatesEntireSession(t *testing.T) {
	s := newTestServer(t)

	h := openPty(t, s)
	sessID := h
	if idx := strings.Index(h, ":"); idx > 0 {
		sessID = h[:idx]
	}

	psBefore := termText(t, s, map[string]any{"cmd": "ls"})
	if !strings.Contains(psBefore, sessID) {
		t.Fatalf("session %q must be running in ps before close:\n%s", sessID, psBefore)
	}

	closeRes := termText(t, s, map[string]any{"cmd": "close " + sessID})
	if !strings.Contains(closeRes, "204") {
		t.Fatalf("close must return 204:\n%s", closeRes)
	}

	psAfter := termText(t, s, map[string]any{"cmd": "ls"})
	for _, line := range strings.Split(psAfter, "\n") {
		if strings.Contains(line, sessID) && strings.Contains(line, "running") {
			t.Errorf("session %q is still running after close:\n%s", sessID, psAfter)
		}
	}

	info := termText(t, s, map[string]any{"cmd": "get " + sessID})
	if strings.Contains(info, "status: running") {
		t.Errorf("get %s reports status: running after close:\n%s", sessID, info)
	}
}

// TestCloseNoSuchTargetReturns404 pins that a non-existent handle or session returns
// 404 instead of pretending success with 204.
func TestCloseNoSuchTargetReturns404(t *testing.T) {
	s := newTestServer(t)

	for _, cmd := range []string{
		"close nosuchhandle123",
		"close nosuchsession456",
	} {
		res := termText(t, s, map[string]any{"cmd": cmd})
		if !strings.Contains(res, "404") {
			t.Errorf("%q must return 404, got:\n%s", cmd, res)
		}
	}
}
