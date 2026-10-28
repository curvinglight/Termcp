// Package port is the only place go.bug.st/serial appears.
//
// It exists so the rest of the program never sees a library type: swapping the
// library, or adding the flow control it lacks, changes this package alone. A
// serial port is not a command in a channel and cannot be configured via the
// SSH profile path, so the seam is deliberately narrow — open a device with a
// validated config, read and write bytes, toggle the modem lines.
//
// The library (v1.8.0) was verified against source rather than its docs:
//
//   - Mode has no flow-control field and Port has no flow-control method, which
//     is why flow control is refused rather than silently dropped (see Config).
//   - SetReadTimeout exists, so a blocked Read can be bounded without relying on
//     Close alone.
//   - PortError is a struct with unexported fields; its code is read with
//     Code(). Errors are normalized here so callers never match on text.
package port

import (
	"errors"
	"time"
)

// Errors reported by this package. Callers branch on them with errors.Is; the
// MCP layer maps each to a stable error code rather than surfacing library text.
var (
	// ErrBusy reports the device is already open, by this process or another.
	ErrBusy = errors.New("serial port busy")

	// ErrNotFound reports the device path does not exist.
	ErrNotFound = errors.New("serial port not found")

	// ErrPermission reports the device exists but cannot be opened (not root,
	// not in dialout, or held by another user).
	ErrPermission = errors.New("serial port permission denied")

	// ErrUnsupportedFormat reports a parameter combination this platform or
	// driver rejects. The library's own error text is not passed through: a
	// failed open is normalized to this so callers see one stable cause.
	ErrUnsupportedFormat = errors.New("serial format not supported")

	// ErrDeviceGone reports the device disappeared mid-session.
	ErrDeviceGone = errors.New("serial device gone")

	// ErrInvalidConfig reports a config this package refuses before touching the
	// device (bad baud, unknown stop bits, flow control requested).
	ErrInvalidConfig = errors.New("invalid serial config")
)

// StopBits is the stop-bit setting. It is a string, not an int, so the wire
// format (TOML, JSON, MCP parameters) keeps the "1.5" spelling readable and the
// conversion to the library's enum stays in one place.
type StopBits string

const (
	StopBitsOne        StopBits = "1"
	StopBitsOneAndHalf StopBits = "1.5"
	StopBitsTwo        StopBits = "2"
)

// Parity values accepted in Config.Parity.
const (
	ParityNone  = "none"
	ParityOdd   = "odd"
	ParityEven  = "even"
	ParityMark  = "mark"
	ParitySpace = "space"
)

// Line-level settings accepted in Config.DTR and Config.RTS. "keep" leaves the
// line untouched; the library's default asserts DTR and RTS, which resets many
// boards, so leaving them alone is the cautious default.
const (
	LineKeep = "keep"
	LineHigh = "high"
	LineLow  = "low"
)

// Config is a validated request to open one device. It carries only what
// internal/serial/port needs; profile fields such as tx_eol and encoding never
// reach this layer, because they describe bytes rather than the port.
type Config struct {
	Device   string
	Baud     int
	DataBits int
	StopBits StopBits
	Parity   string
	DTR      string
	RTS      string
}

// DefaultConfig returns the settings a serial profile inherits when it omits a
// field, given a device. 115200 8N1 is the common embedded console default.
func DefaultConfig(device string) Config {
	return Config{
		Device:   device,
		Baud:     115200,
		DataBits: 8,
		StopBits: StopBitsOne,
		Parity:   ParityNone,
		DTR:      LineKeep,
		RTS:      LineKeep,
	}
}

// Info describes one enumerated port.
type Info struct {
	// Name is the platform device path: /dev/ttyUSB0, /dev/cu.usbserial-XXXX,
	// COM3, \\.\COM3.
	Name string
	// StableName is a path that survives renumbering, when the platform offers
	// one: /dev/serial/by-id/... on Linux. Empty when unavailable (Windows, or
	// macOS without cgo), in which case Name is all there is.
	StableName string
	// IsUSB reports whether the port is a USB serial adapter.
	IsUSB bool
	// VID and PID are USB identifiers, empty when the platform cannot report
	// them (macOS without cgo) or when the port is not USB.
	VID string
	PID string
	// SerialNumber, Manufacturer and Product are USB descriptor strings, empty
	// when unavailable.
	SerialNumber string
	Manufacturer string
	Product      string
}

// Handle is an open port. It is the only view the rest of the program gets;
// nothing above this package imports go.bug.st/serial.
type Handle interface {
	Read(p []byte) (int, error)
	Write(p []byte) (int, error)

	// SetDTR and SetRTS drive the output modem lines. They are how a board is
	// reset into a bootloader, so they are deliberate operations, not setup.
	SetDTR(on bool) error
	SetRTS(on bool) error

	// Break holds the line in the break state for d.
	Break(d time.Duration) error

	// ModemStatus reports the input lines.
	ModemStatus() (CTS, DSR, RI, DCD bool, err error)

	// SetReadTimeout bounds a single Read. Zero means the library default;
	// use NoTimeout to block indefinitely.
	SetReadTimeout(d time.Duration) error

	// FlushInput and FlushOutput discard buffered bytes in the given direction.
	FlushInput() error
	FlushOutput() error

	// Close releases the port. It must be safe to call more than once: the
	// session's Disconnect, finalize and root-shell close can each reach it.
	Close() error
}

// NoTimeout disables the read timeout, matching the library's sentinel. It is
// re-exported so callers of Handle.SetReadTimeout never import the library.
const NoTimeout time.Duration = -1
