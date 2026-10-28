package sshconfig

import (
	"errors"
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/serial"
)

// One document, three kinds: the matrix of what each kind accepts and refuses.
// The refusals are the point — a field accepted and ignored on the wrong kind
// makes a profile that dials a host while naming a device, or the reverse, and
// nothing downstream can tell.

const serialTOML = `kind = "serial"
device = "/dev/ttyUSB0"
baud = 115200
tx_eol = "cr"
`

func TestParseAndValidateAcceptsSerial(t *testing.T) {
	e, err := ParseAndValidate([]byte(serialTOML))
	if err != nil {
		t.Fatalf("serial profile refused: %v", err)
	}
	if e.Kind != KindSerial {
		t.Fatalf("kind = %q, want serial", e.Kind)
	}
	if e.Device != "/dev/ttyUSB0" || e.Baud != 115200 {
		t.Fatalf("serial fields = %+v", e.serialSpec())
	}
	if e.TxEOL != serial.TxEOLCR {
		t.Errorf("tx_eol = %q, want cr (default applied)", e.TxEOL)
	}
	spec, err := SerialFromEntry(e)
	if err != nil || spec.Device != "/dev/ttyUSB0" {
		t.Fatalf("SerialFromEntry = %+v, %v", spec, err)
	}
}

func TestParseAndValidateSerialRequiresDeviceAndBaud(t *testing.T) {
	for name, doc := range map[string]string{
		"no device": "kind = \"serial\"\nbaud = 9600\n",
		"no baud":   "kind = \"serial\"\ndevice = \"/dev/ttyUSB0\"\n",
	} {
		if _, err := ParseAndValidate([]byte(doc)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
}

func TestParseAndValidateSerialRefusesDialFields(t *testing.T) {
	doc := serialTOML + "host = \"h\"\nuser = \"u\"\npassword = \"p\"\n"
	_, err := ParseAndValidate([]byte(doc))
	if err == nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("dial fields on serial: err = %v, want ErrInvalid", err)
	}
	for _, field := range []string{"host", "user", "password"} {
		if !strings.Contains(err.Error(), field) {
			t.Errorf("refusal should name %q: %v", field, err)
		}
	}
}

func TestParseAndValidateSerialRefusesShellFields(t *testing.T) {
	doc := serialTOML + "default_shell = \"/bin/bash\"\n"
	_, err := ParseAndValidate([]byte(doc))
	if err == nil || !strings.Contains(err.Error(), "default_shell") {
		t.Fatalf("default_shell on serial: err = %v, want a named refusal", err)
	}
}

func TestParseAndValidateSerialRefusesFlowControl(t *testing.T) {
	doc := serialTOML + "flow_control = \"hardware\"\n"
	_, err := ParseAndValidate([]byte(doc))
	if !errors.Is(err, serial.ErrFlowControl) {
		t.Fatalf("flow control on serial: err = %v, want ErrFlowControl", err)
	}
}

func TestParseAndValidateRemoteRefusesSerialFields(t *testing.T) {
	doc := "kind = \"remote\"\nhost = \"h\"\nuser = \"u\"\npassword = \"p\"\ndevice = \"/dev/ttyUSB0\"\nbaud = 9600\n"
	_, err := ParseAndValidate([]byte(doc))
	if err == nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("serial fields on remote: err = %v, want ErrInvalid", err)
	}
	if !strings.Contains(err.Error(), "device") {
		t.Errorf("refusal should name the field: %v", err)
	}
}

func TestParseAndValidateInternalRefusesSerialFields(t *testing.T) {
	doc := "kind = \"internal\"\ndevice = \"/dev/ttyUSB0\"\n"
	_, err := ParseAndValidate([]byte(doc))
	if err == nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("serial fields on internal: err = %v, want ErrInvalid", err)
	}
}

func TestSerialFromEntryRefusesNonSerial(t *testing.T) {
	e, err := ParseAndValidate([]byte("kind = \"remote\"\nhost = \"h\"\nuser = \"u\"\npassword = \"p\"\n"))
	if err != nil {
		t.Fatalf("remote profile refused: %v", err)
	}
	if _, err := SerialFromEntry(e); err == nil {
		t.Fatal("SerialFromEntry on a remote profile should be refused")
	}
}

func TestSerialProfileRoundTripsThroughTOML(t *testing.T) {
	// A saved profile must read back with the same device fields, or a session
	// started next boot dials a different line.
	e, err := ParseAndValidate([]byte(serialTOML))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	doc := "kind = \"serial\"\ndevice = \"" + e.Device + "\"\nbaud = " +
		itoa(e.Baud) + "\ndata_bits = " + itoa(e.DataBits) +
		"\ntx_eol = \"" + e.TxEOL + "\"\n"
	e2, err := ParseAndValidate([]byte(doc))
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if e2.Device != e.Device || e2.Baud != e.Baud ||
		e2.DataBits != e.DataBits || e2.TxEOL != e.TxEOL {
		t.Fatalf("round trip drifted: %+v vs %+v", e2.serialSpec(), e.serialSpec())
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
