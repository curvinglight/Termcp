package mcp

import (
	"encoding/json"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// The serial surface, driven the way an agent drives it: through `term` and the
// handlers the directives reach, never by calling startSerialSession directly —
// a test that bypasses the dispatch tests a function nobody can call.

// startPTYPair runs socat and returns the device path termcp should open plus a
// cleanup. The pair's far end is where a test plays the device: a pty is not a
// serial line, so tests that need line discipline or modem ioctls skip (the
// port package's own tests cover those where the OS offers them). What this
// buys is the whole path — profile, session, channel, transcript — on a real
// open() of a real device node, which is where the wiring mistakes live.
func startPTYPair(t *testing.T) (device string, peer string, stop func()) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("pty pairs are a unix fixture")
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
	buf := make([]byte, 4096)
	var paths []string
	// socat prints one "PTY is" line per end on its stderr.
	deadline := 50
	for len(paths) < 2 && deadline > 0 {
		n, _ := stderr.Read(buf)
		if n == 0 {
			deadline--
			continue
		}
		for _, line := range strings.Split(string(buf[:n]), "\n") {
			if i := strings.Index(line, "PTY is "); i >= 0 {
				paths = append(paths, strings.TrimSpace(line[i+len("PTY is "):]))
			}
		}
	}
	if len(paths) != 2 {
		cmd.Process.Kill()
		t.Fatalf("socat reported %d ptys, want 2", len(paths))
	}
	// The profile validator only accepts real serial paths (/dev/ttyUSB…,
	// /dev/serial/…), which is the point of the allowlist: a pty is not a
	// serial line. The udev by-id directory is where an alias for this test's
	// device belongs, and writing there needs root — skip where it is read-only.
	aliasDir := "/dev/serial/by-id"
	alias := aliasDir + "/termcp-test-" + t.Name()
	if err := os.MkdirAll(aliasDir, 0o755); err != nil {
		cmd.Process.Kill()
		t.Skipf("cannot create %s (need root): %v", aliasDir, err)
	}
	if err := os.Symlink(paths[0], alias); err != nil {
		cmd.Process.Kill()
		t.Skipf("cannot symlink %s -> %s: %v", alias, paths[0], err)
	}
	return alias, paths[1], func() {
		cmd.Process.Kill()
		cmd.Wait()
		os.Remove(alias)
	}
}

// serialTestServer is a test server whose profile store already holds one serial
// profile and one remote profile, the pair every mixed-kind test needs.
func serialTestServer(t *testing.T, device string) *Server {
	t.Helper()
	s := newTestServer(t)
	s.ManageSSHConfigs = true
	res, err := s.call(t.Context(), "ssh_config", map[string]any{
		"action": "create", "name": "tty0", "kind": "serial",
		"device": device, "baud": float64(115200),
	})
	if err != nil || res.IsError {
		t.Fatalf("create serial profile: %v / %s", err, toolResultText(res))
	}
	return s
}

func TestConnectSerialProfileOpensADeviceSession(t *testing.T) {
	device, _, stop := startPTYPair(t)
	defer stop()
	s := serialTestServer(t, device)

	out := termText(t, s, map[string]any{"cmd": "connect tty0"})
	if !strings.Contains(out, "Handle: ") || !strings.Contains(out, "session") {
		t.Fatalf("connect did not answer a session:\n%s", out)
	}
	// The handle resolves: get on the returned session answers, and says what
	// was opened.
	doc := termText(t, s, map[string]any{"cmd": "ls"})
	if !strings.Contains(doc, "tty0") {
		t.Fatalf("the serial session is not in the listing:\n%s", doc)
	}
	// SSH-only operations refuse with the transport's own code, not
	// session_not_running.
	sid := extractSerialSessionID(t, s, "tty0")
	res, err := s.call(t.Context(), "file_read", map[string]any{"session_id": sid, "remote_path": "/etc/hosts"})
	if err != nil {
		t.Fatalf("file_read: %v", err)
	}
	txt := toolResultText(res)
	if !strings.Contains(txt, "serial") {
		t.Fatalf("file_read on a serial session should name the transport:\n%s", txt)
	}
	// Close it the ordinary way.
	if res := termText(t, s, map[string]any{"cmd": "close " + sid}); strings.HasPrefix(res, "TERM/1 5") {
		t.Fatalf("close failed:\n%s", res)
	}
}

func extractSerialSessionID(t *testing.T, s *Server, profile string) string {
	t.Helper()
	out := termText(t, s, map[string]any{"cmd": "ls --all"})
	for _, line := range strings.Split(out, "\n") {
		cols := strings.Split(strings.TrimSpace(line), "\t")
		// <session>:<index>  <status>  <mode>  <name>  <age>
		if len(cols) >= 4 && cols[3] == profile {
			return strings.SplitN(cols[0], ":", 2)[0]
		}
	}
	t.Fatalf("no session for %s in:\n%s", profile, out)
	return ""
}

func TestConnectSerialRefusesWhatADeviceCannotMean(t *testing.T) {
	device, _, stop := startPTYPair(t)
	defer stop()
	s := serialTestServer(t, device)

	for _, tc := range []struct{ cmd, want string }{
		{"connect tty0 --argv sleep 5", "--argv"},
		{"connect tty0 --pty", "pty"},
		{"connect tty0 --pipe", "pipe"},
		{"connect tty0 --size 24x80", "size"},
	} {
		out := termText(t, s, map[string]any{"cmd": tc.cmd})
		if !strings.Contains(out, "400") || !strings.Contains(out, tc.want) {
			t.Errorf("%s: want a 400 naming %q:\n%s", tc.cmd, tc.want, out)
		}
	}
}

func TestGetHostShowsTheDevice(t *testing.T) {
	device, _, stop := startPTYPair(t)
	defer stop()
	s := serialTestServer(t, device)

	out := termText(t, s, map[string]any{"cmd": "get host tty0"})
	for _, want := range []string{"serial", device, "115200"} {
		if !strings.Contains(out, want) {
			t.Errorf("get host missing %q:\n%s", want, out)
		}
	}
	// And it does NOT invent a dial spec: a serial profile has no host.
	if strings.Contains(out, "has_password") {
		t.Errorf("serial profile printed SSH dial fields:\n%s", out)
	}
}

func TestLSPortListsTheMachine(t *testing.T) {
	s := newTestServer(t)
	out := termText(t, s, map[string]any{"cmd": "ls port"})
	if !strings.HasPrefix(out, "TERM/1") {
		t.Fatalf("ls port did not answer:\n%s", out)
	}
}

func TestSetOnAnSSHChannelIsRefused(t *testing.T) {
	s := newTestServer(t)
	res, err := s.call(t.Context(), "session_start", map[string]any{"ssh_config": "internal"})
	if err != nil || res.IsError {
		t.Fatalf("session_start: %v / %s", err, toolResultText(res))
	}
	doc := parseResult(t, res)
	handle, _ := doc["shell_id"].(string)
	if handle == "" {
		t.Fatalf("no shell handle in %v", doc)
	}
	res, _ = s.call(t.Context(), "serial_ctl", map[string]any{"shell_id": handle, "action": "dtr", "level": "high"})
	if res == nil || res.IsError == false {
		t.Fatalf("set dtr on an SSH channel should fail")
	}
	txt := toolResultText(res)
	if !strings.Contains(txt, "serial") {
		t.Fatalf("refusal should name the capability:\n%s", txt)
	}
}

func TestSetDTRGoesThroughReview(t *testing.T) {
	device, _, stop := startPTYPair(t)
	defer stop()
	s := serialTestServer(t, device)
	termText(t, s, map[string]any{"cmd": "connect tty0"})
	sid := extractSerialSessionID(t, s, "tty0")
	handle := sid + ":1"

	// Review mode on: the pin change is device state, so it queues for a human
	// rather than running. Enabled directly on the live session, the way the
	// approval tests do — the surface spellings of the switch are covered there.
	sess := s.sessMgr.Get(sid)
	if sess == nil {
		t.Fatalf("no session %s", sid)
	}
	if err := sess.EnableApproval(1, 0); err != nil {
		t.Fatalf("EnableApproval: %v", err)
	}
	slotText := termText(t, s, map[string]any{"cmd": "set " + handle + " dtr high"})
	if !strings.Contains(slotText, "202") && !strings.Contains(slotText, "review") && !strings.Contains(slotText, "Submitted") {
		t.Fatalf("set dtr under review should be held:\n%s", slotText)
	}
}

func TestSerialProfileCharsetDecodesDeviceOutput(t *testing.T) {
	device, peer, stop := startPTYPair(t)
	defer stop()
	s := newTestServer(t)
	s.ManageSSHConfigs = true
	res, err := s.call(t.Context(), "ssh_config", map[string]any{
		"action": "create", "name": "gbkdev", "kind": "serial",
		"device": device, "baud": float64(115200), "charset": "gbk",
	})
	if err != nil || res.IsError {
		t.Fatalf("create: %v / %s", err, toolResultText(res))
	}
	sres, err := s.call(t.Context(), "session_start", map[string]any{"ssh_config": "gbkdev"})
	if err != nil || sres.IsError {
		t.Fatalf("session_start: %v / %s", err, toolResultText(sres))
	}
	doc := parseResult(t, sres)
	handle, _ := doc["shell_id"].(string)

	// The device speaks GBK: "中文" arrives as D6 D0 CE C4, bytes no UTF-8
	// decoder accepts. The profile's charset is the decode default, so the
	// model-facing read answers with the characters, not a refusal.
	p := peerDevice(t, peer)
	p.Write([]byte{0xD6, 0xD0, 0xCE, 0xC4, '\r', '\n'})

	rres, err := s.call(t.Context(), "shell_output", map[string]any{"shell_id": handle, "timeout": 3.0})
	if err != nil || rres.IsError {
		t.Fatalf("shell_output: %v / %s", err, toolResultText(rres))
	}
	out := parseResult(t, rres)
	output, _ := out["output"].(string)
	if !strings.Contains(output, "中文") {
		t.Fatalf("GBK output should decode via the profile charset, got %q", output)
	}
}

func TestSerialCharsetOffsetReadProbe(t *testing.T) {
	device, peer, stop := startPTYPair(t)
	defer stop()
	s := newTestServer(t)
	s.ManageSSHConfigs = true
	res, err := s.call(t.Context(), "ssh_config", map[string]any{
		"action": "create", "name": "gbkdev", "kind": "serial",
		"device": device, "baud": float64(115200), "charset": "gbk",
	})
	if err != nil || res.IsError {
		t.Fatalf("create: %v / %s", err, toolResultText(res))
	}
	sres, err := s.call(t.Context(), "session_start", map[string]any{"ssh_config": "gbkdev"})
	if err != nil || sres.IsError {
		t.Fatalf("session_start: %v / %s", err, toolResultText(sres))
	}
	doc := parseResult(t, sres)
	handle, _ := doc["shell_id"].(string)
	p := peerDevice(t, peer)
	p.Write([]byte{0xD6, 0xD0, 0xCE, 0xC4, '\r', '\n'})
	// EXACTLY what `read <channel> --offset 0` sends: positional read with the
	// tool's own maxima and the default strip. The output pipes flush on their
	// own goroutine, so the read polls until the device's bytes have landed —
	// the same wait-for-output rule the run tests follow.
	deadline := time.Now().Add(5 * time.Second)
	var out map[string]any
	for {
		rres, err := s.call(t.Context(), "shell_output", map[string]any{
			"shell_id": handle, "offset": float64(0), "max_bytes": float64(8192),
			"max_lines": float64(200), "timeout": 3.0,
		})
		if err != nil || rres.IsError {
			t.Fatalf("shell_output: %v / %s", err, toolResultText(rres))
		}
		out = parseResult(t, rres)
		if n, _ := out["total_bytes"].(float64); n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("device bytes never reached the transcript: %v", out)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(output_(out), "中文") {
		t.Fatalf("positional read lost the GBK text: %q", out)
	}
}

func output_(m map[string]any) string { s, _ := m["output"].(string); return s }

func TestSerialProfileCreateRejectsMixedFields(t *testing.T) {
	s := newTestServer(t)
	s.ManageSSHConfigs = true
	res, _ := s.call(t.Context(), "ssh_config", map[string]any{
		"action": "create", "name": "bad", "kind": "serial",
		"device": "/dev/ttyUSB0", "baud": float64(9600), "host": "h", "user": "u", "password": "p",
	})
	if res == nil || !res.IsError {
		t.Fatal("a serial create carrying SSH credentials should be refused")
	}
	if !strings.Contains(toolResultText(res), "host") {
		t.Fatalf("refusal should name the fields:\n%s", toolResultText(res))
	}
}

func TestSerialSessionDisplayFromProfile(t *testing.T) {
	device, peer, stop := startPTYPair(t)
	defer stop()
	s := newTestServer(t)
	s.ManageSSHConfigs = true
	res, err := s.call(t.Context(), "ssh_config", map[string]any{
		"action": "create", "name": "hexdev", "kind": "serial",
		"device": device, "baud": float64(115200), "display": "hex",
	})
	if err != nil || res.IsError {
		t.Fatalf("create: %v / %s", err, toolResultText(res))
	}
	sres, err := s.call(t.Context(), "session_start", map[string]any{"ssh_config": "hexdev"})
	if err != nil || sres.IsError {
		t.Fatalf("session_start: %v / %s", err, toolResultText(sres))
	}
	doc := parseResult(t, sres)
	handle, _ := doc["shell_id"].(string)

	// The device (the pty's far end) emits bytes no UTF-8 decoder accepts. With
	// display=hex the window is ASCII hex — the misconfigured-baud case that
	// started the feature: the client sees the bytes it asked for, not U+FFFD.
	p := peerDevice(t, peer)
	p.Write([]byte{0x00, 0xff, '\n'})

	rres, err := s.call(t.Context(), "shell_output", map[string]any{"shell_id": handle, "timeout": 3.0})
	if err != nil || rres.IsError {
		t.Fatalf("shell_output: %v / %s", err, toolResultText(rres))
	}
	out := parseResult(t, rres)
	output, _ := out["output"].(string)
	if !strings.Contains(output, "00ff") {
		t.Fatalf("hex display expected 00ff, got %q", output)
	}
}

// peerDevice opens the far end of the pair so a test can play the device. The
// raw file is enough: the peer plays bytes, not line discipline.
func peerDevice(t *testing.T, path string) *ptyPeer {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open peer %s: %v", path, err)
	}
	t.Cleanup(func() { f.Close() })
	return &ptyPeer{f: f}
}

type ptyPeer struct{ f *os.File }

func (p *ptyPeer) Write(b []byte) (int, error) { return p.f.Write(b) }

var _ = json.Marshal
var _ = mcpgo.CallToolRequest{}

// TestLSHostNamesASerialProfileAsSerial pins the listing's kind column.
//
// Every non-internal profile used to be labelled "ssh", so a serial profile sat in
// the same column as a remote box with a dash where user@host:port goes: the one
// row a caller reads to decide "can I connect to this" answered with a transport
// that profile does not use. The device and its line speed are what `where` means
// for this kind.
func TestLSHostNamesASerialProfileAsSerial(t *testing.T) {
	device, _, stop := startPTYPair(t)
	defer stop()
	s := serialTestServer(t, device)

	out := termText(t, s, map[string]any{"cmd": "ls host"})
	row := ""
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "tty0") {
			row = line
		}
	}
	if row == "" {
		t.Fatalf("the serial profile is not listed:\n%s", out)
	}
	if !strings.Contains(row, "\tserial\t") {
		t.Errorf("the kind column should say serial, not ssh: %q", row)
	}
	if !strings.Contains(row, device) {
		t.Errorf("the where column should name the device: %q", row)
	}
}
