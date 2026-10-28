package port

import (
	"os"
	"sync"
	"testing"
	"time"

	"go.bug.st/serial"
)

// writeFile is a test helper for creating a file at an exact path (the tests
// build paths themselves to control symlink targets).
func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}

// symlink is a test helper so config_test.go does not import os just for this.
func symlink(oldname, newname string) error {
	return os.Symlink(oldname, newname)
}

// fakeHandle is an in-memory Handle used to pin the wrapper's behaviour without
// a real device. It records calls and lets a test inject errors.
type fakeHandle struct {
	mu sync.Mutex

	readData  []byte
	readErr   error
	readDelay time.Duration
	reads     int

	writes   [][]byte
	writeErr error

	dtr, rts     bool
	dtrCalls     int
	rtsCalls     int
	breakCalls   int
	breakFor     time.Duration
	flushedIn    int
	flushedOut   int
	drainCalls   int
	timeoutSet   time.Duration
	timeoutCalls int
	setModeCalls int
	lastMode     *serial.Mode

	modemStatus *modemBits
	modemErr    error

	closeCalls int
	closeErr   error
}

type modemBits struct {
	cts, dsr, ri, dcd bool
}

func (h *fakeHandle) Read(p []byte) (int, error) {
	h.mu.Lock()
	h.reads++
	delay := h.readDelay
	h.mu.Unlock()

	if delay > 0 {
		time.Sleep(delay)
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.readErr != nil {
		return 0, h.readErr
	}
	if len(h.readData) == 0 {
		return 0, nil
	}
	n := copy(p, h.readData)
	h.readData = h.readData[n:]
	return n, nil
}

func (h *fakeHandle) Write(p []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.writeErr != nil {
		return 0, h.writeErr
	}
	cp := make([]byte, len(p))
	copy(cp, p)
	h.writes = append(h.writes, cp)
	return len(p), nil
}

func (h *fakeHandle) SetDTR(on bool) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.dtr, h.dtrCalls = on, h.dtrCalls+1
	return nil
}

func (h *fakeHandle) SetRTS(on bool) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.rts, h.rtsCalls = on, h.rtsCalls+1
	return nil
}

func (h *fakeHandle) Break(d time.Duration) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.breakCalls++
	h.breakFor = d
	return nil
}

func (h *fakeHandle) ModemStatus() (bool, bool, bool, bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.modemErr != nil {
		return false, false, false, false, h.modemErr
	}
	if h.modemStatus == nil {
		return false, false, false, false, nil
	}
	return h.modemStatus.cts, h.modemStatus.dsr, h.modemStatus.ri, h.modemStatus.dcd, nil
}

// GetModemStatusBits is the library-shaped accessor the wrapper reads. It
// returns nil bits when the fake has none, which is the case the wrapper has to
// guard against.
func (h *fakeHandle) GetModemStatusBits() (*serial.ModemStatusBits, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.modemErr != nil {
		return nil, h.modemErr
	}
	if h.modemStatus == nil {
		return nil, nil
	}
	return &serial.ModemStatusBits{
		CTS: h.modemStatus.cts,
		DSR: h.modemStatus.dsr,
		RI:  h.modemStatus.ri,
		DCD: h.modemStatus.dcd,
	}, nil
}

// SetMode is part of serial.Port. The wrapper never changes the mode after open,
// so this only records the call.
func (h *fakeHandle) SetMode(mode *serial.Mode) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.setModeCalls++
	h.lastMode = mode
	return nil
}

func (h *fakeHandle) SetReadTimeout(d time.Duration) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.timeoutCalls++
	h.timeoutSet = d
	return nil
}

// ResetInputBuffer/ResetOutputBuffer are the library's names for what our Handle
// calls FlushInput/FlushOutput. The fake implements the library interface, so it
// has to use the library spellings; the wrapper is what translates between them.
func (h *fakeHandle) ResetInputBuffer() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.flushedIn++
	return nil
}

func (h *fakeHandle) ResetOutputBuffer() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.flushedOut++
	return nil
}

// Drain waits for buffered output to be sent. It is part of serial.Port.
func (h *fakeHandle) Drain() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.drainCalls++
	return nil
}

func (h *fakeHandle) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closeCalls++
	return h.closeErr
}

// fakeHandle stands in for the LIBRARY port, not for our Handle: the wrapper is
// what turns one into the other. Keeping the assertion on serial.Port means a
// library method rename breaks the build here, next to the code it affects,
// rather than in a distant call site.
var _ serial.Port = (*fakeHandle)(nil)

// requireNoError fails the test with context if err is non-nil.
func requireNoError(t *testing.T, err error, format string, args ...any) {
	t.Helper()
	if err != nil {
		t.Fatalf(format+": %v", append(args, err)...)
	}
}
