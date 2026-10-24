package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/open-mcp-ai/termcp/pkg/api"
)

// TestExtensionListingPrintsAnAddressNotAChannelID pins issue #159 D2.
//
// The reconciler reports a shell id (`9ff338e1-68b`), which identifies the channel
// but is not the spelling the rest of the surface takes: an address is
// `<session>:<index>`, the same one `ps` prints. A caller that copied the shell id
// out of `ls ext` had to call `get ext` to translate it before it could act.
func TestExtensionListingPrintsAnAddressNotAChannelID(t *testing.T) {
	s := newTestServer(t)
	sess := createTestSession(t, s, api.ModePipe)
	live := s.sessMgr.Get(sess)
	if live == nil {
		t.Fatal("session not found")
	}
	shells := live.ListChildShells()
	if len(shells) == 0 {
		t.Fatal("the session has no channel to address")
	}
	want := handleOf(sess, shells[0].Index)

	body, err := json.Marshal(map[string]any{"extensions": []map[string]any{{
		"name": "probe", "status": "available",
		"session_id": sess, "shell_id": shells[0].ID,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	got := s.extensionListing(mcpgo.NewToolResultText(string(body)))
	if !strings.Contains(got, want) {
		t.Errorf("the listing does not print the address %q:\n%s", want, got)
	}
	if strings.Contains(got, shells[0].ID) {
		t.Errorf("the listing still prints the raw channel id %q:\n%s", shells[0].ID, got)
	}

	// An extension with no channel of its own reports its session, which is the
	// address a file read takes.
	only, err := json.Marshal(map[string]any{"extensions": []map[string]any{{
		"name": "docs", "status": "available", "session_id": sess,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.extensionListing(mcpgo.NewToolResultText(string(only))); !strings.Contains(got, sess) {
		t.Errorf("a shell-less extension should report its session:\n%s", got)
	}
}

// TestCreateExtensionReportsStartingNotUnavailable pins issue #159 D3.
//
// Status() reports nothing until the reconciler has walked once, and the fallback
// was `unavailable` — the word for "tried and failed". A caller that had just
// created an extension therefore read a successful create as a broken one and
// retried it in a loop.
func TestCreateExtensionReportsStartingNotUnavailable(t *testing.T) {
	s := newExtensionTestServer(t)

	res, err := s.handleExtensionCreate(context.Background(), makeRequest(map[string]any{
		"action": "create", "name": "fresh", "kind": "skill", "target": "internal",
		"paths": []any{"/srv/"},
	}))
	if err != nil || res.IsError {
		t.Fatalf("create: %v", err)
	}
	doc := parseResult(t, res)
	if doc["status"] == "unavailable" {
		t.Errorf("a fresh template reports unavailable, which reads as a failed create: %v", doc["status"])
	}
	if doc["status"] != "probing" {
		t.Errorf("status = %v, want the transitional one", doc["status"])
	}
	if next, _ := doc["next"].(string); !strings.Contains(next, "fresh") {
		t.Errorf("the next-hint must name the extension so it can be pasted: %q", next)
	}

	// A disabled template is not "starting": nothing will ever probe it.
	res, err = s.handleExtensionCreate(context.Background(), makeRequest(map[string]any{
		"action": "create", "name": "off", "kind": "skill", "target": "internal",
		"paths": []any{"/srv/"}, "enabled": false,
	}))
	if err != nil || res.IsError {
		t.Fatalf("create: %v", err)
	}
	if got := parseResult(t, res)["status"]; got != "disabled" {
		t.Errorf("a disabled template reports %v, want disabled", got)
	}
}
