package sshconfig

import (
	"fmt"
	"github.com/BurntSushi/toml"
	"regexp"
	"strings"

	"github.com/open-mcp-ai/termcp/internal/serial"
	"github.com/open-mcp-ai/termcp/internal/serial/port"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshclient"
)

const maxConfigFileSize = 256 * 1024

const KindInternal = "internal"
const KindRemote = "remote"
const KindSerial = "serial"

var nameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

// DialSpec holds the dial-time SSH fields shared by a remote Entry and a JumpSpec.
// It is embedded with toml:",squash" so TOML keys stay flat (host = ..., not [dialspec]).
type DialSpec struct {
	Host               string `toml:"host,omitempty"`
	Port               int    `toml:"port,omitempty"`
	User               string `toml:"user,omitempty"`
	Password           string `toml:"password,omitempty"`
	PrivateKey         string `toml:"private_key,omitempty"`
	KeyPassphrase      string `toml:"key_passphrase,omitempty"`
	TrustUnknownHost   *bool  `toml:"trust_unknown_host,omitempty"`
	KnownHosts         string `toml:"known_hosts,omitempty"`
	DialTimeoutSeconds int    `toml:"dial_timeout_seconds,omitempty"`
	Proxy              string `toml:"proxy,omitempty"` // e.g. "socks5://user:pass@host:port"
}

// Entry is stored as data-dir/ssh_configs/<name>/config.toml
type Entry struct {
	Kind         string `toml:"kind"` // "internal" | "remote"
	Description  string `toml:"description,omitempty"`
	DefaultShell string `toml:"default_shell,omitempty"`
	DefaultMode  string `toml:"default_mode,omitempty"`
	// DefaultApproval makes every session created from this profile start with
	// review mode on. It is a property of the connection because the decision is
	// about the host, not about one session: a production box is the reason to
	// want every write reviewed, and remembering to flip the switch after each
	// `termcp://entry` launch is exactly the step that gets forgotten.
	DefaultApproval bool `toml:"default_approval,omitempty"`
	DialSpec        `toml:",squash"`
	Jump            *JumpSpec `toml:"jump,omitempty"` // optional bastion chain (ProxyJump)

	// Serial device fields, flat like DialSpec's. They are only meaningful when
	// Kind is "serial"; their zero value is what an internal or remote profile
	// carries because its document never named a device, which is how a serial
	// field on the wrong kind is detected. The TOML key is "display", not
	// "encoding": encoding on the read path means a character-set decoder, and
	// the two axes must not share a name.
	Device      string        `toml:"device,omitempty"`
	Baud        int           `toml:"baud,omitempty"`
	DataBits    int           `toml:"data_bits,omitempty"`
	StopBits    port.StopBits `toml:"stop_bits,omitempty"`
	Parity      string        `toml:"parity,omitempty"`
	DTR         string        `toml:"dtr,omitempty"`
	RTS         string        `toml:"rts,omitempty"`
	FlowControl string        `toml:"flow_control,omitempty"`
	TxEOL       string        `toml:"tx_eol,omitempty"`
	RxNormalize *bool         `toml:"rx_normalize,omitempty"`
	LocalEcho   bool          `toml:"local_echo,omitempty"`
	Display     string        `toml:"display,omitempty"`
	// Charset is what the device speaks ("auto" = sniff, or a name the encoding
	// table maps). Flat like the rest of the serial fields; the decode step the
	// display choice composes with.
	Charset string `toml:"charset,omitempty"`
}

// JumpSpec is a self-contained bastion hop: a DialSpec plus a nested Jump for
// multi-hop chains. It lacks Entry's session-level fields (kind/shell/mode) —
// only the final target runs a shell. Stored inline so the config is
// self-contained at the cost of duplicated bastion credentials across entries.
type JumpSpec struct {
	DialSpec `toml:",squash"`
	Jump     *JumpSpec `toml:"jump,omitempty"`
}

// ValidateName checks directory/config id (reserved: internal is allowed as id).
func ValidateName(name string) error {
	name = strings.TrimSpace(name)
	if !nameRe.MatchString(name) {
		return fmt.Errorf("invalid ssh config name %q (letters, digits, _, -; max 64)", name)
	}
	return nil
}

// ParseAndValidate decodes TOML and validates by kind.
func ParseAndValidate(data []byte) (*Entry, error) {
	if len(data) > maxConfigFileSize {
		return nil, fmt.Errorf("config file too large (max %d bytes)", maxConfigFileSize)
	}
	var e Entry
	if err := toml.Unmarshal(data, &e); err != nil {
		return nil, fmt.Errorf("config toml: %w", err)
	}
	k := strings.TrimSpace(strings.ToLower(e.Kind))
	if k == "" {
		// ErrInvalid, not a bare error: a profile with no kind is the CALLER's
		// document being incomplete, and answering it with operation_failed (500)
		// reads like an outage — a caller that retries on 5xx retries a config that
		// will never work.
		return nil, fmt.Errorf("config must set \"kind\" to %q, %q or %q: %w", KindInternal, KindRemote, KindSerial, ErrInvalid)
	}
	e.Kind = k
	switch k {
	case KindSerial:
		if err := refuseDialFieldsOnSerial(&e); err != nil {
			return nil, err
		}
		if err := refuseShellFieldsOnSerial(&e); err != nil {
			return nil, err
		}
		e.applySerialDefaults()
		if err := e.validateSerial(); err != nil {
			return nil, err
		}
		return &e, nil
	case KindInternal:
		// A DIAL FIELD ON AN INTERNAL PROFILE DOES NOTHING, AND SAYING SO IS THE FIX.
		// `kind = "internal"` means the built-in loopback connection; host/user/port
		// beside it parse cleanly, are never read, and the session connects to
		// localhost anyway. That is the one combination where the caller's mental
		// model and reality diverge with no signal at all: they believe they are
		// reaching 10.99.99.99.
		if err := refuseDialFieldsOnInternal(&e); err != nil {
			return nil, err
		}
		if err := refuseSerialFieldsOnNonSerial(&e, KindInternal); err != nil {
			return nil, err
		}
		if err := validateDefaultMode(&e); err != nil {
			return nil, err
		}
		return &e, nil
	case KindRemote:
		if err := refuseSerialFieldsOnNonSerial(&e, KindRemote); err != nil {
			return nil, err
		}
		if err := validateDialSpec(&e.DialSpec, "remote config"); err != nil {
			return nil, err
		}
		if err := validateJump(e.Jump); err != nil {
			return nil, err
		}
		if err := validateDefaultMode(&e); err != nil {
			return nil, err
		}
		return &e, nil
	default:
		return nil, fmt.Errorf("unknown kind %q (use %q, %q or %q): %w", e.Kind, KindInternal, KindRemote, KindSerial, ErrInvalid)
	}
}

// refuseDialFieldsOnInternal rejects a loopback profile that carries dial settings.
//
// Names the fields rather than saying "unexpected field": the caller has to delete
// them, and "host, user, port" is the edit list.
func refuseDialFieldsOnInternal(e *Entry) error {
	var found []string
	if strings.TrimSpace(e.Host) != "" {
		found = append(found, "host")
	}
	if strings.TrimSpace(e.User) != "" {
		found = append(found, "user")
	}
	if e.Port != 0 {
		found = append(found, "port")
	}
	if strings.TrimSpace(e.Password) != "" {
		found = append(found, "password")
	}
	if strings.TrimSpace(e.PrivateKey) != "" {
		found = append(found, "private_key")
	}
	if e.Jump != nil {
		found = append(found, "jump")
	}
	if len(found) == 0 {
		return nil
	}
	return fmt.Errorf("kind = %q is the built-in loopback connection, so %s would be accepted and ignored; remove it or use kind = %q: %w",
		KindInternal, strings.Join(found, ", "), KindRemote, ErrInvalid)
}

// refuseDialFieldsOnSerial rejects a serial profile that carries SSH dial
// settings. The message names the fields rather than saying "unexpected": the
// caller has to delete them, and the list is the edit.
func refuseDialFieldsOnSerial(e *Entry) error {
	var found []string
	add := func(name, v string) {
		if strings.TrimSpace(v) != "" {
			found = append(found, name)
		}
	}
	add("host", e.Host)
	add("user", e.User)
	if e.Port != 0 {
		found = append(found, "port")
	}
	add("password", e.Password)
	add("private_key", e.PrivateKey)
	add("key_passphrase", e.KeyPassphrase)
	add("proxy", e.Proxy)
	add("known_hosts", e.KnownHosts)
	if e.TrustUnknownHost != nil {
		found = append(found, "trust_unknown_host")
	}
	if e.DialTimeoutSeconds != 0 {
		found = append(found, "dial_timeout_seconds")
	}
	if e.Jump != nil {
		found = append(found, "jump")
	}
	if len(found) == 0 {
		return nil
	}
	return fmt.Errorf("kind = %q names a device on this machine, so %s would be accepted and ignored; remove it: %w",
		KindSerial, strings.Join(found, ", "), ErrInvalid)
}

// refuseShellFieldsOnSerial rejects a serial profile that names a shell or a
// shell mode: a serial device has no process to run and no terminal to size,
// so both fields would be read by callers that can never honour them.
func refuseShellFieldsOnSerial(e *Entry) error {
	var found []string
	if strings.TrimSpace(e.DefaultShell) != "" {
		found = append(found, "default_shell")
	}
	if strings.TrimSpace(e.DefaultMode) != "" {
		found = append(found, "default_mode")
	}
	if len(found) == 0 {
		return nil
	}
	return fmt.Errorf("kind = %q has no shell, so %s would be accepted and ignored; remove it: %w",
		KindSerial, strings.Join(found, ", "), ErrInvalid)
}

// refuseSerialFieldsOnNonSerial rejects internal/remote profiles that carry
// serial device fields: the fields would be accepted and ignored, and a profile
// that silently dials a host while naming a device is the internal/remote
// mirror of the mistake refuseDialFieldsOnSerial catches.
func refuseSerialFieldsOnNonSerial(e *Entry, kind string) error {
	if strings.TrimSpace(e.Device) == "" && e.Baud == 0 {
		return nil
	}
	var found []string
	if strings.TrimSpace(e.Device) != "" {
		found = append(found, "device")
	}
	if e.Baud != 0 {
		found = append(found, "baud")
	}
	return fmt.Errorf("kind = %q dials a host, so serial fields (%s) would be accepted and ignored; remove them or use kind = %q: %w",
		kind, strings.Join(found, ", "), KindSerial, ErrInvalid)
}

func validateDefaultMode(e *Entry) error {
	dm := strings.TrimSpace(strings.ToLower(e.DefaultMode))
	if dm == "" {
		e.DefaultMode = ""
		return nil
	}
	if dm != "pty" && dm != "pipe" {
		return fmt.Errorf("default_mode must be \"pty\" or \"pipe\", got %q: %w", e.DefaultMode, ErrInvalid)
	}
	e.DefaultMode = dm
	return nil
}

// validateDialSpec checks the shared dial fields. label prefixes error messages
// (e.g. "remote config", `jump "host"`).
func validateDialSpec(d *DialSpec, label string) error {
	d.Host = strings.TrimSpace(d.Host)
	d.User = strings.TrimSpace(d.User)
	if d.Host == "" {
		return fmt.Errorf("%s must set \"host\": %w", label, ErrInvalid)
	}
	if d.User == "" {
		return fmt.Errorf("%s must set \"user\": %w", label, ErrInvalid)
	}
	d.Password = strings.TrimSpace(d.Password)
	d.PrivateKey = strings.TrimSpace(d.PrivateKey)
	if d.Password == "" && d.PrivateKey == "" {
		return fmt.Errorf("%s needs password or private_key: %w", label, ErrInvalid)
	}
	if d.Port < 0 || d.Port > 65535 {
		return fmt.Errorf("%s invalid port %d: %w", label, d.Port, ErrInvalid)
	}
	if _, err := sshclient.ParseProxyURL(d.Proxy); err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	// A TIMEOUT ABOVE THE CAP IS NOT A TIMEOUT, and a negative one is a typo. Zero stays
	// legal and means "the default" (an entry written before this field existed has no value
	// at all); anything else has to be inside the range the dialler honours, or the caller is
	// setting a number that will be replaced without a word at dial time.
	//
	// A SHORT TIMEOUT IS LEGAL. The floor is 1 second, not the 30 second default: failing
	// fast is a legitimate thing to ask for, and refusing it would leave a script with no way
	// to bound a dial. The dialler owns the range (session.ClampDialTimeoutSeconds) and this
	// asks it rather than restating the numbers — two copies of a bound drift apart, and the
	// copy that drifts is always the one that is not enforcing anything.
	// NEGATIVE IS REFUSED; EVERYTHING ELSE IS THE CALLER'S NUMBER.
	//
	// A dial timeout above the recommendation is a request this server has no basis to
	// overrule — a slow link or a long bastion chain is a real reason to want one — so it
	// is stored and honoured. Below zero is a typo, not a wish, and it is the one value
	// the dialler has to replace to have anything to do at all.
	if d.DialTimeoutSeconds < 0 {
		return fmt.Errorf("%s dial_timeout_seconds must not be negative, got %d "+
			"(0 means the default, %ds; %ds is the value to stay under): %w",
			label, d.DialTimeoutSeconds, session.DefaultDialTimeoutSeconds,
			session.RecommendedDialTimeoutSeconds, ErrInvalid)
	}
	return nil
}

// validateJump recursively validates a bastion chain. nil = no jump.
func validateJump(j *JumpSpec) error {
	if j == nil {
		return nil
	}
	j.Host = strings.TrimSpace(j.Host)
	label := fmt.Sprintf("jump %q", j.Host)
	if err := validateDialSpec(&j.DialSpec, label); err != nil {
		return err
	}
	return validateJump(j.Jump)
}

// EffectiveCommand resolves command+args when the client sends nothing.
// If the client provides a non-empty command or any args, ent is ignored for the command.
func EffectiveCommand(ent *Entry, cmd string, args []string) (string, []string) {
	cmd = strings.TrimSpace(cmd)
	if cmd != "" || len(args) > 0 {
		return cmd, args
	}
	if ent != nil {
		ds := strings.TrimSpace(ent.DefaultShell)
		if ds != "" {
			f := strings.Fields(ds)
			if len(f) > 0 {
				return f[0], f[1:]
			}
		}
	}
	return "", nil
}

// EffectiveDefaultShell returns the profile's default shell line (may carry
// arguments), or "" when the profile sets none. A session keeps this so shells
// opened later on the same connection resolve their shell the same way the
// first one did: caller's command, then the profile, then the server's own.
func EffectiveDefaultShell(ent *Entry) string {
	if ent == nil {
		return ""
	}
	return strings.TrimSpace(ent.DefaultShell)
}

// EffectiveMode returns pty or pipe; default is pty unless ent.DefaultMode overrides when mode is empty.
func EffectiveMode(ent *Entry, mode string) string {
	m := strings.TrimSpace(strings.ToLower(mode))
	if m != "" {
		return m
	}
	if ent != nil {
		dm := strings.TrimSpace(strings.ToLower(ent.DefaultMode))
		if dm == "pty" || dm == "pipe" {
			return dm
		}
	}
	return "pty"
}

// EffectiveApproval reports whether a session created from ent starts gated.
//
// A nil entry (no profile, or one that failed to load) is not gated: the field
// can only say "on" for a profile that was actually read, so a missing profile
// cannot silently gate a session whose owner never asked for it. A caller that
// wants the safe answer when a profile is missing must say so itself.
func EffectiveApproval(ent *Entry) bool {
	return ent != nil && ent.DefaultApproval
}

// InternalTemplate is the built-in loopback SSH entry.
func InternalTemplate() []byte {
	return []byte("# termcp loopback SSH config (TOML)\nkind = \"internal\"\ndescription = \"Built-in termcp loopback SSH (no host credentials).\"\n")
}

// ParseInternalOverride layers a stored internal config over the built-in
// defaults.
//
// The point of layering rather than replacing: the internal profile has real
// defaults (its kind, its description) that a user setting one field should not
// have to restate. TOML decoding only writes the keys the document actually
// contains, so starting from the template gives "absent key keeps the default,
// present key overrides it" for free — which is exactly the rule a config file
// is expected to follow.
func ParseInternalOverride(data []byte, base *Entry) (*Entry, error) {
	if len(data) > maxConfigFileSize {
		return nil, fmt.Errorf("config file too large (max %d bytes)", maxConfigFileSize)
	}
	e := *base
	if err := toml.Unmarshal(data, &e); err != nil {
		return nil, fmt.Errorf("config toml: %w", err)
	}
	k := strings.TrimSpace(strings.ToLower(e.Kind))
	if k != KindInternal {
		// A stored override for the loopback profile cannot turn it into a
		// remote one: the name means the built-in connection, and silently
		// honouring kind="remote" here would make "internal" dial elsewhere.
		return nil, fmt.Errorf("the internal profile must keep kind = %q, got %q", KindInternal, k)
	}
	e.Kind = KindInternal
	if err := refuseDialFieldsOnInternal(&e); err != nil {
		return nil, err
	}
	if err := validateDefaultMode(&e); err != nil {
		return nil, err
	}
	return &e, nil
}

// SerialFromEntry returns the device spec of a serial host profile. The caller
// has already validated the entry (ParseAndValidate dispatched on kind), so the
// error here covers only the wrong-kind call — the equivalent of handing a
// remote profile to a serial dialler.
func SerialFromEntry(e *Entry) (*serial.Spec, error) {
	if e == nil || e.Kind != KindSerial {
		return nil, fmt.Errorf("profile is not kind = %q: %w", KindSerial, ErrInvalid)
	}
	s := e.serialSpec()
	s.ApplyDefaults()
	return &s, nil
}

// serialSpec projects the flat profile fields onto the device vocabulary the
// serial package speaks. The projection is total: every field moves, so a field
// added to one side without the other fails the round-trip test rather than
// being silently dropped.
func (e *Entry) serialSpec() serial.Spec {
	return serial.Spec{
		Device:      e.Device,
		Baud:        e.Baud,
		DataBits:    e.DataBits,
		StopBits:    e.StopBits,
		Parity:      e.Parity,
		DTR:         e.DTR,
		RTS:         e.RTS,
		FlowControl: e.FlowControl,
		TxEOL:       e.TxEOL,
		RxNormalize: e.RxNormalize,
		LocalEcho:   e.LocalEcho,
		Display:     e.Display,
		Charset:     e.Charset,
	}
}

// applySerialDefaults writes the spec's defaults back onto the flat fields, so
// a parsed serial profile is complete as stored, not only as validated.
func (e *Entry) applySerialDefaults() {
	s := e.serialSpec()
	s.ApplyDefaults()
	e.fromSerialSpec(&s)
}

// validateSerial validates the serial fields in place. A device-side refusal is
// the caller's document being incomplete, so it is surfaced as ErrInvalid (400)
// rather than as the raw port error the transport layer would answer 500 with.
func (e *Entry) validateSerial() error {
	s := e.serialSpec()
	if err := s.Validate(); err != nil {
		return fmt.Errorf("invalid serial config: %w (%w)", ErrInvalid, err)
	}
	return nil
}

// fromSerialSpec copies a spec back onto the flat fields (the inverse of
// serialSpec, used after defaults were applied).
func (e *Entry) fromSerialSpec(s *serial.Spec) {
	e.Device = s.Device
	e.Baud = s.Baud
	e.DataBits = s.DataBits
	e.StopBits = s.StopBits
	e.Parity = s.Parity
	e.DTR = s.DTR
	e.RTS = s.RTS
	e.FlowControl = s.FlowControl
	e.TxEOL = s.TxEOL
	e.RxNormalize = s.RxNormalize
	e.LocalEcho = s.LocalEcho
	e.Display = s.Display
	e.Charset = s.Charset
}

// RemoteFromEntry converts an sshconfig Entry into session.RemoteSSH dial settings,
// including the recursive bastion chain from Entry.Jump.
func RemoteFromEntry(e *Entry, configDir string) (*session.RemoteSSH, error) {
	r := dialSpecToRemote(&e.DialSpec)
	r.Jump = jumpToRemote(e.Jump)
	return r, nil
}

func dialSpecToRemote(d *DialSpec) *session.RemoteSSH {
	pem := strings.TrimSpace(d.PrivateKey)
	trust := true
	if d.TrustUnknownHost != nil {
		trust = *d.TrustUnknownHost
	}
	port := d.Port
	if port == 0 {
		port = 22
	}
	proxy, _ := sshclient.ParseProxyURL(d.Proxy) // validated upstream by ParseAndValidate
	return &session.RemoteSSH{
		Host:               d.Host,
		Port:               port,
		User:               d.User,
		Password:           d.Password,
		PrivateKey:         pem,
		KeyPassphrase:      d.KeyPassphrase,
		TrustUnknownHost:   trust,
		KnownHosts:         d.KnownHosts,
		DialTimeoutSeconds: d.DialTimeoutSeconds,
		Proxy:              proxy,
	}
}

func jumpToRemote(j *JumpSpec) *session.RemoteSSH {
	if j == nil {
		return nil
	}
	r := dialSpecToRemote(&j.DialSpec)
	r.Jump = jumpToRemote(j.Jump)
	return r
}
