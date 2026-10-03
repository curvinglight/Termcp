package serial

import (
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/open-mcp-ai/termcp/internal/serial/port"
)

// Poll and retry policy for the read loop. These are internal constants rather
// than profile fields: they describe how the loop behaves, not how the device is
// configured.
const (
	// internalReadPoll bounds a single Read so the loop regains control often
	// enough to notice a closed port.
	internalReadPoll = 50 * time.Millisecond

	// deviceCheckInterval is how often an idle device is probed for existence.
	// Checking on every empty read would be a stat per 50ms for no benefit; a
	// second is fast enough to notice a cable being pulled.
	deviceCheckInterval = time.Second

	// ioErrorBackoff delays the retry after a failed read, so a device that
	// fails instantly does not spin.
	ioErrorBackoff = 100 * time.Millisecond

	// maxConsecutiveIOErrors is the point at which repeated failures are treated
	// as a lost device rather than a transient glitch.
	maxConsecutiveIOErrors = 3
)

// Channel is one serial device exposed as a shell channel and as a session
// transport. A serial session has exactly one, and it is both: the container
// needs the lifecycle methods, and the root shell needs the byte stream.
//
// It wraps a port.Handle and owns the read loop that feeds the stdout reader. The
// loop exists because the session layer consumes an io.Reader and expects a
// blocking read to mean "no data yet is not an error"; a serial Read returning
// (0, nil) on an idle device would spin that consumer at full speed.
type Channel struct {
	handle  port.Handle
	profile *Entry
	name    string

	// stdout is the reader handed to the session. It is fed by the read loop
	// rather than being the port itself, so empty reads and normalization happen
	// before the session sees anything.
	stdout *pipeReader

	done      chan struct{}
	closeOnce sync.Once
	closeErr  error

	// aborted reports transport loss (device disappeared) as opposed to a
	// deliberate close, so the session can tell "cable pulled" from "user closed
	// the tab" when deciding whether to mark itself DEAD.
	aborted   bool
	abortedMu sync.Mutex

	// lastErr carries the most recent read failure for diagnostics.
	lastErr   error
	lastErrMu sync.Mutex

	// readerClosed lets CloseReaders unblock a session reader without ending the
	// read loop, mirroring the SSH transport's behaviour.
	readersOnce sync.Once
	readersDone chan struct{}
}

// Open opens the device for a profile and starts its read loop.
//
// The caller keeps ownership of the release function returned by Acquire; this
// function does not claim the device itself, so a caller that fails between the
// claim and the open can still release it.
func Open(profile *Entry, profileName string, readPoll time.Duration) (*Channel, error) {
	if profile == nil {
		return nil, errors.New("serial: nil profile")
	}
	if err := profile.Validate(); err != nil {
		return nil, err
	}

	handle, err := port.Open(profile.PortConfig(), readPoll)
	if err != nil {
		return nil, err
	}

	c := &Channel{
		handle:      handle,
		profile:     profile,
		name:        profileName,
		stdout:      newPipeReader(),
		done:        make(chan struct{}),
		readersDone: make(chan struct{}),
	}
	go c.readLoop()
	return c, nil
}

// readLoop pumps the device into the stdout reader until the port closes or the
// device goes away.
//
// It never hands an empty read to the session: an idle device produces no bytes,
// and forwarding (0, nil) would make the session's copy loop spin. Idle time is
// spent here instead, where the interval is known.
func (c *Channel) readLoop() {
	defer c.stdout.Close()

	normalizer := &rxNormalizer{}
	buf := make([]byte, 4096)

	lastDeviceCheck := time.Now()
	consecutiveErrors := 0

	for {
		n, err := c.handle.Read(buf)

		if n > 0 {
			consecutiveErrors = 0
			chunk := buf[:n]
			if c.profile.NormalizeRx() {
				chunk = normalizer.Transform(chunk)
			}
			if len(chunk) > 0 {
				c.stdout.Write(chunk)
			}
			lastDeviceCheck = time.Now()
			continue
		}

		if err != nil {
			// A close that this process performed is not transport loss, whatever
			// error the port happens to return for a read on a closed descriptor.
			// The classification is driven by c.closing, not by the error text:
			// inferring it from the message would make "user closed the tab"
			// depend on a driver's wording, and report a device failure for a
			// deliberate close.
			if c.isClosed() {
				c.finish(false)
				return
			}

			// The device is gone: the read failed and nobody asked it to stop.
			if errors.Is(err, port.ErrDeviceGone) {
				c.finish(true)
				return
			}

			consecutiveErrors++
			c.setLastErr(err)
			slog.Debug("serial read failed", "profile", c.name, "err", err,
				"consecutive", consecutiveErrors)

			if consecutiveErrors >= maxConsecutiveIOErrors {
				// Repeated failures mean the device is unusable; retrying forever
				// would leave a session that looks alive and never produces output.
				c.finish(true)
				return
			}
			select {
			case <-c.done:
				c.finish(false)
				return
			case <-time.After(ioErrorBackoff):
			}
			continue
		}

		// n == 0, err == nil: an idle device. This is the normal case for an
		// embedded console, so it must not end anything. Periodically confirm the
		// device is still there — a USB adapter that is unplugged can leave the
		// read returning empty forever rather than failing.
		if time.Since(lastDeviceCheck) >= deviceCheckInterval {
			lastDeviceCheck = time.Now()
			// A claimed device that no longer resolves is gone. The port may
			// still open a stale node, so existence is checked on the path the
			// profile named rather than on the open handle.
			exists, err := port.DeviceExists(c.profile.Device)
			if err == nil && !exists {
				slog.Debug("serial device disappeared", "profile", c.name, "device", c.profile.Device)
				c.finish(true)
				return
			}
		}

		select {
		case <-c.done:
			c.finish(false)
			return
		default:
		}
	}
}

// finish records how the channel ended, unblocks readers, and closes Done.
func (c *Channel) finish(aborted bool) {
	if aborted {
		c.setAborted()
	}
	// Unblock a session reader waiting on stdout, then signal Done.
	c.stdout.Close()
	c.closeOnce.Do(func() { close(c.done) })
}

// isClosed reports whether the channel has been closed, either deliberately or
// because it finished.
func (c *Channel) isClosed() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

func (c *Channel) setAborted() {
	c.abortedMu.Lock()
	c.aborted = true
	c.abortedMu.Unlock()
}

func (c *Channel) setLastErr(err error) {
	c.lastErrMu.Lock()
	c.lastErr = err
	c.lastErrMu.Unlock()
}

// LastError returns the most recent read failure, or nil.
func (c *Channel) LastError() error {
	c.lastErrMu.Lock()
	defer c.lastErrMu.Unlock()
	return c.lastErr
}

// --- shellChannel ---

// WriteStdin writes to the device.
//
// press_enter is handled by the caller: the session applies replaceEnter before
// calling this, because the line ending is a profile property and this method
// only sees bytes.
func (c *Channel) WriteStdin(data []byte) (int, error) {
	if c.isClosed() {
		return 0, io.ErrClosedPipe
	}
	if len(data) == 0 {
		return 0, nil
	}
	// Local echo writes the bytes into the transcript as well, for devices that
	// do not echo their input: without it the transcript shows replies to
	// commands it never shows anyone typing.
	if c.profile.LocalEcho {
		c.stdout.Write(data)
	}
	return c.handle.Write(data)
}

// StdoutReader returns the device input stream.
func (c *Channel) StdoutReader() io.Reader { return c.stdout }

// StderrReader returns an empty reader. A serial device has one byte stream and
// no stderr; the session's pipe goroutines require a real reader, never nil.
func (c *Channel) StderrReader() io.Reader { return emptyReader{} }

// Done closes when the device is closed or lost.
func (c *Channel) Done() <-chan struct{} { return c.done }

// ExitCode is -1: a serial device has no process and no exit status.
func (c *Channel) ExitCode() int { return -1 }

// Aborted reports whether the channel ended because the device disappeared.
func (c *Channel) Aborted() bool {
	c.abortedMu.Lock()
	defer c.abortedMu.Unlock()
	return c.aborted
}

// CloseReaders releases the session's read side without ending the channel. The
// read loop keeps running so a later write still works, matching the SSH
// transport where this only detaches the readers.
func (c *Channel) CloseReaders() {
	c.readersOnce.Do(func() { close(c.readersDone) })
}

// CloseSessionOnly is Close: a serial channel has no shared connection to leave
// running, so there is nothing narrower to close.
func (c *Channel) CloseSessionOnly() error { return c.Close() }

// Close releases the port. It is idempotent, because Disconnect, finalize and
// the root shell's close can each reach the same channel and a serial descriptor
// is not safe to close twice.
func (c *Channel) Close() error {
	c.closeOnce.Do(func() {
		c.closeErr = c.handle.Close()
		// Closing the port is what unblocks a read in progress; the loop then
		// exits and closes Done.
		c.stdout.Close()
		close(c.done)
	})
	return c.closeErr
}

// --- serialController (optional capability, probed by the MCP layer) ---

// SetDTR drives the DTR line.
func (c *Channel) SetDTR(on bool) error { return c.handle.SetDTR(on) }

// SetRTS drives the RTS line.
func (c *Channel) SetRTS(on bool) error { return c.handle.SetRTS(on) }

// Break holds the line in the break state for d.
func (c *Channel) Break(d time.Duration) error { return c.handle.Break(d) }

// FlushInput discards buffered device output.
func (c *Channel) FlushInput() error { return c.handle.FlushInput() }

// FlushOutput discards buffered host output.
func (c *Channel) FlushOutput() error { return c.handle.FlushOutput() }

// ModemStatus reports the input control lines.
func (c *Channel) ModemStatus() (CTS, DSR, RI, DCD bool, err error) {
	return c.handle.ModemStatus()
}

// Device returns the canonical device path, for session_info and logging.
func (c *Channel) Device() string { return port.CanonicalDevice(c.profile.Device) }

// Baud returns the configured rate, for session_info.
func (c *Channel) Baud() int { return c.profile.Baud }

// ProfileName returns the profile this channel was opened from.
func (c *Channel) ProfileName() string { return c.name }

// emptyReader reports EOF immediately.
type emptyReader struct{}

func (emptyReader) Read([]byte) (int, error) { return 0, io.EOF }
