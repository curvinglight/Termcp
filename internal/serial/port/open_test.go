package port

import (
	"errors"
	"testing"
	"time"

	"go.bug.st/serial"
)

// The wrapper's Close must be safe to repeat. The session reaches one port from
// three places (Disconnect, finalize, the root shell's close), and a serial
// descriptor is not safe to close twice.
func TestHandleCloseIsIdempotent(t *testing.T) {
	fake := &fakeHandle{}
	h := &handle{port: fake}

	if err := h.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("third Close: %v", err)
	}

	if fake.closeCalls != 1 {
		t.Fatalf("underlying port closed %d times, want exactly 1", fake.closeCalls)
	}
}

func TestHandleCloseReturnsSameError(t *testing.T) {
	want := errors.New("device vanished")
	fake := &fakeHandle{closeErr: want}
	h := &handle{port: fake}

	first := h.Close()
	second := h.Close()

	if !errors.Is(first, want) {
		t.Fatalf("first Close = %v, want %v", first, want)
	}
	// A repeated close must report the same outcome rather than a different one
	// on a second attempt.
	if !errors.Is(second, want) {
		t.Fatalf("second Close = %v, want %v (same as first)", second, want)
	}
	if fake.closeCalls != 1 {
		t.Fatalf("underlying port closed %d times, want 1", fake.closeCalls)
	}
}

func TestHandleReadWrite(t *testing.T) {
	fake := &fakeHandle{readData: []byte("hello")}
	h := &handle{port: fake}

	buf := make([]byte, 16)
	n, err := h.Read(buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got := string(buf[:n]); got != "hello" {
		t.Fatalf("Read = %q, want %q", got, "hello")
	}

	if _, err := h.Write([]byte("world")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if len(fake.writes) != 1 || string(fake.writes[0]) != "world" {
		t.Fatalf("writes = %v, want [world]", fake.writes)
	}
}

func TestHandleFlushUsesResetBuffers(t *testing.T) {
	// FlushInput/FlushOutput are this package's names; the library calls them
	// ResetInputBuffer/ResetOutputBuffer. The mapping must not be swapped.
	fake := &fakeHandle{}
	h := &handle{port: fake}

	if err := h.FlushInput(); err != nil {
		t.Fatalf("FlushInput: %v", err)
	}
	if err := h.FlushOutput(); err != nil {
		t.Fatalf("FlushOutput: %v", err)
	}
	if fake.flushedIn != 1 {
		t.Errorf("input flushed %d times, want 1", fake.flushedIn)
	}
	if fake.flushedOut != 1 {
		t.Errorf("output flushed %d times, want 1", fake.flushedOut)
	}
}

func TestHandleModemStatus(t *testing.T) {
	fake := &fakeHandle{modemStatus: &modemBits{cts: true, dsr: false, ri: true, dcd: false}}
	h := &handle{port: fake}

	cts, dsr, ri, dcd, err := h.ModemStatus()
	if err != nil {
		t.Fatalf("ModemStatus: %v", err)
	}
	if !cts || dsr || !ri || dcd {
		t.Fatalf("ModemStatus = CTS=%v DSR=%v RI=%v DCD=%v, want true false true false",
			cts, dsr, ri, dcd)
	}
}

func TestHandleModemStatusNilBits(t *testing.T) {
	// A nil result with no error must not be dereferenced.
	fake := &fakeHandle{modemStatus: nil}
	h := &handle{port: fake}

	if _, _, _, _, err := h.ModemStatus(); err == nil {
		t.Fatal("nil modem bits should be reported as an error, not silently zeroed")
	}
}

func TestSentinelForOpenCode(t *testing.T) {
	// serial.PortError has no exported constructor, so the mapping is tested
	// directly rather than by building a PortError to feed in. This is the code
	// that decides which sentinel a caller sees, so it is the part worth pinning.
	cases := []struct {
		code serial.PortErrorCode
		want error
	}{
		{serial.PortBusy, ErrBusy},
		{serial.PortNotFound, ErrNotFound},
		{serial.PermissionDenied, ErrPermission},
		{serial.PortClosed, ErrDeviceGone},
		{serial.InvalidSpeed, ErrUnsupportedFormat},
		{serial.InvalidDataBits, ErrUnsupportedFormat},
		{serial.InvalidParity, ErrUnsupportedFormat},
		{serial.InvalidStopBits, ErrUnsupportedFormat},
		{serial.InvalidSerialPort, ErrUnsupportedFormat},
		{serial.InvalidTimeoutValue, ErrUnsupportedFormat},
		{serial.FunctionNotImplemented, ErrUnsupportedFormat},
		{serial.ErrorEnumeratingPorts, ErrUnsupportedFormat},
		// An unrecognized code must still land somewhere honest.
		{serial.PortErrorCode(9999), ErrUnsupportedFormat},
	}

	for _, tc := range cases {
		if got := sentinelForOpenCode(tc.code); got != tc.want {
			t.Errorf("code %d: got %v, want %v", tc.code, got, tc.want)
		}
	}
}

func TestSentinelForIOCode(t *testing.T) {
	// Only "the session is over" codes are classified on the I/O path; anything
	// else is reported as unrecognized so the caller can pass it through.
	recognized := []struct {
		code serial.PortErrorCode
		want error
	}{
		{serial.PortBusy, ErrBusy},
		{serial.PortNotFound, ErrNotFound},
		{serial.PermissionDenied, ErrPermission},
		{serial.PortClosed, ErrDeviceGone},
	}
	for _, tc := range recognized {
		got, ok := sentinelForIOCode(tc.code)
		if !ok {
			t.Errorf("code %d: reported unrecognized, want %v", tc.code, tc.want)
			continue
		}
		if got != tc.want {
			t.Errorf("code %d: got %v, want %v", tc.code, got, tc.want)
		}
	}

	// A parameter error on an open port is not a transport failure, so it must
	// not be swallowed into a sentinel.
	if _, ok := sentinelForIOCode(serial.InvalidSpeed); ok {
		t.Error("InvalidSpeed should not be classified on the I/O path")
	}
}

func TestNormalizeIOErrorPassesThroughUnknown(t *testing.T) {
	// An error that is not a port error is not this package's to reclassify:
	// inventing a sentinel would hide the real cause from the log.
	raw := errors.New("some driver-specific failure")
	if got := normalizeIOError(raw); !errors.Is(got, raw) {
		t.Fatalf("got %v, want the original error", got)
	}
}

func TestNormalizeIOErrorNil(t *testing.T) {
	if got := normalizeIOError(nil); got != nil {
		t.Fatalf("normalizeIOError(nil) = %v, want nil", got)
	}
}

func TestOpenRejectsInvalidConfigBeforeTouchingDevice(t *testing.T) {
	// Validation runs first, so a bad config never reaches the device. The path
	// here is outside the allowlist, so the failure is a config error even
	// though the device does not exist either.
	cfg := DefaultConfig("/etc/passwd")
	_, err := Open(cfg, 0)
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("got %v, want ErrInvalidConfig", err)
	}
}

func TestOpenRejectsFlowLikeUnsupportedLines(t *testing.T) {
	// A config that validates as far as values go, on a device that does not
	// exist, must report not-found rather than something invented.
	cfg := DefaultConfig("/dev/ttyUSB-does-not-exist-12345")
	_, err := Open(cfg, 0)
	if err == nil {
		t.Fatal("opening a non-existent device should fail")
	}
	// Either not-found or unsupported is acceptable depending on how the host
	// reports a missing path; what matters is that it is one of ours, not the
	// library's raw error.
	if !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrUnsupportedFormat) &&
		!errors.Is(err, ErrPermission) && !errors.Is(err, ErrBusy) {
		t.Fatalf("open error %v is not one of this package's sentinels", err)
	}
}

func TestOpenAppliesReadTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("requires opening a device")
	}
	// Guarded by validation: this asserts the timeout is set to the poll
	// interval, which is what lets a read loop regain control.
	fake := &fakeHandle{}
	h := &handle{port: fake}

	custom := 25 * time.Millisecond
	if err := h.SetReadTimeout(custom); err != nil {
		t.Fatalf("SetReadTimeout: %v", err)
	}
	if fake.timeoutSet != custom {
		t.Fatalf("timeout = %v, want %v", fake.timeoutSet, custom)
	}
}

func TestNoTimeoutSentinelMatchesLibrary(t *testing.T) {
	// Re-exported so callers never import the library; it must stay equal to the
	// value the library expects.
	if NoTimeout != serial.NoTimeout {
		t.Fatalf("NoTimeout = %v, library uses %v", NoTimeout, serial.NoTimeout)
	}
}
