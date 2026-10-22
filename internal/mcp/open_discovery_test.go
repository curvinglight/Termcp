package mcp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

// toolListPeer is an MCP-ish peer that answers `tools/list` with a real tool list,
// so a test can see what `mcp open` discovered.
func toolListPeer(t *testing.T) *httptest.Server {
	t.Helper()
	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			var f map[string]any
			if err := conn.ReadJSON(&f); err != nil {
				return
			}
			switch f["method"] {
			case "tools/list":
				_ = conn.WriteJSON(map[string]any{
					"jsonrpc": "2.0", "id": f["id"],
					"result": map[string]any{"tools": []any{
						map[string]any{"name": "read_file", "description": "Read a file from the workspace"},
						map[string]any{"name": "run_query", "description": "Execute a SQL query"},
					}},
				})
			default:
				_ = conn.WriteJSON(map[string]any{"id": f["id"], "result": map[string]any{"ok": true}})
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestMcpOpenReportsDiscoveredTools pins design review §3.3② and its acceptance
// criterion: "can an agent that has never read the MCP spec make the next call
// correctly from this reply alone?"
//
// Before this, `mcp open` answered with a handle and a transport name, and the next
// step — an `initialize` handshake, then `tools/list`, then a call addressed by tool
// name — was unguessable without the spec. The adapter exists to carry that state
// machine, so it should hand back the state machine's result, not a receipt.
func TestMcpOpenReportsDiscoveredTools(t *testing.T) {
	srv := toolListPeer(t)
	s := newTestServer(t)
	internalSession(t, s) // gateways need a session to tunnel through

	out := termText(t, s, map[string]any{"cmd": "open internal mcp ws" + strings.TrimPrefix(srv.URL, "http")})
	h := handleFrom(t, out)
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	if !strings.Contains(out, "read_file") {
		t.Errorf("open must report the peer's tools:\n%s", out)
	}
	if !strings.Contains(out, "run_query") {
		t.Errorf("every tool should be listed:\n%s", out)
	}
	if !strings.Contains(out, "tools/call") {
		t.Errorf("open must name the next call's shape, since that is the unguessable part:\n%s", out)
	}
	// The handle must appear in the suggested command, or the caller still has to
	// join two pieces of the reply by hand.
	if !strings.Contains(out, "call "+h) {
		t.Errorf("the suggested call must use this handle:\n%s", out)
	}
}

// TestMcpOpenSucceedsEvenWhenDiscoveryFails pins the best-effort rule: a peer that
// opens but cannot list tools is still an open connection, and failing the whole
// open over a discovery query would turn a partial success into a total failure.
func TestMcpOpenSucceedsEvenWhenDiscoveryFails(t *testing.T) {
	srv, _ := countingPeer(t, nil) // answers initialize, but tools/list returns no tools array
	s := newTestServer(t)
	internalSession(t, s) // gateways need a session to tunnel through

	out := termText(t, s, map[string]any{"cmd": "open internal mcp ws" + strings.TrimPrefix(srv.URL, "http")})
	if !strings.HasPrefix(out, "TERM/1 200") {
		t.Fatalf("open must succeed even when discovery cannot list tools:\n%s", out)
	}
	h := handleFrom(t, out)
	defer termText(t, s, map[string]any{"cmd": "close " + h})
}

// TestJrpcOpenDoesNotClaimTools pins that the three adapters keep their own shapes:
// only MCP has a tool list, so only MCP reports one.
func TestJrpcOpenDoesNotClaimTools(t *testing.T) {
	srv, _ := countingPeer(t, nil)
	s := newTestServer(t)
	internalSession(t, s) // gateways need a session to tunnel through

	out := termText(t, s, map[string]any{"cmd": "open internal jrpc ws" + strings.TrimPrefix(srv.URL, "http")})
	h := handleFrom(t, out)
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	if strings.Contains(out, "tools/call") {
		t.Errorf("jrpc is a bare frame channel and must not advertise tool discovery:\n%s", out)
	}
}
