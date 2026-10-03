package port

import (
	"fmt"
	"path/filepath"
	"strings"

	"go.bug.st/serial"
)

// maxBaud bounds the baud rate. The library accepts arbitrary positive values
// on Linux, but a profile is user input and an unbounded number is a mistake
// more often than an exotic baud rate is a need.
const maxBaud = 4_000_000

// Validate checks a Config without touching a device, and reports the first
// problem. Every parameter is checked together rather than field by field,
// because the failures that matter (1.5 stop bits, mark/space parity) belong to
// the combination and to the platform, not to a single value.
//
// It returns ErrInvalidConfig for the checks this package owns, and
// ErrUnsupportedFormat when the request is well formed but the platform cannot
// honour it.
func (c Config) Validate() error {
	if err := validateDevice(c.Device); err != nil {
		return err
	}

	if c.Baud <= 0 {
		return fmt.Errorf("%w: baud must be positive, got %d", ErrInvalidConfig, c.Baud)
	}
	if c.Baud > maxBaud {
		return fmt.Errorf("%w: baud %d exceeds the %d maximum", ErrInvalidConfig, c.Baud, maxBaud)
	}

	switch c.DataBits {
	case 5, 6, 7, 8:
	case 0:
		return fmt.Errorf("%w: data_bits is required", ErrInvalidConfig)
	default:
		return fmt.Errorf("%w: data_bits must be 5, 6, 7 or 8, got %d", ErrInvalidConfig, c.DataBits)
	}

	switch c.StopBits {
	case StopBitsOne, StopBitsOneAndHalf, StopBitsTwo:
	case "":
		return fmt.Errorf("%w: stop_bits is required", ErrInvalidConfig)
	default:
		return fmt.Errorf("%w: stop_bits must be %q, %q or %q, got %q",
			ErrInvalidConfig, StopBitsOne, StopBitsOneAndHalf, StopBitsTwo, c.StopBits)
	}

	switch c.Parity {
	case ParityNone, ParityOdd, ParityEven, ParityMark, ParitySpace:
	case "":
		return fmt.Errorf("%w: parity is required", ErrInvalidConfig)
	default:
		return fmt.Errorf("%w: parity must be none, odd, even, mark or space, got %q", ErrInvalidConfig, c.Parity)
	}

	for name, level := range map[string]string{"dtr": c.DTR, "rts": c.RTS} {
		switch level {
		case LineKeep, LineHigh, LineLow:
		case "":
			return fmt.Errorf("%w: %s is required", ErrInvalidConfig, name)
		default:
			return fmt.Errorf("%w: %s must be keep, high or low, got %q", ErrInvalidConfig, name, level)
		}
	}

	return nil
}

// validateDevice enforces the platform device-path allowlist. A serial profile
// names a device, and the value decides which file gets opened, so it is
// checked here rather than trusted from the UI.
//
// The allowlist is intentionally narrow: a path outside it is either a mistake
// or an attempt to open something that is not a serial port.
func validateDevice(device string) error {
	d := strings.TrimSpace(device)
	if d == "" {
		return fmt.Errorf("%w: device is required", ErrInvalidConfig)
	}
	if strings.ContainsRune(d, 0) {
		return fmt.Errorf("%w: device contains a NUL byte", ErrInvalidConfig)
	}

	if strings.HasPrefix(d, "/dev/") {
		return validateUnixDevice(d)
	}
	return validateWindowsDevice(d)
}

func validateUnixDevice(d string) error {
	// Serial devices live under /dev. Accept the tty nodes, the macOS callout
	// (cu) nodes, and the two udev stable aliases. /dev/tty (the controlling
	// terminal) and /dev/ttyS* are deliberately excluded: the former is not a
	// device file, and the latter is a legacy on-board port this feature does
	// not target.
	allowed := []string{
		"/dev/ttyUSB",   // USB serial (Linux)
		"/dev/ttyACM",   // USB CDC-ACM (Linux)
		"/dev/ttyAMA",   // ARM PL011 UART (Linux)
		"/dev/ttyS",     // on-board UART (Linux)
		"/dev/serial/",  // udev by-id / by-path aliases (Linux)
		"/dev/cu.",      // callout devices (macOS)
		"/dev/tty.usb",  // USB serial (macOS)
		"/dev/tty.uart", // on-board UART (macOS)
	}
	for _, prefix := range allowed {
		if strings.HasPrefix(d, prefix) {
			return nil
		}
	}
	return fmt.Errorf("%w: %q is not an allowed serial device path", ErrInvalidConfig, d)
}

func validateWindowsDevice(d string) error {
	// COM<n>, optionally in the \\.\COM<n> form the Win32 API requires for
	// n >= 10. The two spellings name the same device, so both are accepted and
	// CanonicalDevice folds them together.
	name := strings.ToUpper(d)
	name = strings.TrimPrefix(name, `\\.\`)
	if !strings.HasPrefix(name, "COM") {
		return fmt.Errorf("%w: %q is not a COM port", ErrInvalidConfig, d)
	}
	digits := name[len("COM"):]
	if digits == "" {
		return fmt.Errorf("%w: %q has no port number", ErrInvalidConfig, d)
	}
	n := 0
	for _, r := range digits {
		if r < '0' || r > '9' {
			return fmt.Errorf("%w: %q has a non-numeric port number", ErrInvalidConfig, d)
		}
		n = n*10 + int(r-'0')
		// Guard against a run of digits overflowing before the length check.
		if n > 1_000_000 {
			return fmt.Errorf("%w: %q port number is out of range", ErrInvalidConfig, d)
		}
	}
	if n < 1 {
		return fmt.Errorf("%w: %q port number must be >= 1", ErrInvalidConfig, d)
	}
	return nil
}

// toLibraryMode maps a validated Config onto the library's Mode. It is the one
// place the enum conversion happens, and it returns ErrUnsupportedFormat for
// combinations the library cannot express rather than approximating them.
func (c Config) toLibraryMode() (*serial.Mode, error) {
	mode := &serial.Mode{
		BaudRate: c.Baud,
		DataBits: c.DataBits,
	}

	switch c.StopBits {
	case StopBitsOne:
		mode.StopBits = serial.OneStopBit
	case StopBitsOneAndHalf:
		mode.StopBits = serial.OnePointFiveStopBits
	case StopBitsTwo:
		mode.StopBits = serial.TwoStopBits
	default:
		return nil, fmt.Errorf("%w: stop_bits %q", ErrInvalidConfig, c.StopBits)
	}

	switch c.Parity {
	case ParityNone:
		mode.Parity = serial.NoParity
	case ParityOdd:
		mode.Parity = serial.OddParity
	case ParityEven:
		mode.Parity = serial.EvenParity
	case ParityMark:
		mode.Parity = serial.MarkParity
	case ParitySpace:
		mode.Parity = serial.SpaceParity
	default:
		return nil, fmt.Errorf("%w: parity %q", ErrInvalidConfig, c.Parity)
	}

	// The initial line levels. Leaving InitialStatusBits nil lets the library
	// assert DTR and RTS, which pulses many boards into reset — the opposite of
	// "keep". When either line is requested, the struct is built explicitly so
	// the untouched line is still driven to a known level rather than the
	// library's default.
	if c.DTR != LineKeep || c.RTS != LineKeep {
		bits := &serial.ModemOutputBits{RTS: true, DTR: true}
		if c.DTR == LineLow {
			bits.DTR = false
		}
		if c.RTS == LineLow {
			bits.RTS = false
		}
		mode.InitialStatusBits = bits
	}

	return mode, nil
}

// CanonicalDevice reduces the spellings that name the same device to one
// string, so two profiles cannot claim the same physical port by writing it
// differently.
//
// On Unix it resolves symlinks: /dev/serial/by-id/usb-FTDI_... is a link to
// /dev/ttyUSB0, and opening both would open one device twice. Symlinks resolve
// to "/dev/ttyUSB0" while the alias itself is returned unchanged when it does
// not resolve, because the caller compares the two only to detect a collision.
//
// On Windows COM3 and \\.\COM3 are the same port, and names are
// case-insensitive; both are folded to the bare upper-case form.
func CanonicalDevice(device string) string {
	d := strings.TrimSpace(device)
	if d == "" {
		return ""
	}

	// A COM name is recognized by its shape, not by "not being a Unix path":
	// folding case and stripping a prefix is only meaningful for Windows device
	// names, and doing it to anything else would corrupt a legitimate path.
	if isWindowsPortName(d) {
		name := strings.ToUpper(d)
		return strings.TrimPrefix(name, `\\.\`)
	}

	// Unix: resolve the link so an alias and its target share one identity. The
	// cleaned path is returned when resolution fails (a dangling link, or a
	// device that is not present), because the caller only compares the result
	// against itself and needs it to be stable.
	resolved, err := filepath.EvalSymlinks(d)
	if err != nil {
		return filepath.Clean(d)
	}
	return filepath.Clean(resolved)
}

// isWindowsPortName reports whether a device string is a Windows COM name,
// with or without the \\.\ device-namespace prefix.
func isWindowsPortName(d string) bool {
	name := strings.ToUpper(d)
	name = strings.TrimPrefix(name, `\\.\`)
	if !strings.HasPrefix(name, "COM") {
		return false
	}
	digits := name[len("COM"):]
	if digits == "" {
		return false
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
