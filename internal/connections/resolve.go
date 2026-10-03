// Package connections resolves a profile name to the kind of connection it
// describes.
//
// A name can be an ssh profile, a serial profile, or nothing. Those are the only
// three answers, and every entry point that takes a profile name — session_start,
// the termcp:// locator, the Web UI — must give the same one. Resolving in one
// place is what keeps that true: when each caller looked in whichever directory
// it happened to know about, a name present in both would resolve differently
// depending on who asked.
package connections

import (
	"errors"
	"fmt"
	"strings"

	"github.com/open-mcp-ai/termcp/internal/serial"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
)

// Kind names the profile directories a name can resolve to.
const (
	KindSSH    = "ssh"
	KindSerial = "serial"
)

// ErrNotFound reports that no profile with the given name exists.
var ErrNotFound = errors.New("connection profile not found")

// ErrAmbiguous reports that the name exists as both an ssh and a serial
// profile. It is a configuration error, not something to resolve by preference:
// whichever directory won, the other profile would be silently unreachable.
var ErrAmbiguous = errors.New("connection profile name is ambiguous")

// Resolved is the outcome of a successful resolution. Exactly one of the two
// profile pointers is set.
type Resolved struct {
	Name string
	Kind string

	// SSH is set when Kind is KindSSH. It carries the loaded entry so callers
	// do not immediately re-read the same file.
	SSH *sshconfig.Entry

	// Serial is set when Kind is KindSerial.
	Serial *serial.Entry
}

// Resolver answers profile-name lookups against both stores.
//
// Both stores may be nil; a nil store means that kind is unavailable on this
// instance (for example --no-serial, or an ssh-only deployment), and its
// directory is simply not consulted.
type Resolver struct {
	SSH    *sshconfig.Store
	Serial *serial.Store
}

// Resolve reports what kind of connection a name describes.
//
// The three outcomes are distinct: a name in both directories is an error rather
// than a preference, because picking one would make the other profile
// unreachable with no indication that it exists.
func (r *Resolver) Resolve(name string) (*Resolved, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("%w: name is required", ErrNotFound)
	}

	sshEntry, sshErr := r.loadSSH(name)
	serialEntry, serialErr := r.loadSerial(name)

	sshFound := sshErr == nil
	serialFound := serialErr == nil

	switch {
	case sshFound && serialFound:
		return nil, fmt.Errorf("%w: %q exists as both an ssh profile (%s) and a serial profile (%s)",
			ErrAmbiguous, name, r.sshPath(name), r.serialPath(name))

	case sshFound:
		return &Resolved{Name: name, Kind: KindSSH, SSH: sshEntry}, nil

	case serialFound:
		return &Resolved{Name: name, Kind: KindSerial, Serial: serialEntry}, nil

	default:
		// Report the underlying reason when it is more than absence: a profile
		// that exists but fails to parse is a different problem from one that
		// does not exist, and the caller needs to be able to tell them apart.
		if err := firstRealError(sshErr, serialErr); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %q", ErrNotFound, name)
	}
}

// loadSSH loads an ssh profile, reporting ErrNotFound when it does not exist.
func (r *Resolver) loadSSH(name string) (*sshconfig.Entry, error) {
	if r.SSH == nil {
		return nil, fmt.Errorf("ssh profile store is not configured")
	}
	entry, err := r.SSH.Load(name)
	if err != nil {
		if errors.Is(err, sshconfig.ErrNotFound) {
			return nil, err
		}
		// A broken profile still means the name is taken; the caller has to see
		// the parse error, not a misleading "not found".
		return nil, err
	}
	return entry, nil
}

// loadSerial loads a serial profile, reporting ErrNotFound when it does not exist.
func (r *Resolver) loadSerial(name string) (*serial.Entry, error) {
	if r.Serial == nil {
		return nil, fmt.Errorf("serial profile store is not configured")
	}
	entry, err := r.Serial.Load(name)
	if err != nil {
		return nil, err
	}
	return entry, nil
}

// firstRealError returns whichever error is a genuine failure rather than a
// plain absence.
func firstRealError(sshErr, serialErr error) error {
	for _, err := range []error{sshErr, serialErr} {
		if err == nil {
			continue
		}
		if errors.Is(err, sshconfig.ErrNotFound) || errors.Is(err, serial.ErrNotFound) {
			continue
		}
		if strings.Contains(err.Error(), "store is not configured") {
			continue
		}
		return err
	}
	return nil
}

// sshPath and serialPath name the file a profile would live in, for the
// ambiguity message. They are best effort: the point is to tell a person where
// to look, not to prove the file is there.
func (r *Resolver) sshPath(name string) string {
	if r.SSH == nil {
		return "(ssh store not configured)"
	}
	return r.SSH.ConfigDir(name) + "/config.toml"
}

func (r *Resolver) serialPath(name string) string {
	if r.Serial == nil {
		return "(serial store not configured)"
	}
	return r.Serial.ConfigDir(name) + "/config.toml"
}
