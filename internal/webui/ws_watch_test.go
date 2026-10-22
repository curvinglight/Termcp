package webui

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshserver"
	"github.com/open-mcp-ai/termcp/internal/storage"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// readDeadline bounds one wait for a frame while watching, and is also the
// yardstick for "did that timeout arrive when it should have": a quiet connection
// reports one only after the full deadline, a dead one reports immediately.
const readDeadline = 2 * time.Second

// TestWatchAddActuallySubscribes drives the Web UI socket the way a browser tab
// does and asks the only question that matters to a user: once a tab says it is
// watching a channel, does that channel's output reach the tab?
//
// It exists because watch_add has no server-side acknowledgement: readPump drops
// addWatch's error (`_ = c.addWatch(id)`), so a failed subscription is completely
// silent — the tab simply never receives bytes and nothing says why. Any handle
// the browser holds but addWatch cannot resolve therefore looks exactly like a
// quiet terminal. This test pins the working path so a regression cannot hide
// behind that silence.
func TestWatchAddActuallySubscribes(t *testing.T) {
	sshSrv := sshserver.New()
	if err := sshSrv.Start(); err != nil {
		t.Fatal(err)
	}
	store := storage.New(t.TempDir())
	msgMgr := message.NewManager(store)
	sessMgr := session.NewManager(msgMgr, store, sshSrv)
	t.Cleanup(func() {
		for _, s := range sessMgr.ListAll() {
			_ = sessMgr.Delete(s.ID)
		}
		sshSrv.Stop()
	})

	h := &Handler{Sessions: sessMgr, MsgMgr: msgMgr, Version: "test"}
	mux := http.NewServeMux()
	h.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	sess, err := sessMgr.Create(session.Config{
		Command:   shellForTest(),
		Args:      shellArgsForTest(),
		Mode:      api.ModePipe,
		Name:      "ws-watch",
		SSHConfig: "internal",
		Rows:      24,
		Cols:      80,
	})
	if err != nil {
		t.Fatal(err)
	}
	shell := sess.PrimaryShell()
	if shell == nil {
		t.Fatal("session has no primary shell")
	}

	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/ui/ws"
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Drain the pushed panel(s) so the reads below are about terminal frames.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		var m map[string]any
		if err := conn.ReadJSON(&m); err != nil {
			t.Fatalf("waiting for the sessions panel: %v", err)
		}
		if m["type"] == "sessions" {
			break
		}
	}

	if err := conn.WriteJSON(map[string]any{"type": "watch_add", "id": shell.ID}); err != nil {
		t.Fatalf("watch_add: %v", err)
	}
	marker := "watch-marker-" + time.Now().Format("150405.000000")

	// THE SHELL IS A PIPE, SO THE LINE ENDS WITH '\n'.
	//
	// Enter is '\r' only on a pty — `shell_key enter` branches on exactly that
	// (internal/session/keys.go) — and a pipe has no line discipline to translate
	// anything, so '\r' is just a byte in the middle of the command and the line never
	// ends. This test creates ModePipe, so '\n' is the separator. Changing it to '\r'
	// made the command never execute at all.
	//
	// THEN LET THE SHELL START, WITHOUT DECIDING HOW LONG THAT TAKES.
	//
	// `handleWSInput` looks the shell up and RETURNS SILENTLY when the session's child
	// shell is not registered yet, so input sent too early is dropped and the test then
	// waits for output nobody asked for. A fixed 200ms sleep sat here and was
	// load-bearing for that reason.
	//
	// A shell that prints a banner gives us something to wait FOR. A silent pipe gives
	// nothing, so the wait is bounded and its expiry is not an error. It cannot share
	// this connection: gorilla caches the first read error and never clears it (see the
	// deadline comment below), so a timeout here would poison the reads that matter.
	// Hence the throwaway connection — it is the only way to ask "has the shell said
	// anything yet" without spending the real one.

	// THE SHELL MUST BE REGISTERED, OR THE INPUT IS DROPPED IN SILENCE.
	//
	// `handleWSInput` resolves the channel with `getTerminalShell` and returns without
	// a word when it is not there yet, so input sent too early simply disappears and
	// the test then waits for output nobody asked for. A fixed 200ms sleep used to sit
	// here for that reason; on a loaded runner it was a coin flip.
	//
	// The wait polls the SAME lookup the input path uses, so it is not a guess about
	// timing, and it costs nothing when the shell is already registered (the usual
	// case). No websocket read is involved: gorilla caches the first read error, so a
	// probe read that times out would poison the connection the assertions need.
	deadline := time.Now().Add(10 * time.Second)
	for sessMgr.GetChildShell(shell.ID) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("channel %s never registered with the session manager", shell.ID)
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := conn.WriteJSON(map[string]any{"type": "input", "id": shell.ID, "d": "echo " + marker + "\n"}); err != nil {
		t.Fatalf("input: %v", err)
	}

	// ONE DEADLINE FOR THE WHOLE WAIT, NOT A POLL PER FRAME.
	//
	// gorilla caches the first read error on a connection and NEVER clears it: one
	// expired read deadline poisons the socket for the rest of its life, and every
	// later read returns that same error in microseconds without touching the wire.
	// A poll loop with a 2s deadline per iteration therefore kills its own connection
	// the first time it waits for output that has not arrived yet -- and then spins,
	// because the failures come back instantly.
	//
	// So the deadline is armed once, for the whole budget. A timeout at the end means
	// the marker never showed up, which is the thing worth failing on. A non-timeout
	// error means the peer is gone. Nothing in between needs a heuristic.
	readDeadline := time.Now().Add(20 * time.Second)
	var got strings.Builder
	_ = conn.SetReadDeadline(readDeadline)
	for {
		var m map[string]any
		if err := conn.ReadJSON(&m); err != nil {
			if isTimeout(err) {
				t.Fatalf("the marker never arrived within %v; got %q", 20*time.Second, got.String())
			}
			t.Fatalf("reading the watch channel: %v (got %q)", err, got.String())
		}
		if m["type"] != "terminal" {
			continue
		}
		if id, _ := m["id"].(string); id != shell.ID {
			continue
		}
		d, _ := m["d"].(string)
		got.WriteString(d)
		if strings.Contains(got.String(), marker) {
			return // the tab received the channel's live output
		}
	}
}

// shellForTest and shellArgsForTest pick a shell that stays alive and echoes,
// which is what an interactive terminal test needs. Kept local to this file so it
// does not collide with other test helpers in the package.
func shellForTest() string {
	if runtime.GOOS == "windows" {
		return "cmd.exe"
	}
	return "/bin/sh"
}

func shellArgsForTest() []string {
	if runtime.GOOS == "windows" {
		return nil
	}
	return []string{"-i"}
}

// isTimeout reports whether err is a read deadline, as opposed to the
// connection being gone. Only the first kind means "keep waiting": re-reading a
// closed WebSocket panics inside gorilla rather than returning again.
func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}
