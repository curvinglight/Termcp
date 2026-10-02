package session

import (
	"errors"
	"io"
	"testing"
)

// The SSH transport must satisfy every seam a transport can occupy. If an
// interface here grows a method sshclient.ExecSession does not implement, this
// file stops compiling — which is the point: the failure lands on the interface
// definition rather than on a distant call site.
var (
	_ shellChannel = (*fakeChannel)(nil)
	_ transport    = (*fakeChannel)(nil)
	_ ptyResizer   = (*fakeChannel)(nil)
)

// fakeChannel is a minimal channel used to pin the interface contract, in
// particular the parts a serial channel has to get right: no exit code, a
// non-nil stderr, and idempotent close.
type fakeChannel struct {
	closed    int
	closeErr  error
	stdout    io.Reader
	stderr    io.Reader
	done      chan struct{}
	aborted   bool
	exitCode  int
	written   []byte
	writeErr  error
	resizedTo [2]int
}

func newFakeChannel() *fakeChannel {
	return &fakeChannel{
		stdout:   emptyReader{},
		stderr:   emptyReader{},
		done:     make(chan struct{}),
		exitCode: -1,
	}
}

// emptyReader reports EOF immediately. Used for a serial channel's stderr, which
// must never be nil: the pipe goroutines call Read on whatever they are given.
type emptyReader struct{}

func (emptyReader) Read([]byte) (int, error) { return 0, io.EOF }

func (c *fakeChannel) WriteStdin(p []byte) (int, error) {
	if c.writeErr != nil {
		return 0, c.writeErr
	}
	c.written = append(c.written, p...)
	return len(p), nil
}

func (c *fakeChannel) StdoutReader() io.Reader { return c.stdout }
func (c *fakeChannel) StderrReader() io.Reader { return c.stderr }
func (c *fakeChannel) Done() <-chan struct{}   { return c.done }
func (c *fakeChannel) ExitCode() int           { return c.exitCode }
func (c *fakeChannel) Aborted() bool           { return c.aborted }
func (c *fakeChannel) CloseReaders()           {}

func (c *fakeChannel) CloseSessionOnly() error { return c.Close() }

// Close counts calls so a test can assert idempotence: Disconnect, finalize and
// the root shell's TerminateShell can each reach the same transport.
func (c *fakeChannel) Close() error {
	c.closed++
	return c.closeErr
}

func (c *fakeChannel) ResizePty(rows, cols int) error {
	c.resizedTo = [2]int{rows, cols}
	return nil
}

// A channel with no terminal must be reported as unsupported rather than
// accepted and ignored. The serial channel relies on this: it does not implement
// ptyResizer, so ResizePty has to fall through to ErrUnsupported.
//
// The method set matters here. A type that embeds a value with ResizePty would
// PROMOTE it and satisfy the probe — which is how a serial channel could
// accidentally claim to have a terminal. explicitChannel therefore implements
// shellChannel by hand and embeds nothing.
type explicitChannel struct {
	closed   int
	stdout   io.Reader
	stderr   io.Reader
	done     chan struct{}
	exitCode int
}

func newExplicitChannel() *explicitChannel {
	return &explicitChannel{
		stdout:   emptyReader{},
		stderr:   emptyReader{},
		done:     make(chan struct{}),
		exitCode: -1,
	}
}

func (c *explicitChannel) WriteStdin(p []byte) (int, error) { return len(p), nil }
func (c *explicitChannel) StdoutReader() io.Reader          { return c.stdout }
func (c *explicitChannel) StderrReader() io.Reader          { return c.stderr }
func (c *explicitChannel) Done() <-chan struct{}            { return c.done }
func (c *explicitChannel) ExitCode() int                    { return c.exitCode }
func (c *explicitChannel) Aborted() bool                    { return false }
func (c *explicitChannel) CloseReaders()                    {}
func (c *explicitChannel) CloseSessionOnly() error          { return c.Close() }
func (c *explicitChannel) Close() error                     { c.closed++; return nil }

func TestChannelWithoutPTYFailsOptionalProbes(t *testing.T) {
	var ch shellChannel = newExplicitChannel()

	if _, ok := ch.(ptyResizer); ok {
		t.Fatal("a channel declaring no ResizePty must not satisfy ptyResizer")
	}
	if _, ok := ch.(signaler); ok {
		t.Fatal("a channel declaring no Signal must not satisfy signaler")
	}
}

func TestPtyResizerProbeAcceptsChannelWithPTY(t *testing.T) {
	var ch shellChannel = newFakeChannel()

	resizer, ok := ch.(ptyResizer)
	if !ok {
		t.Fatal("a channel with ResizePty must satisfy ptyResizer")
	}
	if err := resizer.ResizePty(40, 120); err != nil {
		t.Fatalf("ResizePty: %v", err)
	}
	if got := ch.(*fakeChannel).resizedTo; got != [2]int{40, 120} {
		t.Fatalf("resized to %v, want [40 120]", got)
	}
}

// The signaler capability is what decides whether Terminate may wait out a grace
// period. A channel without it takes the direct-close path.
func TestSignalerProbeRejectsChannelWithoutSignal(t *testing.T) {
	var ch transport = newFakeChannel()

	if _, ok := ch.(signaler); ok {
		t.Fatal("a channel without Signal must not satisfy signaler")
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	ch := newFakeChannel()

	if err := ch.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := ch.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if ch.closed != 2 {
		t.Fatalf("Close called %d times, want 2 (both must be observed)", ch.closed)
	}
}

func TestClosePropagatesError(t *testing.T) {
	want := errors.New("port closed")
	ch := newFakeChannel()
	ch.closeErr = want

	if err := ch.Close(); !errors.Is(err, want) {
		t.Fatalf("Close() = %v, want %v", err, want)
	}
}

func TestErrUnsupportedIsASentinel(t *testing.T) {
	if !errors.Is(ErrUnsupported, ErrUnsupported) {
		t.Fatal("ErrUnsupported must match itself under errors.Is")
	}
}
