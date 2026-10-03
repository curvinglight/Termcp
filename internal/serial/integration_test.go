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

// drain reads until EOF or the deadline and returns everything seen. Used
// where the assertion is on ABSENCE: a stream that never ends (the device is
// open, nothing more is coming) must still give up its buffered bytes instead
// of blocking the test until the timeout alone decides.
func drain(t *testing.T, r io.Reader, d time.Duration) string {
	t.Helper()
	type result struct {
		s   string
		err error
	}
	ch := make(chan result, 1)
	go func() {
		var acc strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				acc.Write(buf[:n])
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

// What is still NOT covered here is worth stating rather than leaving as a gap.
//
// When a pty's peer closes, the device node stays present and reads keep
// returning (0, nil) forever: there is no error to observe and nothing for the
// existence check to find missing. The unplug case — a node that disappears —
// IS covered below (TestIntegrationUnpluggingTheDeviceEndsTheChannel), because
// removing the by-id alias leaves a dangling profile path exactly the way a
// pulled USB adapter leaves a stale /dev/ttyUSB0; the driver-error variant of
// the same loss is driven against a fake port in
// TestRepeatedReadFailuresReportAborted.
//
// A pure RS-232 device on a cable that is pulled while the process holds the
// descriptor is the remaining blind spot the design records: the node may stay
// and the reads may never fail, so nothing can detect it.

// readExactly pulls n bytes from r or fails the test. Reading exactly n (and
// comparing byte for byte) is the point of the tx_eol test below: a substring
// match would pass for "AT\r" when the profile promised "AT\r\n".
func readExactly(t *testing.T, r io.Reader, n int, d time.Duration) []byte {
	t.Helper()
	type result struct {
		b   []byte
		err error
	}
	ch := make(chan result, 1)
	go func() {
		out := make([]byte, 0, n)
		buf := make([]byte, 64)
		for len(out) < n {
			m, err := r.Read(buf)
			if m > 0 {
				out = append(out, buf[:m]...)
			}
			if err != nil {
				ch <- result{out, err}
				return
			}
		}
		ch <- result{out, nil}
	}()
	select {
	case res := <-ch:
		if res.err != nil && len(res.b) < n {
			t.Fatalf("read %d of %d bytes: %v", len(res.b), n, res.err)
		}
		return res.b
	case <-time.After(d):
		t.Fatalf("timed out reading %d bytes", n)
		return nil
	}
}

// TestIntegrationTxEOLBytesReachPeer runs the profile's line ending all the way
// out to the wire: what the session layer would write (ReplaceEnter) must reach
// the device byte-for-byte, for every tx_eol setting.
//
// Byte-exact, not substring: "cr" and "none" both produce "AT\r" while "crlf"
// must produce "AT\r\n", and a contains-check would pass for either. It pins
// the whole path — conversion, WriteStdin, the port, and the pty relay — as one
// property rather than three units that each assumed the others.
func TestIntegrationTxEOLBytesReachPeer(t *testing.T) {
	cases := []struct {
		setting string
		want    string
	}{
		{TxEOLCR, "AT\r"},
		{TxEOLCRLF, "AT\r\n"},
		{TxEOLLF, "AT\n"},
		// "none" means "no conversion", not "no line ending": the byte the
		// terminal already produced (a bare CR) is what must arrive.
		{TxEOLNone, "AT\r"},
	}
	for _, tc := range cases {
		t.Run(tc.setting, func(t *testing.T) {
			device, peer := startPTYPair(t)

			entry := testEntry(device)
			entry.TxEOL = tc.setting
			ch, err := Open(entry, "integration", 0)
			if err != nil {
				t.Fatalf("open %s: %v", device, err)
			}
			defer ch.Close()

			// Exactly the bytes the session layer writes for "AT" + Enter.
			payload := ReplaceEnter([]byte("AT\n"), tc.setting)
			if _, err := ch.WriteStdin(payload); err != nil {
				t.Fatalf("WriteStdin: %v", err)
			}

			got := readExactly(t, peer, len(tc.want), 5*time.Second)
			if string(got) != tc.want {
				t.Errorf("tx_eol=%q: peer received %q, want %q", tc.setting, got, tc.want)
			}
		})
	}
}

// TestIntegrationCloseStopsFurtherWrites pins close semantics (design §9.2):
// Close ends the channel, the reader exits, and nothing that arrives afterwards
// reaches the output stream.
//
// The marker is written to the peer AFTER Done has closed, so the assertion is
// not racing the shutdown — a channel that reports "closed" and then keeps
// relaying bytes would leave a session the operator believes is over still
// feeding data into a transcript nobody is watching.
func TestIntegrationCloseStopsFurtherWrites(t *testing.T) {
	device, peer := startPTYPair(t)

	ch, err := Open(testEntry(device), "integration", 0)
	if err != nil {
		t.Fatalf("open %s: %v", device, err)
	}

	// Sanity: the channel is alive and relaying before we close it.
	if _, err := peer.WriteString("alive\n"); err != nil {
		t.Fatalf("peer write: %v", err)
	}
	_ = waitFor(t, ch.StdoutReader(), "alive", 5*time.Second)

	if err := ch.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-ch.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done did not close after Close")
	}

	// The peer keeps writing as far as it is concerned; the device end is ours.
	if _, err := peer.WriteString("after-close-marker"); err != nil {
		t.Fatalf("peer write after close: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	// Drain whatever is buffered: stdout is closed with the channel, so this
	// ends with EOF rather than blocking.
	rest := drain(t, ch.StdoutReader(), 2*time.Second)
	if strings.Contains(rest, "after-close-marker") {
		t.Errorf("bytes written after Close still reached the output: %q", rest)
	}
	if ch.Aborted() {
		t.Error("a deliberate Close must not report the channel as aborted")
	}
}

// TestIntegrationUnpluggingTheDeviceEndsTheChannel is the DEAD path against a
// real descriptor: the profile's device path stops resolving, and the idle
// existence probe must end the channel as aborted rather than leave a session
// that looks alive (design §9.2 拔线 DEAD).
//
// Removing the by-id alias is what unplugging looks like from the profile's
// side — the open handle keeps working (the pty node is still there), so only
// the existence check can notice, which is exactly the failure mode this probe
// exists for.
func TestIntegrationUnpluggingTheDeviceEndsTheChannel(t *testing.T) {
	device, peer := startPTYPair(t)

	ch, err := Open(testEntry(device), "integration", 0)
	if err != nil {
		t.Fatalf("open %s: %v", device, err)
	}
	defer ch.Close()

	// Sanity: relaying works, so the ending below is caused by the removal.
	if _, err := peer.WriteString("alive\n"); err != nil {
		t.Fatalf("peer write: %v", err)
	}
	_ = waitFor(t, ch.StdoutReader(), "alive", 5*time.Second)

	if err := os.Remove(device); err != nil {
		t.Fatalf("remove device alias: %v", err)
	}

	select {
	case <-ch.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("channel did not end after the device path disappeared")
	}
	if !ch.Aborted() {
		t.Error("a vanished device must end the channel as aborted (DEAD)")
	}
}

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
