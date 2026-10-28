package serial

import (
	"sync"
	"time"

	"github.com/open-mcp-ai/termcp/internal/serial/port"
)

// fakePort stands in for port.Handle so the channel can be exercised without a
// device. It records calls and lets a test drive the read side.
type fakePort struct {
	mu sync.Mutex

	writes     []byte
	writeErr   error
	readCh     chan []byte
	readErr    error
	readDelay  time.Duration
	closeCount int
	closeErr   error

	dtr, rts   bool
	breakCalls int
	breakFor   time.Duration
	flushIn    int
	flushOut   int
	modemCalls int
}

func newFakePort() *fakePort {
	return &fakePort{readCh: make(chan []byte, 16)}
}

func (f *fakePort) Read(p []byte) (int, error) {
	f.mu.Lock()
	delay := f.readDelay
	f.mu.Unlock()

	if delay > 0 {
		time.Sleep(delay)
	}

	select {
	case data, ok := <-f.readCh:
		if !ok {
			return 0, port.ErrDeviceGone
		}
		return copy(p, data), nil
	case <-time.After(10 * time.Millisecond):
		// Idle: no bytes and no error, which is what a real serial read does on
		// a silent device.
		return 0, nil
	}
}

func (f *fakePort) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	f.writes = append(f.writes, p...)
	return len(p), nil
}

func (f *fakePort) SetDTR(on bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dtr = on
	return nil
}

func (f *fakePort) SetRTS(on bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rts = on
	return nil
}

func (f *fakePort) Break(d time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.breakCalls++
	f.breakFor = d
	return nil
}

func (f *fakePort) ModemStatus() (bool, bool, bool, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.modemCalls++
	return true, false, false, false, nil
}

func (f *fakePort) SetReadTimeout(time.Duration) error { return nil }

func (f *fakePort) FlushInput() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.flushIn++
	return nil
}

func (f *fakePort) FlushOutput() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.flushOut++
	return nil
}

func (f *fakePort) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeCount++
	return f.closeErr
}

// feed queues bytes for the next reads.
func (f *fakePort) feed(b []byte) {
	cp := make([]byte, len(b))
	copy(cp, b)
	f.readCh <- cp
}

// disconnect makes the next read report the device gone.
func (f *fakePort) disconnect() { close(f.readCh) }

func (f *fakePort) closeCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closeCount
}

func (f *fakePort) written() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]byte, len(f.writes))
	copy(out, f.writes)
	return out
}

var _ port.Handle = (*fakePort)(nil)

// failingPort always fails its reads with a plain error, standing in for a
// driver that reports a generic failure rather than ErrDeviceGone.
type failingPort struct {
	fakePort
	err error
}

func (f *failingPort) Read([]byte) (int, error) { return 0, f.err }

var _ port.Handle = (*failingPort)(nil)
