package serial

import (
	"io"
	"testing"
	"time"
)

// The session package declares shellChannel and transport but cannot be imported
// here: session will import serial, so the dependency has to run one way. These
// mirrors repeat the two interfaces as they are declared there, so a signature
// drift shows up as a compile error in this package. session/channel_test.go
// holds the authoritative assertion; if one side changes without the other, the
// build breaks in whichever package was not updated.
type shellChannelMirror interface {
	WriteStdin([]byte) (int, error)
	StdoutReader() io.Reader
	StderrReader() io.Reader
	Done() <-chan struct{}
	ExitCode() int
	Aborted() bool
	CloseReaders()
	CloseSessionOnly() error
	Close() error
}

type transportMirror interface {
	Close() error
	CloseSessionOnly() error
	Done() <-chan struct{}
}

var (
	_ shellChannelMirror = (*Channel)(nil)
	_ transportMirror    = (*Channel)(nil)
)

func TestStderrReaderIsNeverNil(t *testing.T) {
	// The session's pipe goroutines call Read on whatever this returns. A nil
	// reader would panic them on the first use, so "no stderr" has to be an
	// empty reader rather than nothing.
	c := &Channel{}
	r := c.StderrReader()
	if r == nil {
		t.Fatal("StderrReader must not return nil")
	}

	n, err := r.Read(make([]byte, 8))
	if err != io.EOF {
		t.Fatalf("Read on the empty stderr = (%d, %v), want (0, io.EOF)", n, err)
	}
	if n != 0 {
		t.Fatalf("empty stderr returned %d bytes, want 0", n)
	}
}

func TestExitCodeIsMinusOne(t *testing.T) {
	// A serial device has no process, so there is no exit status. -1 is the
	// sentinel the session records.
	c := &Channel{}
	if got := c.ExitCode(); got != -1 {
		t.Fatalf("ExitCode = %d, want -1", got)
	}
}

func TestAbortedDefaultsFalse(t *testing.T) {
	c := &Channel{}
	if c.Aborted() {
		t.Fatal("a fresh channel should not report aborted")
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	// Disconnect, finalize and the root shell's close can each reach the same
	// channel; the underlying port must be closed exactly once.
	fake := newFakePort()
	c := &Channel{
		handle:      fake,
		profile:     &Entry{Kind: Kind},
		stdout:      newPipeReader(),
		done:        make(chan struct{}),
		readersDone: make(chan struct{}),
	}

	if err := c.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := c.CloseSessionOnly(); err != nil {
		t.Fatalf("CloseSessionOnly: %v", err)
	}

	if got := fake.closeCalls(); got != 1 {
		t.Fatalf("underlying port closed %d times, want 1", got)
	}
}

func TestCloseUnblocksDone(t *testing.T) {
	fake := newFakePort()
	c := &Channel{
		handle:      fake,
		profile:     &Entry{Kind: Kind},
		stdout:      newPipeReader(),
		done:        make(chan struct{}),
		readersDone: make(chan struct{}),
	}

	select {
	case <-c.Done():
		t.Fatal("Done should be open before Close")
	default:
	}

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	select {
	case <-c.Done():
	case <-time.After(time.Second):
		t.Fatal("Close should close Done")
	}
}

func TestWriteAfterCloseFails(t *testing.T) {
	fake := newFakePort()
	c := &Channel{
		handle:      fake,
		profile:     &Entry{Kind: Kind},
		stdout:      newPipeReader(),
		done:        make(chan struct{}),
		readersDone: make(chan struct{}),
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := c.WriteStdin([]byte("x")); err == nil {
		t.Fatal("writing to a closed channel should fail")
	}
}

func TestWriteStdinReachesPort(t *testing.T) {
	fake := newFakePort()
	c := &Channel{
		handle:      fake,
		profile:     &Entry{Kind: Kind},
		stdout:      newPipeReader(),
		done:        make(chan struct{}),
		readersDone: make(chan struct{}),
	}

	if _, err := c.WriteStdin([]byte("AT\r")); err != nil {
		t.Fatalf("WriteStdin: %v", err)
	}
	if got := string(fake.written()); got != "AT\r" {
		t.Fatalf("port received %q, want %q", got, "AT\r")
	}
}

func TestLocalEchoCopiesToStdout(t *testing.T) {
	// With local_echo the bytes are also written into the transcript, so it
	// shows what was sent even though the device never echoes.
	fake := newFakePort()
	c := &Channel{
		handle:      fake,
		profile:     &Entry{Kind: Kind, LocalEcho: true},
		stdout:      newPipeReader(),
		done:        make(chan struct{}),
		readersDone: make(chan struct{}),
	}

	if _, err := c.WriteStdin([]byte("hi")); err != nil {
		t.Fatalf("WriteStdin: %v", err)
	}

	buf := make([]byte, 8)
	done := make(chan int, 1)
	go func() {
		n, _ := c.StdoutReader().Read(buf)
		done <- n
	}()

	select {
	case n := <-done:
		if got := string(buf[:n]); got != "hi" {
			t.Fatalf("echoed %q, want %q", got, "hi")
		}
	case <-time.After(time.Second):
		t.Fatal("local echo should have produced readable output")
	}
}

func TestNoLocalEchoByDefault(t *testing.T) {
	fake := newFakePort()
	c := &Channel{
		handle:      fake,
		profile:     &Entry{Kind: Kind},
		stdout:      newPipeReader(),
		done:        make(chan struct{}),
		readersDone: make(chan struct{}),
	}

	if _, err := c.WriteStdin([]byte("hi")); err != nil {
		t.Fatalf("WriteStdin: %v", err)
	}

	// Nothing should be readable: the device did not reply and local_echo is off.
	got := make(chan struct{})
	go func() {
		c.StdoutReader().Read(make([]byte, 8))
		close(got)
	}()
	select {
	case <-got:
		t.Fatal("without local_echo, writing must not echo into the transcript")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestControllerDelegatesToPort(t *testing.T) {
	// DTR/RTS/Break/Flush/ModemStatus are the serial_ctl surface; each must reach
	// the port rather than being swallowed.
	fake := newFakePort()
	c := &Channel{handle: fake}

	if err := c.SetDTR(true); err != nil {
		t.Fatalf("SetDTR: %v", err)
	}
	if err := c.SetRTS(false); err != nil {
		t.Fatalf("SetRTS: %v", err)
	}
	if err := c.Break(50 * time.Millisecond); err != nil {
		t.Fatalf("Break: %v", err)
	}
	if err := c.FlushInput(); err != nil {
		t.Fatalf("FlushInput: %v", err)
	}
	if err := c.FlushOutput(); err != nil {
		t.Fatalf("FlushOutput: %v", err)
	}
	if _, _, _, _, err := c.ModemStatus(); err != nil {
		t.Fatalf("ModemStatus: %v", err)
	}

	if fake.dtr != true {
		t.Error("SetDTR(true) did not reach the port")
	}
	if fake.rts != false {
		t.Error("SetRTS(false) did not reach the port")
	}
	if fake.breakCalls != 1 {
		t.Error("Break did not reach the port")
	}
	if fake.flushIn != 1 || fake.flushOut != 1 {
		t.Error("Flush did not reach the port")
	}
	if fake.modemCalls != 1 {
		t.Error("ModemStatus did not reach the port")
	}
}

func TestCloseReadersDoesNotEndChannel(t *testing.T) {
	// CloseReaders detaches the session's readers; the channel itself stays
	// usable, matching the SSH transport.
	c := &Channel{
		handle:      newFakePort(),
		profile:     &Entry{Kind: Kind},
		stdout:      newPipeReader(),
		done:        make(chan struct{}),
		readersDone: make(chan struct{}),
	}

	c.CloseReaders()
	c.CloseReaders() // must not panic on a second call

	select {
	case <-c.Done():
		t.Fatal("CloseReaders must not close the channel")
	default:
	}
}

func TestDeviceAndBaudExposeProfile(t *testing.T) {
	c := &Channel{profile: &Entry{Kind: Kind, Device: "/dev/ttyUSB0", Baud: 115200}}
	if c.Baud() != 115200 {
		t.Fatalf("Baud = %d, want 115200", c.Baud())
	}
	if c.Device() == "" {
		t.Fatal("Device should not be empty")
	}
}
