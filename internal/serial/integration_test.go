package serial

import (
	"bufio"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/serial/port"
)

// These tests open a real device. socat creates a linked pair of pseudo
// terminals, so one end is the "device" and the other is the test's peer: what
// is written to the peer appears on the device and vice versa.
//
// They are skipped where socat is unavailable (Windows CI), because the platform
// has no pty pair to build. The unit tests cover the same logic against a fake
// port, so nothing is untested on those platforms — only unverified against a
// real descriptor.

var ptyPathRe = regexp.MustCompile(`/dev/pts/\d+|/dev/ttys\d+`)

// startPTYPair runs socat and returns the device path and the peer file.
//
// socat's ends are /dev/pts/N, which is a pseudo-terminal and not a serial
// device: the profile allowlist refuses it, correctly, because nothing that
// looks like a UART ever appears there. Rather than widen the allowlist for a
// test, the device path handed back is a symlink created under /dev/serial/by-id
// — an allowed location that resolves to the pty. That is exactly the shape a
// real by-id path has, so the production validation runs unchanged.
func startPTYPair(t *testing.T) (device string, peer *os.File) {
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

	// socat prints the two pty paths on its stderr, one "PTY is" line each.
	scanner := bufio.NewScanner(stderr)
	var paths []string
	deadline := time.After(5 * time.Second)
	found := make(chan struct{})
	go func() {
		defer close(found)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.Contains(line, "PTY is") {
				if m := ptyPathRe.FindString(line); m != "" {
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

	// A by-id path for the device end, so the profile allowlist accepts it.
	aliasDir := "/dev/serial/by-id"
	if err := os.MkdirAll(aliasDir, 0o755); err != nil {
		t.Skipf("cannot create %s (need root): %v", aliasDir, err)
	}
	alias := filepath.Join(aliasDir, "termcp-test-"+strconv.Itoa(os.Getpid()))
	if err := os.Symlink(paths[0], alias); err != nil {
		t.Skipf("cannot symlink %s -> %s: %v", alias, paths[0], err)
	}
	t.Cleanup(func() { os.Remove(alias) })

	return alias, peer
}

func testEntry(device string) *Entry {
	return &Entry{
		Kind:        Kind,
		Device:      device,
		Baud:        115200,
		DataBits:    8,
		StopBits:    "1",
		Parity:      "none",
		DTR:         "keep",
		RTS:         "keep",
		TxEOL:       TxEOLCR,
		Encoding:    EncodingAuto,
		RxNormalize: boolPtr(true),
	}
}

// waitFor reads until want appears or the deadline passes.
func waitFor(t *testing.T, r io.Reader, want string, d time.Duration) string {
	t.Helper()
	type result struct {
		s   string
		err error
	}
	ch := make(chan result, 1)
	go func() {
		buf := make([]byte, 4096)
		var acc strings.Builder
		for {
			n, err := r.Read(buf)
			if n > 0 {
				acc.Write(buf[:n])
				if strings.Contains(acc.String(), want) {
					ch <- result{acc.String(), nil}
					return
				}
			}
			if err != nil {
				ch <- result{acc.String(), err}
				return
			}
		}
	}()

	select {
	case res := <-ch:
		return res.s
	case <-time.After(d):
		t.Fatalf("timed out waiting for %q", want)
		return ""
	}
}

func TestIntegrationWriteAndRead(t *testing.T) {
	device, peer := startPTYPair(t)

	ch, err := Open(testEntry(device), "integration", 0)
	if err != nil {
		t.Fatalf("open %s: %v", device, err)
	}
	defer ch.Close()

	// The peer writes; the device (our channel) must deliver it.
	if _, err := peer.WriteString("hello from peer\n"); err != nil {
		t.Fatalf("peer write: %v", err)
	}

	got := waitFor(t, ch.StdoutReader(), "hello from peer", 5*time.Second)
	if !strings.Contains(got, "hello from peer") {
		t.Fatalf("read %q, want it to contain the peer's text", got)
	}

	// Normalization is on, so the lone LF must have become CRLF.
	if !strings.Contains(got, "\r\n") {
		t.Errorf("read %q: an LF from the device should arrive as CRLF", got)
	}
}

func TestIntegrationChannelWritesReachPeer(t *testing.T) {
	device, peer := startPTYPair(t)

	ch, err := Open(testEntry(device), "integration", 0)
	if err != nil {
		t.Fatalf("open %s: %v", device, err)
	}
	defer ch.Close()

	if _, err := ch.WriteStdin([]byte("AT\r\n")); err != nil {
		t.Fatalf("WriteStdin: %v", err)
	}

	buf := make([]byte, 64)
	got := make(chan string, 1)
	go func() {
		n, _ := peer.Read(buf)
		got <- string(buf[:n])
	}()

	select {
	case s := <-got:
		if !strings.Contains(s, "AT") {
			t.Fatalf("peer read %q, want it to contain AT", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the peer to receive the write")
	}
}

func TestIntegrationDeviceBusyIsReported(t *testing.T) {
	device, _ := startPTYPair(t)

	first, err := Open(testEntry(device), "first", 0)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	defer first.Close()

	// A pty can be opened twice: the kernel does not make one exclusive, which
	// is a property of the fixture rather than of serial devices. The check that
	// matters here is only that when a platform does refuse, the failure arrives
	// as one of our sentinels rather than a raw driver error.
	second, err := Open(testEntry(device), "second", 0)
	if err == nil {
		second.Close()
		t.Skip("a pty does not enforce single-open; exclusivity is a real-device property")
	}
	if !errors.Is(err, port.ErrBusy) && !errors.Is(err, port.ErrPermission) {
		t.Fatalf("second open = %v, want a busy or permission sentinel", err)
	}
}

// Device loss is not covered here, and the reason is worth stating rather than
// leaving as a gap.
//
// When a pty's peer closes, the device node stays present and reads keep
// returning (0, nil) forever: there is no error to observe and nothing for the
// existence check to find missing. A USB adapter being unplugged is different —
// the node disappears, which is what DeviceExists catches, and the driver
// starts returning errors, which is what the consecutive-failure path catches.
// The unit tests drive both of those paths against a fake port
// (TestReadLoopMarksAbortedOnDeviceGone, TestRepeatedReadFailuresReportAborted,
// TestDeviceDisappearanceDetectedByExistenceCheck).
//
// This matches the limitation the design already records: a pure RS-232 device
// on a cable that is pulled leaves the session looking alive.

func TestIntegrationAcquireBlocksSecondProfile(t *testing.T) {
	device, _ := startPTYPair(t)

	release, err := Acquire(device, "first")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer release()

	if _, err := Acquire(device, "second"); !errors.Is(err, port.ErrBusy) {
		t.Fatalf("second Acquire = %v, want ErrBusy", err)
	}
}
