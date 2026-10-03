package serial

import (
	"errors"
	"runtime"
	"strings"
	"testing"
)

// deviceFor builds a device path valid on the host, so tests that are about
// something other than the allowlist can use a config that validates.
func deviceFor() string {
	if runtime.GOOS == "windows" {
		return "COM3"
	}
	return "/dev/ttyUSB0"
}

const minimalConfig = `
kind = "serial"
device = "/dev/ttyUSB0"
baud = 115200
`

func TestParseMinimalConfigAppliesDefaults(t *testing.T) {
	e, err := ParseAndValidate([]byte(minimalConfig))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if e.DataBits != DefaultDataBits {
		t.Errorf("DataBits = %d, want %d", e.DataBits, DefaultDataBits)
	}
	if e.TxEOL != DefaultTxEOL {
		t.Errorf("TxEOL = %q, want %q", e.TxEOL, DefaultTxEOL)
	}
	if e.Encoding != DefaultEncoding {
		t.Errorf("Encoding = %q, want %q", e.Encoding, DefaultEncoding)
	}
	if !e.NormalizeRx() {
		t.Error("rx_normalize should default to true")
	}
	if e.LocalEcho {
		t.Error("local_echo should default to false")
	}
	if e.DefaultApproval {
		t.Error("default_approval should default to false")
	}
}

func TestParseRejectsUnknownField(t *testing.T) {
	// The whole reason for decoding strictly: a misspelled key must not be
	// silently ignored, or the session runs with a setting the user never chose.
	cfg := minimalConfig + "baudrate = 9600\n"
	_, err := ParseAndValidate([]byte(cfg))
	if err == nil {
		t.Fatal("an unknown field should be rejected")
	}
	if !strings.Contains(err.Error(), "baudrate") {
		t.Fatalf("error should name the offending field, got %v", err)
	}
}

func TestParseRejectsSSHFields(t *testing.T) {
	// A serial profile carrying ssh fields was written for the wrong kind;
	// ignoring them would hide that.
	sshFields := []string{
		`host = "example.com"`,
		`port = 22`,
		`user = "root"`,
		`password = "hunter2"`,
		`private_key = "/k"`,
		`key_passphrase = "p"`,
		`proxy = "socks5://x"`,
		`known_hosts = "/kh"`,
		`trust_unknown_host = true`,
		`default_shell = "bash"`,
		`default_mode = "pty"`,
	}
	for _, field := range sshFields {
		cfg := minimalConfig + field + "\n"
		_, err := ParseAndValidate([]byte(cfg))
		if err == nil {
			t.Errorf("field %q should be rejected in a serial profile", field)
			continue
		}
		if !strings.Contains(err.Error(), "SSH field") {
			t.Errorf("field %q: error should say it is an SSH field, got %v", field, err)
		}
	}
}

func TestParseRejectsWrongKind(t *testing.T) {
	cases := []string{
		`kind = "remote"` + "\ndevice = \"/dev/ttyUSB0\"\nbaud = 115200\n",
		`kind = "serial2"` + "\ndevice = \"/dev/ttyUSB0\"\nbaud = 115200\n",
		`device = "/dev/ttyUSB0"` + "\nbaud = 115200\n", // kind absent
	}
	for _, cfg := range cases {
		if _, err := ParseAndValidate([]byte(cfg)); err == nil {
			t.Errorf("config should be rejected for kind:\n%s", cfg)
		}
	}
}

func TestValidateRefusesFlowControl(t *testing.T) {
	// Flow control must be refused before any device is touched: a profile that
	// asks for it and silently gets none drops data at high baud rates.
	for _, fc := range []string{"rtscts", "xonxoff", "dsrdtr", "hardware"} {
		cfg := minimalConfig + "flow_control = \"" + fc + "\"\n"
		_, err := ParseAndValidate([]byte(cfg))
		if err == nil {
			t.Errorf("flow_control %q should be refused", fc)
			continue
		}
		if !strings.Contains(err.Error(), "flow control") {
			t.Errorf("flow_control %q: error should mention it, got %v", fc, err)
		}
		// The sentinel, not just prose: the MCP layer maps ErrFlowControl to
		// serial_flow_control_unsupported, and a caller branching on that code
		// must get it wherever the refusal surfaced (design §7).
		if !errors.Is(err, ErrFlowControl) {
			t.Errorf("flow_control %q: error should wrap ErrFlowControl, got %v", fc, err)
		}
	}
}

func TestValidateAcceptsFlowControlNone(t *testing.T) {
	cfg := minimalConfig + "flow_control = \"none\"\n"
	if _, err := ParseAndValidate([]byte(cfg)); err != nil {
		t.Fatalf("flow_control = none should be accepted, got %v", err)
	}
}

func TestValidateRejectsBadTxEOL(t *testing.T) {
	cfg := minimalConfig + "tx_eol = \"crlfx\"\n"
	if _, err := ParseAndValidate([]byte(cfg)); err == nil {
		t.Fatal("an unknown tx_eol should be rejected")
	}
}

func TestValidateAcceptsAllTxEOL(t *testing.T) {
	for _, v := range []string{TxEOLCR, TxEOLCRLF, TxEOLLF, TxEOLNone} {
		cfg := minimalConfig + "tx_eol = \"" + v + "\"\n"
		if _, err := ParseAndValidate([]byte(cfg)); err != nil {
			t.Errorf("tx_eol %q should be accepted, got %v", v, err)
		}
	}
}

func TestValidateRejectsBadEncoding(t *testing.T) {
	cfg := minimalConfig + "encoding = \"base32\"\n"
	if _, err := ParseAndValidate([]byte(cfg)); err == nil {
		t.Fatal("an unknown encoding should be rejected")
	}
}

func TestValidateAcceptsAllEncodings(t *testing.T) {
	for _, v := range []string{EncodingAuto, EncodingText, EncodingHex, EncodingB64} {
		cfg := minimalConfig + "encoding = \"" + v + "\"\n"
		if _, err := ParseAndValidate([]byte(cfg)); err != nil {
			t.Errorf("encoding %q should be accepted, got %v", v, err)
		}
	}
}

func TestRxNormalizeIsDistinguishableFromFalse(t *testing.T) {
	// rx_normalize defaults to true, so the parser has to tell "absent" from
	// "explicitly false". A plain bool cannot.
	on, err := ParseAndValidate([]byte(minimalConfig))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !on.NormalizeRx() {
		t.Error("omitted rx_normalize should mean true")
	}

	off, err := ParseAndValidate([]byte(minimalConfig + "rx_normalize = false\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if off.NormalizeRx() {
		t.Error("rx_normalize = false should mean false")
	}
}

func TestValidateRejectsBadDevice(t *testing.T) {
	// The device path decides which file is opened, so it is checked here as
	// well as in the port layer.
	cfg := `
kind = "serial"
device = "/etc/passwd"
baud = 115200
`
	if _, err := ParseAndValidate([]byte(cfg)); err == nil {
		t.Fatal("a device outside the allowlist should be rejected")
	}
}

func TestValidateRejectsBadBaud(t *testing.T) {
	for _, baud := range []string{"0", "-9600", "9999999"} {
		cfg := "kind = \"serial\"\ndevice = \"" + deviceFor() + "\"\nbaud = " + baud + "\n"
		if _, err := ParseAndValidate([]byte(cfg)); err == nil {
			t.Errorf("baud %s should be rejected", baud)
		}
	}
}

func TestValidateRequiresBaud(t *testing.T) {
	// There is no safe rate to assume for an unknown device, so baud is required
	// rather than defaulted.
	cfg := "kind = \"serial\"\ndevice = \"" + deviceFor() + "\"\n"
	if _, err := ParseAndValidate([]byte(cfg)); err == nil {
		t.Fatal("a profile without baud should be rejected")
	}
}

func TestParseRejectsOversizedFile(t *testing.T) {
	big := make([]byte, maxConfigFileSize+1)
	for i := range big {
		big[i] = ' '
	}
	if _, err := ParseAndValidate(big); err == nil {
		t.Fatal("an oversized config should be rejected")
	}
}

func TestPortConfigProjectsOnlyPortFields(t *testing.T) {
	cfg := minimalConfig + `
data_bits = 7
stop_bits = "2"
parity = "even"
dtr = "low"
rts = "high"
tx_eol = "lf"
encoding = "hex"
`
	e, err := ParseAndValidate([]byte(cfg))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	pc := e.PortConfig()
	if pc.Baud != 115200 {
		t.Errorf("Baud = %d, want 115200", pc.Baud)
	}
	if pc.DataBits != 7 {
		t.Errorf("DataBits = %d, want 7", pc.DataBits)
	}
	if pc.StopBits != "2" {
		t.Errorf("StopBits = %q, want \"2\"", pc.StopBits)
	}
	if pc.Parity != "even" {
		t.Errorf("Parity = %q, want \"even\"", pc.Parity)
	}
	if pc.DTR != "low" || pc.RTS != "high" {
		t.Errorf("DTR/RTS = %q/%q, want low/high", pc.DTR, pc.RTS)
	}
}

func TestParseAcceptsWindowsDeviceOnAnyHost(t *testing.T) {
	// A stored profile is not bound to the machine that wrote it.
	cfg := "kind = \"serial\"\ndevice = \"COM10\"\nbaud = 9600\n"
	if _, err := ParseAndValidate([]byte(cfg)); err != nil {
		t.Fatalf("a COM profile should parse on any host, got %v", err)
	}
}
