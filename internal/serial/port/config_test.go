package port

import (
	"errors"
	"runtime"
	"strings"
	"testing"
)

func TestDefaultConfigIsValid(t *testing.T) {
	// A default config must pass validation on its own platform, so a profile
	// that sets only a device is usable.
	cfg := DefaultConfig(platformDevice())
	if err := cfg.Validate(); err != nil {
		t.Fatalf("DefaultConfig should validate, got %v", err)
	}
}

func TestValidateRejectsBadBaud(t *testing.T) {
	for _, baud := range []int{-1, 0, 4_000_001} {
		cfg := DefaultConfig("/dev/ttyUSB0")
		cfg.Baud = baud
		err := cfg.Validate()
		if !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("baud %d: got %v, want ErrInvalidConfig", baud, err)
		}
	}
}

func TestValidateAcceptsNonStandardBaud(t *testing.T) {
	// The profile deliberately allows any positive baud, not just the classic
	// ladder: some devices run at rates like 2000000 that are not in it.
	cfg := DefaultConfig("/dev/ttyUSB0")
	cfg.Baud = 2_000_000
	if err := cfg.Validate(); err != nil {
		t.Fatalf("non-standard baud should be accepted, got %v", err)
	}
}

func TestValidateRejectsBadDataBits(t *testing.T) {
	for _, bits := range []int{0, 4, 9, -1} {
		cfg := DefaultConfig("/dev/ttyUSB0")
		cfg.DataBits = bits
		if err := cfg.Validate(); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("data_bits %d: got %v, want ErrInvalidConfig", bits, err)
		}
	}
}

func TestValidateAcceptsAllDataBits(t *testing.T) {
	for _, bits := range []int{5, 6, 7, 8} {
		cfg := DefaultConfig("/dev/ttyUSB0")
		cfg.DataBits = bits
		if err := cfg.Validate(); err != nil {
			t.Errorf("data_bits %d should be valid, got %v", bits, err)
		}
	}
}

func TestValidateStopBits(t *testing.T) {
	for _, sb := range []StopBits{StopBitsOne, StopBitsOneAndHalf, StopBitsTwo} {
		cfg := DefaultConfig("/dev/ttyUSB0")
		cfg.StopBits = sb
		if err := cfg.Validate(); err != nil {
			t.Errorf("stop_bits %q should be valid, got %v", sb, err)
		}
	}

	for _, sb := range []StopBits{"", "0", "3", "1.5x", "one"} {
		cfg := DefaultConfig("/dev/ttyUSB0")
		cfg.StopBits = sb
		if err := cfg.Validate(); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("stop_bits %q: got %v, want ErrInvalidConfig", sb, err)
		}
	}
}

func TestValidateParity(t *testing.T) {
	for _, p := range []string{ParityNone, ParityOdd, ParityEven, ParityMark, ParitySpace} {
		cfg := DefaultConfig("/dev/ttyUSB0")
		cfg.Parity = p
		if err := cfg.Validate(); err != nil {
			t.Errorf("parity %q should be valid, got %v", p, err)
		}
	}

	for _, p := range []string{"", "NONE", "8", "no"} {
		cfg := DefaultConfig("/dev/ttyUSB0")
		cfg.Parity = p
		if err := cfg.Validate(); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("parity %q: got %v, want ErrInvalidConfig", p, err)
		}
	}
}

func TestValidateLineLevels(t *testing.T) {
	for _, level := range []string{LineKeep, LineHigh, LineLow} {
		cfg := DefaultConfig("/dev/ttyUSB0")
		cfg.DTR = level
		cfg.RTS = level
		if err := cfg.Validate(); err != nil {
			t.Errorf("line level %q should be valid, got %v", level, err)
		}
	}

	for _, level := range []string{"", "on", "LOW"} {
		cfg := DefaultConfig("/dev/ttyUSB0")
		cfg.DTR = level
		if err := cfg.Validate(); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("dtr %q: got %v, want ErrInvalidConfig", level, err)
		}
	}
}

func TestValidateRejectsEmptyDevice(t *testing.T) {
	cfg := DefaultConfig("   ")
	if err := cfg.Validate(); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("blank device should be rejected, got %v", err)
	}
}

func TestValidateRejectsDeviceOutsideAllowlist(t *testing.T) {
	// The device path decides which file gets opened, so anything outside the
	// serial allowlist has to be refused before an open is attempted.
	bad := []string{
		"/etc/passwd",
		"/dev/null",
		"/dev/zero",
		"/dev/tty",      // controlling terminal, not a device file
		"/dev/sda",      // a disk
		"/tmp/whatever", // not /dev at all
		"../../etc/shadow",
	}
	for _, device := range bad {
		cfg := DefaultConfig(device)
		if err := cfg.Validate(); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("device %q: got %v, want ErrInvalidConfig", device, err)
		}
	}
}

func TestValidateAcceptsAllowedLinuxDevices(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix device paths")
	}
	good := []string{
		"/dev/ttyUSB0",
		"/dev/ttyACM0",
		"/dev/ttyS0",
		"/dev/serial/by-id/usb-FTDI_FT232R_USB_UART_A10K3PTS-if00-port0",
		"/dev/serial/by-path/pci-0000:00:14.0-usb-0:1:1.0-port0",
		"/dev/cu.usbserial-A10K3PTS",
		"/dev/tty.usbserial-1420",
	}
	for _, device := range good {
		cfg := DefaultConfig(device)
		if err := cfg.Validate(); err != nil {
			t.Errorf("device %q should be accepted, got %v", device, err)
		}
	}
}

func TestValidateRejectsNULByte(t *testing.T) {
	cfg := DefaultConfig("/dev/ttyUSB0\x00/etc/passwd")
	if err := cfg.Validate(); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("NUL byte should be rejected, got %v", err)
	}
}

func TestWindowsDeviceNames(t *testing.T) {
	// These are validated regardless of host OS: the rule belongs to the value,
	// and a Windows profile can be created from any platform.
	good := []string{"COM1", "COM3", "COM10", "COM256", `\\.\COM3`, `\\.\COM10`}
	for _, device := range good {
		if err := validateWindowsDevice(device); err != nil {
			t.Errorf("device %q should be accepted, got %v", device, err)
		}
	}

	bad := []string{"COM0", "COM", "COMX", "COM-1", "LPT1", "COM1x", `\\.\COM`}
	for _, device := range bad {
		if err := validateWindowsDevice(device); err == nil {
			t.Errorf("device %q should be rejected", device)
		}
	}
}

func TestValidateAcceptsWindowsDeviceRegardlessOfHost(t *testing.T) {
	// A profile naming COM3 must validate on Linux too: profiles are stored, not
	// bound to the machine that wrote them.
	cfg := DefaultConfig("COM3")
	if err := cfg.Validate(); err != nil {
		t.Fatalf("COM3 should validate on any host, got %v", err)
	}
}

func TestCanonicalDeviceFoldsWindowsSpellings(t *testing.T) {
	// COM3 and \\.\COM3 are the same port; two profiles must not be able to
	// claim the same device by spelling it differently.
	cases := map[string]string{
		"COM3":      "COM3",
		`\\.\COM3`:  "COM3",
		"com3":      "COM3",
		`\\.\com10`: "COM10",
		"  COM10  ": "COM10",
		"Com1":      "COM1",
		`\\.\COM1`:  "COM1",
	}
	for in, want := range cases {
		if got := CanonicalDevice(in); got != want {
			t.Errorf("CanonicalDevice(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCanonicalDeviceEmpty(t *testing.T) {
	if got := CanonicalDevice("   "); got != "" {
		t.Fatalf("CanonicalDevice(blank) = %q, want \"\"", got)
	}
}

func TestCanonicalDeviceResolvesSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}

	dir := t.TempDir()
	target := dir + "/ttyUSB0"
	link := dir + "/by-id-link"

	if err := writeFile(target, "not a real device"); err != nil {
		t.Fatalf("setup target: %v", err)
	}
	if err := symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	// The alias and its target must fold to the same identity, or a second
	// profile could open the same device through the other name.
	gotLink := CanonicalDevice(link)
	gotTarget := CanonicalDevice(target)
	if gotLink != gotTarget {
		t.Fatalf("alias %q resolved to %q, target is %q: they must match",
			link, gotLink, gotTarget)
	}
}

func TestCanonicalDeviceUnresolvableLinkStaysStable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}

	dir := t.TempDir()
	link := dir + "/dangling"
	if err := symlink(dir+"/nowhere", link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	// A dangling link must still canonicalize to something deterministic, so
	// the same misspelled profile compares equal to itself.
	first := CanonicalDevice(link)
	second := CanonicalDevice(link)
	if first != second {
		t.Fatalf("canonical form is unstable: %q then %q", first, second)
	}
	if !strings.HasSuffix(first, "dangling") {
		t.Fatalf("expected the link path back, got %q", first)
	}
}

func TestToLibraryModeMapsFields(t *testing.T) {
	cfg := Config{
		Device:   "/dev/ttyUSB0",
		Baud:     9600,
		DataBits: 7,
		StopBits: StopBitsTwo,
		Parity:   ParityEven,
		DTR:      LineKeep,
		RTS:      LineKeep,
	}
	mode, err := cfg.toLibraryMode()
	if err != nil {
		t.Fatalf("toLibraryMode: %v", err)
	}
	if mode.BaudRate != 9600 {
		t.Errorf("BaudRate = %d, want 9600", mode.BaudRate)
	}
	if mode.DataBits != 7 {
		t.Errorf("DataBits = %d, want 7", mode.DataBits)
	}
	if mode.InitialStatusBits != nil {
		t.Errorf("InitialStatusBits = %+v, want nil for keep/keep", mode.InitialStatusBits)
	}
}

func TestToLibraryModeKeepLeavesLinesUnasserted(t *testing.T) {
	// Leaving the lines alone has to mean InitialStatusBits stays nil: setting
	// the struct at all makes the library drive both lines, which resets many
	// boards. This is the difference between "keep" and "assert both".
	cfg := DefaultConfig("/dev/ttyUSB0")
	mode, err := cfg.toLibraryMode()
	if err != nil {
		t.Fatalf("toLibraryMode: %v", err)
	}
	if mode.InitialStatusBits != nil {
		t.Fatalf("keep/keep should leave InitialStatusBits nil, got %+v", mode.InitialStatusBits)
	}
}

func TestToLibraryModeExplicitLines(t *testing.T) {
	cfg := DefaultConfig("/dev/ttyUSB0")
	cfg.DTR = LineLow
	cfg.RTS = LineHigh

	mode, err := cfg.toLibraryMode()
	if err != nil {
		t.Fatalf("toLibraryMode: %v", err)
	}
	if mode.InitialStatusBits == nil {
		t.Fatal("an explicit line level must set InitialStatusBits")
	}
	if mode.InitialStatusBits.DTR {
		t.Error("DTR should be low")
	}
	if !mode.InitialStatusBits.RTS {
		t.Error("RTS should be high")
	}
}

// platformDevice returns a device path valid for the host, for tests that need
// a config which passes validation.
func platformDevice() string {
	if runtime.GOOS == "windows" {
		return "COM3"
	}
	return "/dev/ttyUSB0"
}
