// Package serial holds a serial connection profile: its schema, its validation,
// and the store that reads and writes it under data-dir/serial_configs.
//
// A serial profile is not an sshconfig.Entry. It shares the file layout and the
// naming rules, and nothing else: there is no host to dial, no credentials, and
// the connection is a device on this machine. Keeping the two schemas apart is
// what makes the cross-directory name check meaningful — a name that exists in
// both places is ambiguous, not a fallback.
package serial

import (
	"errors"
	"fmt"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/open-mcp-ai/termcp/internal/encoding"
	"github.com/open-mcp-ai/termcp/internal/serial/port"
)

// ErrFlowControl reports a profile that asked for flow control. It is a
// sentinel because the MCP layer maps it to its own error code: a caller
// branching on serial_flow_control_unsupported must get one answer wherever
// the refusal surfaced (create, update, or a session start re-validating the
// profile), not operation_failed plus prose.
var ErrFlowControl = errors.New("serial flow control unsupported")

const maxConfigFileSize = 256 * 1024

// Kind is the fixed kind of every serial profile. A file whose kind says
// otherwise is rejected rather than reinterpreted: the value is what tells a
// reader which schema to expect.
const Kind = "serial"

// ReservedName is refused for serial profiles. "internal" is the built-in SSH
// loopback entry, and a serial profile by that name would collide with it in
// every place a profile name is resolved.
const ReservedName = "internal"

// Entry is the parsed profile. Field names follow the TOML keys in the design
// document, so a profile reads the same way in the file and in this struct.
type Entry struct {
	Kind        string `toml:"kind"`
	Description string `toml:"description,omitempty"`

	Device   string        `toml:"device"`
	Baud     int           `toml:"baud"`
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
	Encoding    string `toml:"encoding,omitempty"`

	DefaultApproval bool `toml:"default_approval,omitempty"`
}

// TxEOL values. "none" means the Enter key sends a bare CR — the same byte the
// terminal sends today — and NOT "send no line ending at all".
const (
	TxEOLCR   = "cr"
	TxEOLCRLF = "crlf"
	TxEOLLF   = "lf"
	TxEOLNone = "none"
)

// Encoding values. They describe how bytes are presented, not how they are
// stored: the transcript keeps raw bytes either way.
//
// They are aliases of the display layer's vocabulary rather than a second copy
// of it: the value stored in the profile is the value EncodeForDisplay switches
// on, so one string written in TOML and one string matched at the outlet must
// be the same string by construction, not by two switches agreeing today.
const (
	EncodingAuto = encoding.Auto
	EncodingText = encoding.Text
	EncodingHex  = encoding.Hex
	EncodingB64  = encoding.B64
)

// Defaults applied when a field is absent. Baud has none: it is required,
// because there is no rate that is safe to assume for an unknown device.
const (
	DefaultDataBits = 8
	DefaultTxEOL    = TxEOLCR
	DefaultEncoding = EncodingAuto
)

// sshOnlyKeys are fields that belong to an sshconfig profile. Their presence in
// a serial profile is refused: a file that carries them was probably written for
// the wrong kind, and silently ignoring a host or a password would hide that.
var sshOnlyKeys = []string{
	"host", "port", "user", "password", "private_key", "key_passphrase",
	"jump", "proxy", "known_hosts", "trust_unknown_host",
	"default_shell", "default_mode",
}

// ParseAndValidate decodes a serial profile and validates it.
//
// Decoding is strict: an unrecognized key is an error rather than a default.
// That is the point of using MetaData.Undecoded — a profile with "baudrate" or
// "parityy" would otherwise start a session at 115200 with no parity and give
// no hint that the setting was never read.
func ParseAndValidate(data []byte) (*Entry, error) {
	if len(data) > maxConfigFileSize {
		return nil, fmt.Errorf("serial config too large (max %d bytes)", maxConfigFileSize)
	}

	var e Entry
	md, err := toml.Decode(string(data), &e)
	if err != nil {
		return nil, fmt.Errorf("serial config toml: %w", err)
	}

	// The SSH-field check runs first so the message names the likely mistake
	// ("that is an SSH field") instead of the generic "unknown field", which is
	// all Undecoded can say.
	if err := rejectSSHOnlyKeys(md); err != nil {
		return nil, err
	}

	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, 0, len(undecoded))
		for _, k := range undecoded {
			keys = append(keys, k.String())
		}
		return nil, fmt.Errorf("serial config has unknown field(s): %s", strings.Join(keys, ", "))
	}

	e.applyDefaults()

	if err := e.Validate(); err != nil {
		return nil, err
	}
	return &e, nil
}

// rejectSSHOnlyKeys looks for ssh field names explicitly, so the error names the
// actual mistake instead of reporting "unknown field: host" from Undecoded.
func rejectSSHOnlyKeys(md toml.MetaData) error {
	for _, key := range sshOnlyKeys {
		if md.IsDefined(key) {
			return fmt.Errorf("serial config must not set %q: that is an SSH field", key)
		}
	}
	return nil
}

// applyDefaults fills the fields that have one. It runs before validation so a
// profile that omits everything but device and baud is complete by the time the
// checks run.
func (e *Entry) applyDefaults() {
	e.Kind = strings.TrimSpace(strings.ToLower(e.Kind))
	e.Device = strings.TrimSpace(e.Device)
	e.Parity = strings.TrimSpace(strings.ToLower(e.Parity))
	e.DTR = strings.TrimSpace(strings.ToLower(e.DTR))
	e.RTS = strings.TrimSpace(strings.ToLower(e.RTS))
	e.FlowControl = strings.TrimSpace(strings.ToLower(e.FlowControl))
	e.TxEOL = strings.TrimSpace(strings.ToLower(e.TxEOL))
	e.Encoding = strings.TrimSpace(strings.ToLower(e.Encoding))
	e.StopBits = port.StopBits(strings.TrimSpace(string(e.StopBits)))

	if e.DataBits == 0 {
		e.DataBits = DefaultDataBits
	}
	if e.StopBits == "" {
		e.StopBits = port.StopBitsOne
	}
	if e.Parity == "" {
		e.Parity = port.ParityNone
	}
	if e.DTR == "" {
		e.DTR = port.LineKeep
	}
	if e.RTS == "" {
		e.RTS = port.LineKeep
	}
	if e.TxEOL == "" {
		e.TxEOL = DefaultTxEOL
	}
	if e.Encoding == "" {
		e.Encoding = DefaultEncoding
	}
}

// Validate checks the profile as a whole.
//
// Fields are checked together, not one at a time: the failures that matter
// (flow control, an impossible stop-bit/parity pair) are properties of the
// combination, and a profile is only usable if all of it is.
func (e *Entry) Validate() error {
	if e.Kind == "" {
		return fmt.Errorf("serial config must set \"kind\" to %q", Kind)
	}
	if e.Kind != Kind {
		return fmt.Errorf("serial config kind must be %q, got %q", Kind, e.Kind)
	}

	// Flow control first: it is the one field that must be refused before any
	// device is touched, and its error is more specific than a generic one.
	if e.FlowControl != "" && e.FlowControl != "none" {
		return fmt.Errorf("%w: %q is not supported (only \"none\"); "+
			"the device would silently drop data without it", ErrFlowControl, e.FlowControl)
	}

	if err := e.PortConfig().Validate(); err != nil {
		return err
	}

	switch e.TxEOL {
	case TxEOLCR, TxEOLCRLF, TxEOLLF, TxEOLNone:
	default:
		return fmt.Errorf("tx_eol must be cr, crlf, lf or none, got %q", e.TxEOL)
	}

	switch e.Encoding {
	case EncodingAuto, EncodingText, EncodingHex, EncodingB64:
	default:
		return fmt.Errorf("encoding must be auto, text, hex or b64, got %q", e.Encoding)
	}

	return nil
}

// PortConfig projects the profile onto what the port layer needs to open the
// device. Fields that describe bytes (tx_eol, encoding) stay behind: they are
// not port settings.
func (e *Entry) PortConfig() port.Config {
	return port.Config{
		Device:   e.Device,
		Baud:     e.Baud,
		DataBits: e.DataBits,
		StopBits: e.StopBits,
		Parity:   e.Parity,
		DTR:      e.DTR,
		RTS:      e.RTS,
	}
}

// NormalizeRx reports whether the receive-side line-ending normalization is on.
// It defaults to true, so the field is a pointer: an omitted rx_normalize and an
// explicit `rx_normalize = false` must be distinguishable.
func (e *Entry) NormalizeRx() bool {
	if e.RxNormalize == nil {
		return true
	}
	return *e.RxNormalize
}
