// Package serial holds the serial device layer: a connection profile's device
// fields (Spec), the channel that exposes one open device as a session
// transport, and the port sub-package that wraps go.bug.st/serial.
//
// Profile PARSING and STORAGE live in internal/sshconfig: a serial profile is a
// host profile with kind = "serial", stored beside the SSH profiles, so every
// surface that lists or resolves hosts sees both kinds through one door. What
// stays here is the vocabulary only serial cares about — device, line
// parameters, tx line ending, display encoding — and the code that talks to the
// hardware.
package serial

import (
	"errors"
	"fmt"
	"strings"

	"github.com/open-mcp-ai/termcp/internal/encoding"
	"github.com/open-mcp-ai/termcp/internal/serial/port"
)

// ErrFlowControl reports a profile that asked for flow control. It is a
// sentinel because the MCP layer maps it to its own error code: a caller
// branching on serial_flow_control_unsupported must get one answer wherever
// the refusal surfaced (create, update, or a session start re-validating the
// profile), not operation_failed plus prose.
var ErrFlowControl = errors.New("serial flow control unsupported")

// Spec holds the serial-specific fields of a host profile. It is embedded
// (squashed) into sshconfig.Entry, so the TOML keys stay flat — device = …, not
// [serial] device = … — exactly the way the SSH DialSpec is embedded.
//
// It carries no kind, description or default_approval: those are host-profile
// properties every kind shares, and they live on sshconfig.Entry. Splitting the
// two is what lets one struct serve all three kinds without a serial field
// leaking into an SSH profile's schema.
type Spec struct {
	Device string `toml:"device"`
	Baud   int    `toml:"baud"`

	DataBits int           `toml:"data_bits,omitempty"`
	StopBits port.StopBits `toml:"stop_bits,omitempty"`
	Parity   string        `toml:"parity,omitempty"`
	DTR      string        `toml:"dtr,omitempty"`
	RTS      string        `toml:"rts,omitempty"`

	// FlowControl is retained so a profile that asks for it gets a clear
	// refusal. The library has no flow-control support; silently ignoring the
	// field would drop data at high baud rates, which is worse than an error.
	FlowControl string `toml:"flow_control,omitempty"`

	TxEOL       string `toml:"tx_eol,omitempty"`
	RxNormalize *bool  `toml:"rx_normalize,omitempty"`
	LocalEcho   bool   `toml:"local_echo,omitempty"`
	// Display is how the device's bytes are PRESENTED to clients (auto, text,
	// hex or b64). It is deliberately not named "encoding": that word, on the
	// read path, means a character-set decoder (GBK, UTF-16), which is a
	// different axis — a GBK device still needs a display choice for the bytes
	// its protocol emits. The transcript keeps raw bytes either way.
	Display string `toml:"display,omitempty"`
	// Charset is the character set the DEVICE speaks: the decode step that
	// turns GBK bytes into text before any presentation choice runs. "auto"
	// (the default) sniffs each window — BOM first, then a round-trip probe —
	// and falls back to UTF-8 when nothing matches; a firmware's encoding is
	// usually a known fact, so naming it explicitly is the reliable choice.
	// Validated against the encoding package's table, so a profile with
	// "gbkrate" is refused rather than silently undecoded.
	Charset string `toml:"charset,omitempty"`
}

// TxEOL values. "none" means the Enter key sends a bare CR — the same byte the
// terminal sends today — and NOT "send no line ending at all".
const (
	TxEOLCR   = "cr"
	TxEOLCRLF = "crlf"
	TxEOLLF   = "lf"
	TxEOLNone = "none"
)

// Display values. They describe how bytes are presented, not how they are
// stored. Aliases of the display layer's vocabulary rather than a second copy
// of it: the value stored in the profile is the value EncodeForDisplay switches
// on, so one string written in TOML and one string matched at the outlet must
// be the same string by construction, not by two switches agreeing today.
const (
	DisplayAuto = encoding.Auto
	DisplayText = encoding.Text
	DisplayHex  = encoding.Hex
	DisplayB64  = encoding.B64
)

// CharsetAuto is the profile value that sniffs each window instead of naming
// the encoding. It shares the literal with the display layer's auto on
// purpose: both mean "decide from the bytes", and a profile that says auto in
// either field is making the same kind of statement.
const CharsetAuto = encoding.CharsetAuto

// Defaults applied when a field is absent. Baud has none: it is required,
// because there is no rate that is safe to assume for an unknown device.
const (
	DefaultDataBits = 8
	DefaultTxEOL    = TxEOLCR
	DefaultDisplay  = DisplayAuto
	DefaultCharset  = CharsetAuto
)

func flowControlError(got string) error {
	return fmt.Errorf("%w: %q is not supported (only %q); "+
		"the device would silently drop data without it", ErrFlowControl, got, "none")
}

func errf(format string, args ...any) error {
	return fmt.Errorf(format, args...)
}

func lower(s string) string { return strings.ToLower(s) }

func trimSpace(s string) string { return strings.TrimSpace(s) }

// ApplyDefaults fills the fields that have one. Exported because the profile's
// owner (sshconfig) applies it before validating: a profile that omits
// everything but device and baud is complete by the time the checks run.
func (s *Spec) ApplyDefaults() { s.applyDefaults() }

// applyDefaults fills the fields that have one. It runs before validation so a
// profile that omits everything but device and baud is complete by the time the
// checks run.
func (s *Spec) applyDefaults() {
	s.Device = trimSpace(s.Device)
	s.Parity = trimSpace(lower(s.Parity))
	s.DTR = trimSpace(lower(s.DTR))
	s.RTS = trimSpace(lower(s.RTS))
	s.FlowControl = trimSpace(lower(s.FlowControl))
	s.TxEOL = trimSpace(lower(s.TxEOL))
	s.Display = trimSpace(lower(s.Display))
	s.Charset = trimSpace(lower(s.Charset))
	s.StopBits = port.StopBits(trimSpace(string(s.StopBits)))

	if s.DataBits == 0 {
		s.DataBits = DefaultDataBits
	}
	if s.StopBits == "" {
		s.StopBits = port.StopBitsOne
	}
	if s.Parity == "" {
		s.Parity = port.ParityNone
	}
	if s.DTR == "" {
		s.DTR = port.LineKeep
	}
	if s.RTS == "" {
		s.RTS = port.LineKeep
	}
	if s.TxEOL == "" {
		s.TxEOL = DefaultTxEOL
	}
	if s.Display == "" {
		s.Display = DefaultDisplay
	}
	if s.Charset == "" {
		s.Charset = DefaultCharset
	}
}

// Validate checks the device fields as a whole.
//
// Fields are checked together, not one at a time: the failures that matter
// (flow control, an impossible stop-bit/parity pair) are properties of the
// combination, and a profile is only usable if all of it is. The kind check is
// the caller's: sshconfig.ParseAndValidate dispatches on kind before asking a
// Spec to validate, and a Spec cannot know which profile carried it.
func (s *Spec) Validate() error {
	// Flow control first: it is the one field that must be refused before any
	// device is touched, and its error is more specific than a generic one.
	if s.FlowControl != "" && s.FlowControl != "none" {
		return flowControlError(s.FlowControl)
	}

	if err := s.PortConfig().Validate(); err != nil {
		return err
	}

	switch s.TxEOL {
	case TxEOLCR, TxEOLCRLF, TxEOLLF, TxEOLNone:
	default:
		return errf("tx_eol must be cr, crlf, lf or none, got %q", s.TxEOL)
	}

	if !encoding.DisplayValid(s.Display) {
		return errf("display must be auto, text, hex or b64, got %q", s.Display)
	}

	if !encoding.CharsetValid(s.Charset) {
		return errf("charset must be %q or one of: %s (got %q)",
			"auto", encoding.SupportedCharsets(), s.Charset)
	}

	return nil
}

// PortConfig projects the profile onto what the port layer needs to open the
// device. Fields that describe bytes (tx_eol, display) stay behind: they are
// not port settings.
func (s *Spec) PortConfig() port.Config {
	return port.Config{
		Device:   s.Device,
		Baud:     s.Baud,
		DataBits: s.DataBits,
		StopBits: s.StopBits,
		Parity:   s.Parity,
		DTR:      s.DTR,
		RTS:      s.RTS,
	}
}

// NormalizeRx reports whether the receive-side line-ending normalization is on.
// It defaults to true, so the field is a pointer: an omitted rx_normalize and an
// explicit `rx_normalize = false` must be distinguishable.
func (s *Spec) NormalizeRx() bool {
	if s.RxNormalize == nil {
		return true
	}
	return *s.RxNormalize
}

// Empty reports whether the spec carries no serial configuration at all — the
// zero value an SSH or internal profile has because its TOML never named a
// device. Callers use it to refuse serial fields on the wrong kind.
func (s *Spec) Empty() bool {
	return s.Device == "" && s.Baud == 0
}
