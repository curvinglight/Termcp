package port

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"go.bug.st/serial"
)

// DefaultReadPoll bounds a single Read. The port layer owns this rather than a
// profile: it is not a user preference but the interval at which the read loop
// regains control, so Close can interrupt it and a device that vanishes can be
// noticed. A value that is too large delays DEAD detection; too small burns CPU
// on wakeups that find nothing.
const DefaultReadPoll = 50 * time.Millisecond

// Open opens the device described by cfg and bounds each Read to readPoll.
//
// A zero readPoll uses DefaultReadPoll. The returned Handle is closed idempotently
// (see handle.Close), because the session's Disconnect, finalize and root-shell
// close can each reach it.
//
// Validation runs first, so a bad config is reported as ErrInvalidConfig rather
// than as a device error. Errors from the library are normalized: the caller
// gets ErrBusy, ErrNotFound, ErrPermission, ErrDeviceGone or
// ErrUnsupportedFormat, never the library's message.
func Open(cfg Config, readPoll time.Duration) (Handle, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if readPoll == 0 {
		readPoll = DefaultReadPoll
	}

	mode, err := cfg.toLibraryMode()
	if err != nil {
		return nil, err
	}

	p, err := serial.Open(cfg.Device, mode)
	if err != nil {
		return nil, normalizeOpenError(err)
	}

	// Bound each Read. Without this a Read blocks until at least one byte
	// arrives, so a read loop on an idle console would never regain control and
	// Close could not make it exit.
	if err := p.SetReadTimeout(readPoll); err != nil {
		_ = p.Close()
		return nil, normalizeIOError(err)
	}

	return &handle{port: p}, nil
}

// sentinelForOpenCode maps a library error code to this package's sentinel for
// the open path.
//
// It is a separate pure function because serial.PortError has no exported
// constructor (its code field is unexported), so a test cannot fabricate one to
// drive normalizeOpenError end to end. Testing the mapping here covers the part
// that can be wrong; the wrapping around it is exercised by describing a real
// device failure.
//
// A code this package does not recognize falls to ErrUnsupportedFormat: the open
// failed for a reason other than busy/not-found/permission, which is what the
// MCP layer reports as an unsupported request.
func sentinelForOpenCode(code serial.PortErrorCode) error {
	switch code {
	case serial.PortBusy:
		return ErrBusy
	case serial.PortNotFound:
		return ErrNotFound
	case serial.PermissionDenied:
		return ErrPermission
	case serial.PortClosed:
		return ErrDeviceGone
	case serial.InvalidSpeed, serial.InvalidDataBits, serial.InvalidParity,
		serial.InvalidStopBits, serial.InvalidSerialPort, serial.InvalidTimeoutValue,
		serial.FunctionNotImplemented, serial.ErrorEnumeratingPorts:
		return ErrUnsupportedFormat
	default:
		return ErrUnsupportedFormat
	}
}

// sentinelForIOCode maps a library error code to this package's sentinel for an
// operation on an already-open port.
//
// It differs from the open mapping on purpose: an I/O failure that is not one of
// these is left alone (see normalizeIOError), because reclassifying an unknown
// driver error would hide the real cause from the log.
func sentinelForIOCode(code serial.PortErrorCode) (error, bool) {
	switch code {
	case serial.PortBusy:
		return ErrBusy, true
	case serial.PortNotFound:
		return ErrNotFound, true
	case serial.PermissionDenied:
		return ErrPermission, true
	case serial.PortClosed:
		return ErrDeviceGone, true
	default:
		return nil, false
	}
}

// normalizeOpenError maps a library open failure onto this package's errors.
//
// PortError carries its code in an unexported field, so it is read through
// Code() rather than by matching text.
func normalizeOpenError(err error) error {
	var perr serial.PortError
	if errors.As(err, &perr) {
		return fmt.Errorf("%w: %s", sentinelForOpenCode(perr.Code()), err)
	}
	return fmt.Errorf("%w: %s", ErrUnsupportedFormat, err)
}

// normalizeIOError maps a failure on an already-open port.
//
// The distinction that matters is "the device is gone" versus "this one
// operation failed": only the former ends the session. A closed port reports
// PortClosed, which is a deliberate close rather than a vanished device, so it
// also lands on ErrDeviceGone — the session is over either way, and the caller
// distinguishes deliberate closes by its own flag, not by this error.
func normalizeIOError(err error) error {
	if err == nil {
		return nil
	}
	var perr serial.PortError
	if errors.As(err, &perr) {
		if sentinel, ok := sentinelForIOCode(perr.Code()); ok {
			return fmt.Errorf("%w: %s", sentinel, err)
		}
	}
	// A modem-line or break request the device cannot perform comes back as a
	// bare ENOTTY ("inappropriate ioctl for device"), not as a PortError: the
	// library passes the syscall error straight through for SetDTR/SetRTS/Break.
	// It means the operation is not available here, which is what
	// ErrUnsupportedFormat says, and it keeps the raw errno text out of the
	// caller's error code.
	if isUnsupportedIoctl(err) {
		return fmt.Errorf("%w: this device does not support the requested operation", ErrUnsupportedFormat)
	}
	return err
}

// handle wraps the library port. It exists to keep go.bug.st/serial out of the
// rest of the program and to make Close idempotent, which the library does not
// promise.
type handle struct {
	port serial.Port

	// closeOnce guards the library Close. Three session paths can reach it, and
	// a double Close on a serial file descriptor is not safe to assume away.
	closeOnce sync.Once
	closeErr  error
}

func (h *handle) Read(p []byte) (int, error) {
	n, err := h.port.Read(p)
	if err != nil {
		return n, normalizeIOError(err)
	}
	return n, nil
}

func (h *handle) Write(p []byte) (int, error) {
	n, err := h.port.Write(p)
	if err != nil {
		return n, normalizeIOError(err)
	}
	return n, nil
}

func (h *handle) SetDTR(on bool) error { return normalizeIOError(h.port.SetDTR(on)) }
func (h *handle) SetRTS(on bool) error { return normalizeIOError(h.port.SetRTS(on)) }
func (h *handle) Break(d time.Duration) error {
	return normalizeIOError(h.port.Break(d))
}

func (h *handle) SetReadTimeout(d time.Duration) error {
	return normalizeIOError(h.port.SetReadTimeout(d))
}

func (h *handle) FlushInput() error  { return normalizeIOError(h.port.ResetInputBuffer()) }
func (h *handle) FlushOutput() error { return normalizeIOError(h.port.ResetOutputBuffer()) }

// ModemStatus reports the input control lines. GetModemStatusBits returns a
// pointer, so a nil result with no error is treated as a library fault rather
// than dereferenced.
func (h *handle) ModemStatus() (CTS, DSR, RI, DCD bool, err error) {
	bits, err := h.port.GetModemStatusBits()
	if err != nil {
		return false, false, false, false, normalizeIOError(err)
	}
	if bits == nil {
		return false, false, false, false, fmt.Errorf("serial: modem status unavailable")
	}
	return bits.CTS, bits.DSR, bits.RI, bits.DCD, nil
}

// Close releases the port once. Later calls return the first result, so a
// caller repeating a close cannot learn a different answer or double-close the
// descriptor.
func (h *handle) Close() error {
	h.closeOnce.Do(func() {
		h.closeErr = h.port.Close()
	})
	return h.closeErr
}
