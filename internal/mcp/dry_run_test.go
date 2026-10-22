package mcp

import (
	"strings"
	"testing"
)

// TestRmDryRunDescribesWithoutErasing pins feedback §2.4: the approval API's shape,
// reserved now so that adding review mode later does not change the contract.
//
// A preview that erases is not a preview, so the assertion is on the effect as well
// as the reply: the session must still exist afterwards.
func TestRmDryRunDescribesWithoutErasing(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	sessID := h
	if i := strings.Index(h, ":"); i > 0 {
		sessID = h[:i]
	}
	// Stop it, so the real rm would not need --force and the preview can be about
	// the erase alone.
	termText(t, s, map[string]any{"cmd": "close " + sessID})

	got := termText(t, s, map[string]any{"cmd": "rm " + sessID + " --dry-run"})
	if !strings.HasPrefix(got, "TERM/1 200") {
		t.Fatalf("dry run is not a failure:\n%s", got)
	}
	if !strings.Contains(got, "Dry-Run: true") {
		t.Errorf("the reply must be marked as a dry run:\n%s", got)
	}
	if !strings.Contains(got, "would erase") {
		t.Errorf("the reply must say what would happen:\n%s", got)
	}

	// The session must STILL EXIST: a preview that erases is not a preview.
	after := termText(t, s, map[string]any{"cmd": "get " + sessID})
	if strings.Contains(after, "not found") || strings.HasPrefix(after, "TERM/1 404") {
		t.Fatalf("dry run erased the session:\n%s", after)
	}
	termText(t, s, map[string]any{"cmd": "rm " + sessID})
}

// TestRmDryRunMentionsForceForARunningSession pins that the preview carries the same
// warning the real call would enforce, so the caller learns what the real call needs
// before making it.
func TestRmDryRunMentionsForceForARunningSession(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})
	sessID := strings.Split(h, ":")[0]

	got := termText(t, s, map[string]any{"cmd": "rm " + sessID + " --dry-run"})
	if !strings.Contains(got, "Dry-Run: true") {
		t.Fatalf("expected a dry run:\n%s", got)
	}
	if !strings.Contains(got, "--force") {
		t.Errorf("a running session's preview must say the real call needs --force:\n%s", got)
	}
	// Still running.
	after := termText(t, s, map[string]any{"cmd": "get " + sessID})
	if strings.Contains(after, "not found") {
		t.Fatalf("dry run removed the running session:\n%s", after)
	}
}
