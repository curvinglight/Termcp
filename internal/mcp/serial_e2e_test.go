package mcp

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/approval"
	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/serial"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
	"github.com/open-mcp-ai/termcp/internal/storage"
)

var mcpPTYPathRe = regexp.MustCompile(`/dev/pts/\d+|/dev/ttys\d+`)

// startSerialPTYForMCP hands the test a real device for the MCP serial path:
// socat's linked pty pair with the device end symlinked under /dev/serial/by-id,
// the exact shape a real by-id path has, so the production allowlist, the open
// and the read loop all run unchanged.
//
// Skipped where that shape cannot be built: Windows has no pty pairs, and the
// alias directory needs write access to /dev (GitHub runners are not root).
// The acquire-level tests cover occupancy on every platform; this file covers
// the wiring through session/MCP on the platforms with a device.
func startSerialPTYForMCP(t *testing.T) (device string, peer *os.File) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("no pty pairs on Windows")
	}
	if _, err := exec.LookPath("socat"); err != nil {
		t.Skip("socat not installed")
	}

	cmd := exec.Command("socat", "-d", "-d", "pty,raw,echo=0", "pty,raw,echo=0")
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("socat stderr pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start socat: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	scanner := bufio.NewScanner(stderr)
	var paths []string
	deadline := time.After(5 * time.Second)
	found := make(chan struct{})
	go func() {
		defer close(found)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.Contains(line, "PTY is") {
				if m := mcpPTYPathRe.FindString(line); m != "" {
					paths = append(paths, m)
					if len(paths) == 2 {
						return
					}
				}
			}
		}
	}()
	select {
	case <-found:
	case <-deadline:
		t.Fatal("timed out waiting for socat to report its pty pair")
	}
	if len(paths) < 2 {
		t.Fatalf("socat reported %d ptys, want 2", len(paths))
	}

	peer, err = os.OpenFile(paths[1], os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open peer pty %s: %v", paths[1], err)
	}
	t.Cleanup(func() { peer.Close() })

	if err := os.MkdirAll("/dev/serial/by-id", 0o755); err != nil {
		t.Skipf("cannot create /dev/serial/by-id (need root): %v", err)
	}
	// A per-package prefix: the serial and webui packages build the same kind of
	// alias and their test binaries run concurrently under `go test ./internal/...`.
	alias := filepath.Join("/dev/serial/by-id", "termcp-mcp-test-"+strconv.Itoa(os.Getpid()))
	if err := os.Symlink(paths[0], alias); err != nil {
		t.Skipf("cannot symlink %s -> %s: %v", alias, paths[0], err)
	}
	t.Cleanup(func() { os.Remove(alias) })
	return alias, peer
}

// newMCPSerialServer builds a server with the serial store wired in and no SSH
// server behind it: the serial path never dials, so an SSH fixture would be
// dead weight. The session manager still gets a cleanup that deletes every
// session, because a serial session holds the device claim until it is deleted.
func newMCPSerialServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	store := storage.New(dir)
	msgMgr := message.NewManager(store)
	sessMgr := session.NewManager(msgMgr, store, nil)
	t.Cleanup(func() {
		for _, s := range sessMgr.ListAll() {
			_ = sessMgr.Delete(s.ID)
		}
	})
	return New(sessMgr, msgMgr, sshconfig.NewStore(dir), nil, "test",
		WithSerialStore(serial.NewStore(dir)))
}

// pendingCount counts requests still awaiting a decision.
func pendingCount(q *approval.Queue) int {
	n := 0
	for _, r := range q.List() {
		if r.State == approval.Pending {
			n++
		}
	}
	return n
}

// The MCP surface of a serial session, end to end against a real device.
//
// It pins four things the design requires of this path (§9.1, §6.5):
//
//   - the session opens as kind=serial with a shell, and both "give me another
//     channel" and "resize my terminal" are refused with their own codes rather
//     than a generic failure;
//   - serial_ctl's read-only action is never gated — an operator must not need
//     approval to look at a pin;
//   - the state-changing actions ride the review queue like file and forward
//     writes, with a summary a human can decide on;
//   - approval replays the operation through the same handler without queueing
//     it again, and there is no action that writes arbitrary bytes.
func TestSerialSessionAndControlReview(t *testing.T) {
	device, _ := startSerialPTYForMCP(t)
	s := newMCPSerialServer(t)

	if err := s.serialConfigs.Save("esp32", []byte("kind = \"serial\"\ndevice = \""+device+"\"\nbaud = 115200\n")); err != nil {
		t.Fatalf("save profile: %v", err)
	}

	// --- open the session ---
	res, err := s.handleStartSession(context.Background(), makeRequest(map[string]any{
		"ssh_config": "esp32",
	}))
	if err != nil {
		t.Fatal(err)
	}
	start := parseResult(t, res)
	sessionID, _ := start["session_id"].(string)
	shellID, _ := start["shell_id"].(string)
	if sessionID == "" || shellID == "" {
		t.Fatalf("start returned %v, want session_id and shell_id", start)
	}
	if start["kind"] != "serial" {
		t.Errorf("kind = %v, want serial", start["kind"])
	}
	t.Cleanup(func() { s.sessMgr.Delete(sessionID) })

	// --- a second channel is refused by name ---
	res, err = s.handleStartSubShell(context.Background(), makeRequest(map[string]any{
		"session_id": sessionID,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := extractErrorInfo(res); code != CodeSerialSingleChannel {
		t.Errorf("second channel error_code = %q, want %q", code, CodeSerialSingleChannel)
	}

	// --- a serial session has no terminal to resize ---
	res, err = s.handleResizePty(context.Background(), makeRequest(map[string]any{
		"shell_id": shellID, "rows": float64(24), "cols": float64(80),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := extractErrorInfo(res); code != CodeSerialNoPTY {
		t.Errorf("resize error_code = %q, want %q", code, CodeSerialNoPTY)
	}

	// --- review mode on ---
	live := s.sessMgr.Get(sessionID)
	if live == nil {
		t.Fatal("session missing from the manager")
	}
	if err := live.EnableApproval(1, time.Minute); err != nil {
		t.Fatalf("EnableApproval: %v", err)
	}
	q := live.ApprovalQueue()
	if q == nil {
		t.Fatal("no approval queue")
	}

	// status is read-only: it must pass straight through, queue untouched. On a
	// pty the modem-line read itself fails (there are no lines), which is fine —
	// the property under test is that a failure reaches the caller as a normal
	// error result instead of becoming a review hold.
	res, err = s.handleSerialCtl(context.Background(), makeRequest(map[string]any{
		"action": "status", "shell_id": shellID,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if body := parseResult(t, res); body["review_pending"] == true {
		t.Errorf("status was gated for review: %v", body)
	}
	if n := pendingCount(q); n != 0 {
		t.Errorf("status queued %d requests, want 0", n)
	}

	// A state-changing action is queued, described, and carries no id the agent
	// could use to approve its own request.
	res, err = s.handleSerialCtl(context.Background(), makeRequest(map[string]any{
		"action": "dtr", "level": "high", "shell_id": shellID,
	}))
	if err != nil {
		t.Fatal(err)
	}
	body := parseResult(t, res)
	if body["review_pending"] != true {
		t.Fatalf("dtr was not held for review: %v", body)
	}
	if _, has := body["pending_id"]; has {
		t.Error("the held reply must not carry a pending_id")
	}
	reqs := q.List()
	if len(reqs) != 1 {
		t.Fatalf("queued %d requests, want 1", len(reqs))
	}
	dtrReq := reqs[0]
	if dtrReq.Kind != approval.KindSerialCtl {
		t.Errorf("kind = %q, want %q", dtrReq.Kind, approval.KindSerialCtl)
	}
	if !strings.Contains(dtrReq.Summary, "dtr") {
		t.Errorf("summary %q does not name the action, so a reviewer cannot decide", dtrReq.Summary)
	}
	if len(dtrReq.Payload) == 0 {
		t.Error("the request has no payload to replay")
	}

	// A malformed request fails for the agent that sent it, not for the reviewer
	// who would have approved it: validation runs before queueing.
	res, err = s.handleSerialCtl(context.Background(), makeRequest(map[string]any{
		"action": "dtr", "level": "maybe", "shell_id": shellID,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := extractErrorInfo(res); code != CodeInvalidArgument {
		t.Errorf("bad level error_code = %q, want %q", code, CodeInvalidArgument)
	}
	if n := pendingCount(q); n != 1 {
		t.Errorf("a malformed dtr changed the queue to %d pending, want 1", n)
	}

	// flush rides the same queue…
	res, err = s.handleSerialCtl(context.Background(), makeRequest(map[string]any{
		"action": "flush", "direction": "both", "shell_id": shellID,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if body := parseResult(t, res); body["review_pending"] != true {
		t.Fatalf("flush was not held for review: %v", body)
	}
	if n := pendingCount(q); n != 2 {
		t.Fatalf("after flush: %d pending, want 2", n)
	}
	var flushReq approval.Request
	for _, r := range q.List() {
		if r.Kind == approval.KindSerialCtl && strings.Contains(r.Summary, "flush") {
			flushReq = r
		}
	}
	if flushReq.ID == "" {
		t.Fatal("the flush request is not in the queue")
	}

	// …and approval replays it through the same handler without queueing it
	// again. A gate that re-queues on replay could never finish, and one that
	// skips the handler would run a different code path than the agent called.
	if _, err := q.Approve(flushReq.ID); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if err := s.ExecuteApprovedOperation(flushReq); err != nil {
		t.Fatalf("executing the approved flush: %v", err)
	}
	if n := pendingCount(q); n != 1 {
		t.Errorf("after replay: %d pending, want 1 (the replay must not re-queue)", n)
	}
	approved, err := q.Get(flushReq.ID)
	if err != nil {
		t.Fatalf("get after replay: %v", err)
	}
	if approved.State != approval.Approved {
		t.Errorf("flush state = %q, want approved", approved.State)
	}

	// No arbitrary write: an action outside the enum is refused and never
	// queued, so serial_ctl cannot become a byte-payload side channel.
	res, err = s.handleSerialCtl(context.Background(), makeRequest(map[string]any{
		"action": "write", "shell_id": shellID,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := extractErrorInfo(res); code != CodeInvalidArgument {
		t.Errorf("write action error_code = %q, want %q", code, CodeInvalidArgument)
	}
	if n := pendingCount(q); n != 1 {
		t.Errorf("a refused action changed the queue to %d pending, want 1", n)
	}

	// The leftover dtr is never replayed here; drop it so the session teardown
	// does not race an expiring request.
	q.Cancel(dtrReq.ID, "test cleanup")
}
