package serial

import (
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

// newTestChannel wires a Channel around a fake port without going through Open,
// so the read loop can be driven directly.
func newTestChannel(p *fakePort, spec *Spec) *Channel {
	c := &Channel{
		handle:      p,
		spec:        spec,
		name:        "test",
		stdout:      newPipeReader(),
		done:        make(chan struct{}),
		readersDone: make(chan struct{}),
	}
	go c.readLoop()
	return c
}

// readWithTimeout reads once, failing the test rather than hanging.
func readWithTimeout(t *testing.T, r io.Reader, d time.Duration) []byte {
	t.Helper()
	type result struct {
		b   []byte
		err error
	}
	ch := make(chan result, 1)
	go func() {
		buf := make([]byte, 4096)
		n, err := r.Read(buf)
		ch <- result{buf[:n], err}
	}()

	select {
	case res := <-ch:
		if res.err != nil && res.err != io.EOF {
			t.Fatalf("read: %v", res.err)
		}
		return res.b
	case <-time.After(d):
		t.Fatal("timed out waiting for output")
		return nil
	}
}

func TestReadLoopForwardsDeviceOutput(t *testing.T) {
	p := newFakePort()
	c := newTestChannel(p, &Spec{})
	defer c.Close()

	p.feed([]byte("hello"))

	got := readWithTimeout(t, c.StdoutReader(), 2*time.Second)
	if string(got) != "hello" {
		t.Fatalf("got %q, want %q", got, "hello")
	}
}

func TestReadLoopNormalizesLoneLF(t *testing.T) {
	// The whole reason rx_normalize exists: a device sends "ok\n" and the
	// terminal needs "ok\r\n" or the cursor stays in the same column.
	p := newFakePort()
	c := newTestChannel(p, &Spec{RxNormalize: boolPtr(true)})
	defer c.Close()

	p.feed([]byte("ok\n"))

	got := readWithTimeout(t, c.StdoutReader(), 2*time.Second)
	if string(got) != "ok\r\n" {
		t.Fatalf("got %q, want %q", got, "ok\r\n")
	}
}

func TestReadLoopPreservesCRLF(t *testing.T) {
	// A device that already sends CRLF must not get a second CR.
	p := newFakePort()
	c := newTestChannel(p, &Spec{RxNormalize: boolPtr(true)})
	defer c.Close()

	p.feed([]byte("a\r\nb"))

	got := readWithTimeout(t, c.StdoutReader(), 2*time.Second)
	if string(got) != "a\r\nb" {
		t.Fatalf("got %q, want %q", got, "a\r\nb")
	}
}

func TestReadLoopNormalizationAcrossChunks(t *testing.T) {
	// CR and LF split across two reads is one line ending. Converting each chunk
	// independently would double the CR or drop it.
	p := newFakePort()
	c := newTestChannel(p, &Spec{RxNormalize: boolPtr(true)})
	defer c.Close()

	p.feed([]byte("line\r"))
	p.feed([]byte("\nnext"))

	// Read until both chunks have been through the normalizer.
	var got []byte
	deadline := time.After(2 * time.Second)
	for len(got) < len("line\r\nnext") {
		select {
		case <-deadline:
			t.Fatalf("timed out; got %q", got)
		default:
		}
		got = append(got, readWithTimeout(t, c.StdoutReader(), time.Second)...)
	}

	if string(got) != "line\r\nnext" {
		t.Fatalf("got %q, want %q", got, "line\r\nnext")
	}
}

func TestReadLoopLeftAloneWhenNormalizationOff(t *testing.T) {
	p := newFakePort()
	c := newTestChannel(p, &Spec{RxNormalize: boolPtr(false)})
	defer c.Close()

	p.feed([]byte("ok\n"))

	got := readWithTimeout(t, c.StdoutReader(), 2*time.Second)
	if string(got) != "ok\n" {
		t.Fatalf("got %q, want the bytes unchanged (%q)", got, "ok\n")
	}
}

func TestReadLoopNeverReturnsEmptyRead(t *testing.T) {
	// An idle device produces (0, nil). Forwarding that would spin the session's
	// copy loop, so the read loop must absorb it and block the reader instead.
	p := newFakePort()
	c := newTestChannel(p, &Spec{})
	defer c.Close()

	readStarted := make(chan struct{})
	readDone := make(chan struct{})
	go func() {
		close(readStarted)
		c.StdoutReader().Read(make([]byte, 64)) // blocks: nothing to read
		close(readDone)
	}()

	<-readStarted
	select {
	case <-readDone:
		t.Fatal("a read returned with no data available; it must block")
	case <-time.After(300 * time.Millisecond):
		// Still blocked, which is correct.
	}
}

func TestReadLoopMarksAbortedOnDeviceGone(t *testing.T) {
	// A vanished device is transport loss: the session uses this to distinguish
	// "cable pulled" from "user closed the tab".
	p := newFakePort()
	c := newTestChannel(p, &Spec{})
	defer c.Close()

	p.disconnect()

	select {
	case <-c.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Done should close when the device goes away")
	}

	if !c.Aborted() {
		t.Fatal("a device that disappears must be reported as aborted")
	}
}

func TestReadLoopClosesStdoutOnDeviceGone(t *testing.T) {
	// The session's reader has to see EOF, or drainPipes waits out its grace
	// period on every device loss.
	p := newFakePort()
	c := newTestChannel(p, &Spec{})
	defer c.Close()

	p.disconnect()

	select {
	case <-c.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Done should close")
	}

	readDone := make(chan error, 1)
	go func() {
		_, err := c.StdoutReader().Read(make([]byte, 64))
		readDone <- err
	}()

	select {
	case err := <-readDone:
		if err != io.EOF {
			t.Fatalf("read after device loss = %v, want io.EOF", err)
		}
	case <-time.After(time.Second):
		t.Fatal("the reader should see EOF once the device is gone")
	}
}

func TestCloseEndsReadLoopWithoutAborting(t *testing.T) {
	// A deliberate close is not transport loss: marking it aborted would make
	// the session report a device failure for a tab the user closed.
	p := newFakePort()
	c := newTestChannel(p, &Spec{})

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	select {
	case <-c.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Done should close after Close")
	}

	if c.Aborted() {
		t.Fatal("a deliberate close must not be reported as aborted")
	}
}

func TestDeliberateCloseWinsOverFailingReads(t *testing.T) {
	// The classification must come from "did we close it", not from the error a
	// read happens to return on a closed descriptor. A driver that reports a
	// bare error rather than ErrDeviceGone must not turn a tab the user closed
	// into a device failure.
	//
	// The close happens before the failure threshold, which is the case that
	// matters: once the loop has already concluded the device is gone, that
	// conclusion stands. Up to that point a close is still the user's decision.
	p := &failingPort{err: errors.New("i/o error")}
	c := &Channel{
		handle:      p,
		spec:        &Spec{},
		name:        "test",
		stdout:      newPipeReader(),
		done:        make(chan struct{}),
		readersDone: make(chan struct{}),
	}
	go c.readLoop()

	// Well inside the threshold (3 failures over ~300ms), so the loop has seen
	// errors but has not yet given up on the device.
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	select {
	case <-c.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Close should close Done")
	}

	if c.Aborted() {
		t.Fatal("closing a channel whose reads were failing must still not report aborted")
	}
}

func TestRepeatedReadFailuresReportAborted(t *testing.T) {
	// The other side of the same rule: if nobody closed the channel and reads
	// keep failing, the device is gone and the session has to be told, or it
	// would look alive forever while producing nothing.
	p := &failingPort{err: errors.New("i/o error")}
	c := &Channel{
		handle:      p,
		spec:        &Spec{},
		name:        "test",
		stdout:      newPipeReader(),
		done:        make(chan struct{}),
		readersDone: make(chan struct{}),
	}
	defer c.Close()
	go c.readLoop()

	select {
	case <-c.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("repeated read failures should end the channel")
	}

	if !c.Aborted() {
		t.Fatal("repeated read failures with no close should be reported as aborted")
	}
}

func TestConcurrentReadsAndClose(t *testing.T) {
	// Close must be safe while readers are blocked, and must wake them.
	p := newFakePort()
	c := newTestChannel(p, &Spec{})

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.StdoutReader().Read(make([]byte, 64))
		}()
	}

	time.Sleep(50 * time.Millisecond)
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close should wake every blocked reader")
	}
}

func TestDeviceDisappearanceDetectedByExistenceCheck(t *testing.T) {
	// A USB adapter can be unplugged leaving reads returning empty forever.
	// The periodic existence check catches that; without it the session would
	// look alive and never produce output, and never end.
	//
	// The device path here does not exist, so the check succeeds in reporting it
	// gone. It is a real path check on the host, which is the point.
	p := newFakePort()
	c := newTestChannel(p, &Spec{Device: "/dev/ttyUSB-does-not-exist-99"})
	defer c.Close()

	select {
	case <-c.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("a device that does not exist should end the channel")
	}
}

func boolPtr(b bool) *bool { return &b }
