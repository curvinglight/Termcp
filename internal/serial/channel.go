package serial

import (
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"
	"unicode/utf8"

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
	spec   *Spec
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

	// The local-echo line state. Unlike the read path, which is stateless
	// because the device decides what a reply looks like, erasing what was typed
	// needs to know what is currently on screen — the transcript is append-only
	// and a device that does not echo will never send the rewrite an SSH shell
	// would. Guarded separately because it is per-keystroke hot and unrelated to
	// the lifecycle mutexes above.
	echoMu    sync.Mutex
	echoBuf   []byte // the bytes echoed for the line being edited
	echoCells int    // display cells echoBuf occupies
}

// Open opens the device for a profile and starts its read loop.
//
// The caller keeps ownership of the release function returned by Acquire; this
// function does not claim the device itself, so a caller that fails between the
// claim and the open can still release it.
func Open(spec *Spec, profileName string, readPoll time.Duration) (*Channel, error) {
	if spec == nil {
		return nil, errors.New("serial: nil spec")
	}
	if err := spec.Validate(); err != nil {
		return nil, err
	}

	handle, err := port.Open(spec.PortConfig(), readPoll)
	if err != nil {
		return nil, err
	}

	c := &Channel{
		handle:      handle,
		spec:        spec,
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
			if c.spec.NormalizeRx() {
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
			exists, err := port.DeviceExists(c.spec.Device)
			if err == nil && !exists {
				slog.Debug("serial device disappeared", "profile", c.name, "device", c.spec.Device)
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
	if c.spec.LocalEcho {
		c.echoLocal(data)
	}
	// The device always gets the bytes verbatim. 0x7f is a real backspace the
	// far side may act on; rewriting it for the display must never rewrite it
	// for the protocol.
	return c.handle.Write(data)
}

// echoLocal renders keystrokes into the transcript the way a terminal would:
// the bytes go in as typed, and the editing keys rewrite the line already on
// screen. It is a display of the input, not a second copy of it.
//
// Only bytes that draw a cell are remembered. Controls that draw nothing (ESC
// from an arrow key, say) are passed through untracked: counting them would
// make a later backspace erase a cell that was never drawn, which is the exact
// wrongness this function exists to remove.
func (c *Channel) echoLocal(data []byte) {
	c.echoMu.Lock()
	defer c.echoMu.Unlock()
	for i := 0; i < len(data); {
		b := data[i]
		switch {
		case b == '\r' || b == '\n':
			// Enter ends the line. What follows is the device's reply, not an
			// edit of what was typed, so the remembered line is dropped.
			c.stdout.Write(data[i : i+1])
			c.echoBuf = c.echoBuf[:0]
			c.echoCells = 0
			i++
		case b == 0x7f || b == 0x08:
			c.eraseEchoLast()
			i++
		case b == 0x15: // ctrl+u, kill the line
			c.eraseEchoCells(c.echoCells)
			c.echoBuf = c.echoBuf[:0]
			c.echoCells = 0
			i++
		case b == 0x17: // ctrl+w, kill the word before the cursor
			c.eraseEchoWord()
			i++
		case b < 0x20 || b == 0x7f:
			// Every other control draws no cell of its own; xterm ignores it
			// here, so the transcript gets the byte and the line state does not.
			c.stdout.Write(data[i : i+1])
			i++
		default:
			r, size := utf8.DecodeRune(data[i:])
			if r == utf8.RuneError && size <= 1 {
				// Not valid UTF-8 — an invalid byte or a sequence split across
				// two writes. Echo it as the single replacement cell the
				// terminal draws, so a backspace over it still erases one.
				size = 1
			}
			c.stdout.Write(data[i : i+size])
			c.echoBuf = append(c.echoBuf, data[i:i+size]...)
			c.echoCells += runeWidth(r)
			i += size
		}
	}
}

// eraseEchoLast pops the last rune from the remembered line and erases the
// cells it occupied.
func (c *Channel) eraseEchoLast() {
	if len(c.echoBuf) == 0 {
		return
	}
	r, size := utf8.DecodeLastRune(c.echoBuf)
	c.echoBuf = c.echoBuf[:len(c.echoBuf)-size]
	w := runeWidth(r)
	c.eraseEchoCells(w)
	c.echoCells -= w
	if c.echoCells < 0 {
		c.echoCells = 0
	}
}

// eraseEchoWord drops trailing spaces and then the word before them, erasing
// exactly the cells that disappear — so ctrl+w after a space removes the
// previous word and only that, the way a shell does.
func (c *Channel) eraseEchoWord() {
	n := len(c.echoBuf)
	for n > 0 && (c.echoBuf[n-1] == ' ' || c.echoBuf[n-1] == '\t') {
		n--
	}
	for n > 0 && c.echoBuf[n-1] != ' ' && c.echoBuf[n-1] != '\t' {
		n--
	}
	w := echoWidth(c.echoBuf[n:])
	if w == 0 {
		return
	}
	c.echoBuf = c.echoBuf[:n]
	c.eraseEchoCells(w)
	c.echoCells -= w
	if c.echoCells < 0 {
		c.echoCells = 0
	}
}

// eraseEchoCells writes the sequence every terminal emits to rub out n cells:
// back over them, blank them, back again. n is the cell count, not the byte
// count, so a wide character erases as the two cells it was drawn as.
func (c *Channel) eraseEchoCells(n int) {
	if n <= 0 {
		return
	}
	var seq []byte
	for i := 0; i < n; i++ {
		seq = append(seq, '\b')
	}
	for i := 0; i < n; i++ {
		seq = append(seq, ' ')
	}
	for i := 0; i < n; i++ {
		seq = append(seq, '\b')
	}
	c.stdout.Write(seq)
}

// echoWidth is the cell count of bytes already echoed, decoding them the same
// way echoLocal did so the two never disagree about what is on screen.
func echoWidth(s []byte) int {
	w := 0
	for len(s) > 0 {
		r, size := utf8.DecodeRune(s)
		if r == utf8.RuneError && size <= 1 {
			w++
			s = s[1:]
			continue
		}
		w += runeWidth(r)
		s = s[size:]
	}
	return w
}

// wideRanges are the East Asian Wide and Fullwidth blocks plus the emoji
// blocks that terminals draw as two cells. The module deliberately carries no
// text-width dependency, and an erase must count the same cells the terminal
// drew, so the ranges are listed rather than derived from a table that would
// arrive as a new transitive dependency for one display fix.
var wideRanges = [][2]rune{
	{0x1100, 0x115F}, {0x2E80, 0x303E}, {0x3041, 0x33FF}, {0x3400, 0x4DBF},
	{0x4E00, 0x9FFF}, {0xA000, 0xA4CF}, {0xA960, 0xA97F}, {0xAC00, 0xD7A3},
	{0xF900, 0xFAFF}, {0xFE10, 0xFE19}, {0xFE30, 0xFE6F}, {0xFF00, 0xFF60},
	{0xFFE0, 0xFFE6}, {0x1F300, 0x1F64F}, {0x1F680, 0x1F6FF}, {0x1F900, 0x1F9FF},
	{0x20000, 0x2FFFD}, {0x30000, 0x3FFFD},
}

// runeWidth returns how many cells r occupies: two for the wide and fullwidth
// ranges, one for everything else, which is what a terminal does with them.
func runeWidth(r rune) int {
	for _, rg := range wideRanges {
		if r >= rg[0] && r <= rg[1] {
			return 2
		}
	}
	return 1
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

// Device returns the device path as the profile named it, for session_info.
//
// It is deliberately not the canonical path. Canonicalisation resolves a by-id
// alias to the tty it points at, which is what the claim gate needs — two names
// for one device must collide — and what a person reading the session record
// does not: the by-id path is the stable identity the profile was written with,
// it is what DeviceExists probes, and it is the string the Web UI badge
// (FT232R @115200) is derived from. port.CanonicalDevice still runs wherever
// identity is the question.
func (c *Channel) Device() string { return c.spec.Device }

// Baud returns the configured rate, for session_info.
func (c *Channel) Baud() int { return c.spec.Baud }

// ProfileName returns the profile this channel was opened from.
func (c *Channel) ProfileName() string { return c.name }

// emptyReader reports EOF immediately.
type emptyReader struct{}

func (emptyReader) Read([]byte) (int, error) { return 0, io.EOF }
