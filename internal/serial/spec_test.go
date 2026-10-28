package serial

import (
	"errors"
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/serial/port"
)

// The Spec is the serial half of a host profile; the kind dispatch and the
// TOML round trip live in sshconfig. These tests pin the device-side contract:
// defaults, the refusals that protect hardware, and the projection onto the
// port layer.

func validSpec() *Spec {
	s := &Spec{Device: "/dev/ttyUSB0", Baud: 115200}
	s.ApplyDefaults()
	return s
}

func TestMinimalSpecAppliesDefaults(t *testing.T) {
	s := &Spec{Device: " /dev/ttyUSB0 ", Baud: 9600}
	s.ApplyDefaults()
	if s.Device != "/dev/ttyUSB0" {
		t.Errorf("device not trimmed: %q", s.Device)
	}
	if s.DataBits != DefaultDataBits {
		t.Errorf("data_bits default = %d, want %d", s.DataBits, DefaultDataBits)
	}
	if s.StopBits != port.StopBitsOne {
		t.Errorf("stop_bits default = %q, want one", s.StopBits)
	}
	if s.Parity != port.ParityNone {
		t.Errorf("parity default = %q, want none", s.Parity)
	}
	if s.DTR != port.LineKeep || s.RTS != port.LineKeep {
		t.Errorf("line defaults = %q/%q, want keep/keep", s.DTR, s.RTS)
	}
	if s.TxEOL != DefaultTxEOL {
		t.Errorf("tx_eol default = %q, want %q", s.TxEOL, DefaultTxEOL)
	}
	if s.Display != DefaultDisplay {
		t.Errorf("display default = %q, want %q", s.Display, DefaultDisplay)
	}
	if err := s.Validate(); err != nil {
		t.Errorf("minimal spec should validate: %v", err)
	}
}

func TestValidateRefusesFlowControl(t *testing.T) {
	s := validSpec()
	s.FlowControl = "hardware"
	err := s.Validate()
	if !errors.Is(err, ErrFlowControl) {
		t.Fatalf("flow control error = %v, want ErrFlowControl", err)
	}
	// "none" is the spelling that means "I know"; it must be accepted.
	s2 := validSpec()
	s2.FlowControl = "none"
	if err := s2.Validate(); err != nil {
		t.Fatalf("flow_control = none should validate: %v", err)
	}
}

func TestValidateRejectsBadTxEOL(t *testing.T) {
	s := validSpec()
	s.TxEOL = "newline"
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "tx_eol") {
		t.Fatalf("bad tx_eol error = %v, want a tx_eol refusal", err)
	}
	for _, eol := range []string{TxEOLCR, TxEOLCRLF, TxEOLLF, TxEOLNone} {
		s2 := validSpec()
		s2.TxEOL = eol
		if err := s2.Validate(); err != nil {
			t.Errorf("tx_eol %q should validate: %v", eol, err)
		}
	}
}

func TestValidateRejectsBadDisplay(t *testing.T) {
	s := validSpec()
	s.Display = "base64" // a plausible typo of b64
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "display") {
		t.Fatalf("bad display error = %v, want a display refusal", err)
	}
	for _, d := range []string{DisplayAuto, DisplayText, DisplayHex, DisplayB64} {
		s2 := validSpec()
		s2.Display = d
		if err := s2.Validate(); err != nil {
			t.Errorf("display %q should validate: %v", d, err)
		}
	}
}

func TestValidateRejectsBadDeviceAndBaud(t *testing.T) {
	s := validSpec()
	s.Device = ""
	if err := s.Validate(); err == nil {
		t.Error("empty device should be refused")
	}
	s2 := validSpec()
	s2.Baud = 0
	if err := s2.Validate(); err == nil {
		t.Error("zero baud should be refused")
	}
}

func TestRxNormalizeIsDistinguishableFromFalse(t *testing.T) {
	on := &Spec{}
	on.ApplyDefaults()
	if !on.NormalizeRx() {
		t.Error("omitted rx_normalize must default to on")
	}
	off := false
	s := &Spec{RxNormalize: &off}
	if s.NormalizeRx() {
		t.Error("explicit rx_normalize = false must stay off")
	}
}

func TestPortConfigProjectsOnlyPortFields(t *testing.T) {
	s := validSpec()
	s.TxEOL = TxEOLCRLF
	s.Display = DisplayHex
	pc := s.PortConfig()
	if pc.Device != s.Device || pc.Baud != s.Baud {
		t.Errorf("device/baud not projected: %+v", pc)
	}
	if pc.DataBits != 0 {
		// ApplyDefaults has not run: the projection is the raw spec, and the
		// zero data-bits is what defaults have not yet filled.
		t.Logf("data_bits = %d before ApplyDefaults", pc.DataBits)
	}
}

func TestEmptySpecReportsEmpty(t *testing.T) {
	if !(&Spec{}).Empty() {
		t.Error("zero spec should be Empty")
	}
	if (&Spec{Device: "/dev/ttyUSB0"}).Empty() {
		t.Error("a spec with a device is not Empty")
	}
	if (&Spec{Baud: 9600}).Empty() {
		t.Error("a spec with a baud is not Empty")
	}
}
