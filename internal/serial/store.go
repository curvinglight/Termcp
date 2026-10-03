package serial

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/open-mcp-ai/termcp/internal/sshconfig"
)

// ErrNotFound reports that a named serial profile does not exist. Callers branch
// on it with errors.Is instead of matching the message.
var ErrNotFound = errors.New("serial config not found")

// Store manages data-dir/serial_configs/<name>/config.toml.
//
// It mirrors sshconfig.Store rather than embedding it. The two directories have
// separate lifetimes (an ssh config can hold credentials the serial store must
// never see), and sshconfig.Store's mutex and change hook would tie them
// together for no benefit. What is shared — the name rules and the atomic write
// — is shared by calling the same functions.
type Store struct {
	dataDir  string
	mu       sync.Mutex
	onChange atomic.Value // func(), set via SetOnChange

	// nameChecker reports whether a name is already taken in another profile
	// directory. sshconfig.Store gets the mirrored hook; see SetNameChecker.
	nameChecker atomic.Value // func(string) error
}

// SetOnChange registers a callback fired after Save, Delete, or Rename
// succeeds, so the Web UI refresh path is shared with ssh profiles.
func (s *Store) SetOnChange(fn func()) {
	s.onChange.Store(fn)
}

func (s *Store) notifyChange() {
	fn, _ := s.onChange.Load().(func())
	if fn != nil {
		fn()
	}
}

// SetNameChecker registers a hook that rejects a name already used by another
// profile kind.
//
// A name existing in both ssh_configs/ and serial_configs/ is ambiguous: every
// caller that resolves a bare profile name would have to guess which one was
// meant. Refusing it at write time keeps that ambiguity from being created,
// rather than resolving it differently in each of the three call sites.
func (s *Store) SetNameChecker(fn func(string) error) {
	s.nameChecker.Store(fn)
}

func (s *Store) checkName(name string) error {
	fn, _ := s.nameChecker.Load().(func(string) error)
	if fn == nil {
		return nil
	}
	return fn(name)
}

// NewStore returns a Store rooted at dataDir.
func NewStore(dataDir string) *Store {
	d := filepath.Clean(strings.TrimSpace(dataDir))
	if d == "" {
		d = "."
	}
	abs, err := filepath.Abs(d)
	if err != nil {
		abs = d
	}
	return &Store{dataDir: abs}
}

func (s *Store) root() string {
	return filepath.Join(s.dataDir, "serial_configs")
}

// ConfigDir returns the directory holding a named profile's config.toml.
func (s *Store) ConfigDir(name string) string {
	return filepath.Join(s.root(), name)
}

// configPath validates the name and returns the profile's file path.
func (s *Store) configPath(name string) (string, error) {
	if err := ValidateName(name); err != nil {
		return "", err
	}
	return filepath.Join(s.root(), name, "config.toml"), nil
}

// ValidateName checks a profile id.
//
// The syntax rules come from sshconfig.ValidateName rather than a copy: a serial
// profile and an ssh profile are addressed by the same name in the same call
// sites, so the two must accept exactly the same strings. Only the reserved-name
// rule is added, and that one is serial-specific.
func ValidateName(name string) error {
	if err := sshconfig.ValidateName(name); err != nil {
		return fmt.Errorf("invalid serial config name %q: %w", strings.TrimSpace(name), err)
	}
	if strings.EqualFold(strings.TrimSpace(name), ReservedName) {
		return fmt.Errorf("serial config name %q is reserved", ReservedName)
	}
	return nil
}

// Load reads and validates a named profile.
func (s *Store) Load(name string) (*Entry, error) {
	name = strings.TrimSpace(name)
	p, err := s.configPath(name)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("serial config %q %w (use data-dir/serial_configs/%s/config.toml)",
				name, ErrNotFound, name)
		}
		return nil, err
	}
	entry, err := ParseAndValidate(data)
	if err != nil {
		return nil, fmt.Errorf("serial config %q: %w", name, err)
	}
	return entry, nil
}

// ReadRaw returns the profile's bytes, validating before handing them back so a
// caller editing the file starts from something loadable.
func (s *Store) ReadRaw(name string) ([]byte, error) {
	name = strings.TrimSpace(name)
	p, err := s.configPath(name)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("serial config %q %w", name, ErrNotFound)
		}
		return nil, err
	}
	if _, err := ParseAndValidate(data); err != nil {
		return nil, fmt.Errorf("serial config %q: %w", name, err)
	}
	return data, nil
}

// List returns the sorted names of every stored serial profile.
//
// An absent directory is an empty list, not an error: the feature is present
// whether or not anything has been configured yet.
func (s *Store) List() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := os.ReadDir(s.root())
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}

	names := []string{}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		// Only directories that actually hold a config are profiles; a stray
		// directory is not one.
		cfg := filepath.Join(s.root(), e.Name(), "config.toml")
		if st, err := os.Stat(cfg); err == nil && !st.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// Save validates and writes a profile, rejecting a name already used by another
// profile kind.
func (s *Store) Save(name string, data []byte) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	if _, err := ParseAndValidate(data); err != nil {
		return err
	}
	if err := s.checkName(name); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	p := filepath.Join(s.root(), name, "config.toml")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	if err := writeFileAtomic(p, data); err != nil {
		return err
	}
	s.notifyChange()
	return nil
}

// Rename moves a profile to a new validated name.
func (s *Store) Rename(oldName, newName string) error {
	oldName = strings.TrimSpace(oldName)
	newName = strings.TrimSpace(newName)
	if oldName == newName {
		return nil
	}
	oldPath, err := s.configPath(oldName)
	if err != nil {
		return err
	}
	newPath, err := s.configPath(newName)
	if err != nil {
		return err
	}
	if err := s.checkName(newName); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if st, err := os.Stat(oldPath); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("serial config %q %w", oldName, ErrNotFound)
		}
		return err
	} else if st.IsDir() {
		return fmt.Errorf("serial config %q is not a file", oldName)
	}

	// Collision check is case-insensitive: the same name differing only in case
	// is the same profile on Windows and macOS, and having two would make which
	// one loads depend on the filesystem.
	entries, err := os.ReadDir(s.root())
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() && e.Name() != oldName && strings.EqualFold(e.Name(), newName) {
			return fmt.Errorf("serial config already exists: %s", filepath.Join(s.root(), e.Name(), "config.toml"))
		}
	}

	if err := os.Rename(filepath.Dir(oldPath), filepath.Dir(newPath)); err != nil {
		return err
	}
	s.notifyChange()
	return nil
}

// Delete removes a profile. A missing profile is not an error: the caller asked
// for it to be gone, and it is.
func (s *Store) Delete(name string) error {
	p, err := s.configPath(name)
	if err != nil {
		return err
	}
	dir := filepath.Dir(p)
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	_ = os.Remove(dir)
	s.notifyChange()
	return nil
}

// writeFileAtomic writes through a temp file and renames it into place, so a
// crash cannot leave a half-written config.
//
// The mode is 0644, not the 0600 ssh profiles use: a serial profile holds no
// credentials, only a device path, so there is nothing to protect from other
// local users.
func writeFileAtomic(p string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-cfg-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, p); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}
