package serial

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"
)

func writeTestConfig(device string) []byte {
	return []byte("kind = \"serial\"\ndevice = \"" + device + "\"\nbaud = 115200\n")
}

func TestStoreSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	data := writeTestConfig(deviceFor())
	if err := s.Save("board", data); err != nil {
		t.Fatalf("Save: %v", err)
	}

	e, err := s.Load("board")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if e.Baud != 115200 {
		t.Errorf("Baud = %d, want 115200", e.Baud)
	}
	if e.Kind != Kind {
		t.Errorf("Kind = %q, want %q", e.Kind, Kind)
	}
}

func TestStoreLoadMissing(t *testing.T) {
	s := NewStore(t.TempDir())
	_, err := s.Load("nope")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Load of a missing profile = %v, want ErrNotFound", err)
	}
}

func TestStoreListEmptyWhenDirectoryAbsent(t *testing.T) {
	// The feature exists whether or not anything has been configured; listing
	// before the first save must not be an error.
	s := NewStore(t.TempDir())
	names, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(names) != 0 {
		t.Fatalf("List = %v, want empty", names)
	}
}

func TestStoreListSorted(t *testing.T) {
	s := NewStore(t.TempDir())
	for _, name := range []string{"zulu", "alpha", "mike"} {
		if err := s.Save(name, writeTestConfig(deviceFor())); err != nil {
			t.Fatalf("Save %s: %v", name, err)
		}
	}

	names, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if !sort.StringsAreSorted(names) {
		t.Fatalf("List = %v, want sorted", names)
	}
	if len(names) != 3 {
		t.Fatalf("List = %v, want 3 names", names)
	}
}

func TestStoreListIgnoresDirectoryWithoutConfig(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	if err := s.Save("real", writeTestConfig(deviceFor())); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// A stray directory is not a profile.
	if err := os.MkdirAll(filepath.Join(dir, "serial_configs", "stray"), 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}

	names, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(names) != 1 || names[0] != "real" {
		t.Fatalf("List = %v, want [real]", names)
	}
}

func TestStoreSaveValidatesBeforeWriting(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	// An invalid profile must not leave a file behind.
	err := s.Save("bad", []byte("kind = \"serial\"\nbaud = 115200\n")) // no device
	if err == nil {
		t.Fatal("saving an invalid profile should fail")
	}
	if _, statErr := os.Stat(filepath.Join(dir, "serial_configs", "bad", "config.toml")); !os.IsNotExist(statErr) {
		t.Fatal("a rejected profile must not be written to disk")
	}
}

func TestStoreSaveRejectsReservedName(t *testing.T) {
	s := NewStore(t.TempDir())
	err := s.Save(ReservedName, writeTestConfig(deviceFor()))
	if err == nil {
		t.Fatal("the reserved name should be refused")
	}
}

func TestStoreSaveRejectsInvalidName(t *testing.T) {
	s := NewStore(t.TempDir())
	for _, name := range []string{"", " ", "../escape", "has space", "a/b", "."} {
		if err := s.Save(name, writeTestConfig(deviceFor())); err == nil {
			t.Errorf("name %q should be rejected", name)
		}
	}
}

func TestStoreNameCheckerBlocksConflicts(t *testing.T) {
	// The cross-directory check belongs to the caller (which knows about the
	// other store); the serial store only has to honour it.
	s := NewStore(t.TempDir())
	s.SetNameChecker(func(name string) error {
		if name == "taken" {
			return errors.New("already an ssh profile")
		}
		return nil
	})

	if err := s.Save("free", writeTestConfig(deviceFor())); err != nil {
		t.Fatalf("an allowed name should save, got %v", err)
	}
	if err := s.Save("taken", writeTestConfig(deviceFor())); err == nil {
		t.Fatal("a name rejected by the checker should not save")
	}
}

func TestStoreNameCheckerAppliesToRename(t *testing.T) {
	s := NewStore(t.TempDir())
	if err := s.Save("board", writeTestConfig(deviceFor())); err != nil {
		t.Fatalf("Save: %v", err)
	}
	s.SetNameChecker(func(name string) error {
		if name == "taken" {
			return errors.New("already an ssh profile")
		}
		return nil
	})

	if err := s.Rename("board", "taken"); err == nil {
		t.Fatal("a rename into a conflicting name should fail")
	}
	// The original must still be there after the refusal.
	if _, err := s.Load("board"); err != nil {
		t.Fatalf("the original profile should survive a refused rename: %v", err)
	}
}

func TestStoreRename(t *testing.T) {
	s := NewStore(t.TempDir())
	if err := s.Save("old", writeTestConfig(deviceFor())); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := s.Rename("old", "new"); err != nil {
		t.Fatalf("Rename: %v", err)
	}

	if _, err := s.Load("new"); err != nil {
		t.Fatalf("the renamed profile should load: %v", err)
	}
	if _, err := s.Load("old"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the old name should be gone, got %v", err)
	}
}

func TestStoreRenameMissing(t *testing.T) {
	s := NewStore(t.TempDir())
	if err := s.Rename("ghost", "other"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("renaming a missing profile = %v, want ErrNotFound", err)
	}
}

func TestStoreRenameCaseInsensitiveCollision(t *testing.T) {
	// On Linux the two names are distinct directories, so it is the case-folding
	// check rather than the filesystem that has to refuse this. The check must
	// still fire, because a profile written here may be read on Windows or macOS
	// where the two names are one.
	s := NewStore(t.TempDir())
	if err := s.Save("Board", writeTestConfig(deviceFor())); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := s.Save("other", writeTestConfig(deviceFor())); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if err := s.Rename("other", "BOARD"); err == nil {
		t.Fatal("renaming to a name differing only in case should be refused")
	}
}

func TestStoreDelete(t *testing.T) {
	s := NewStore(t.TempDir())
	if err := s.Save("board", writeTestConfig(deviceFor())); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := s.Delete("board"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Load("board"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the profile should be gone after Delete, got %v", err)
	}
}

func TestStoreDeleteMissingIsNotAnError(t *testing.T) {
	// The caller asked for it to be gone; it is.
	s := NewStore(t.TempDir())
	if err := s.Delete("ghost"); err != nil {
		t.Fatalf("deleting a missing profile should not fail, got %v", err)
	}
}

func TestStoreOnChangeFires(t *testing.T) {
	s := NewStore(t.TempDir())
	var calls int
	s.SetOnChange(func() { calls++ })

	if err := s.Save("a", writeTestConfig(deviceFor())); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if calls == 0 {
		t.Error("Save should notify the change hook")
	}

	before := calls
	if err := s.Delete("a"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if calls == before {
		t.Error("Delete should notify the change hook")
	}
}

func TestStoreConfigFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no POSIX permission bits")
	}
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Save("board", writeTestConfig(deviceFor())); err != nil {
		t.Fatalf("Save: %v", err)
	}

	p := filepath.Join(dir, "serial_configs", "board", "config.toml")
	st, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// 0644, not the 0600 ssh profiles use: a serial profile holds no
	// credentials. Readable by others is the intended mode.
	if got := st.Mode().Perm(); got != 0o644 {
		t.Errorf("config mode = %04o, want 0644", got)
	}
}

func TestStoreReadRawRejectsInvalid(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	// Write a broken file directly, bypassing Save's validation.
	p := filepath.Join(dir, "serial_configs", "broken", "config.toml")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := os.WriteFile(p, []byte("kind = \"serial\"\n"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	if _, err := s.ReadRaw("broken"); err == nil {
		t.Fatal("ReadRaw should refuse to hand back an invalid profile")
	}
}

func TestLoadReportsParseErrorNotNotFound(t *testing.T) {
	// A profile that exists but does not parse is a different problem from one
	// that does not exist; a caller cleaning up broken profiles needs to tell
	// them apart.
	dir := t.TempDir()
	s := NewStore(dir)

	p := filepath.Join(dir, "serial_configs", "broken", "config.toml")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := os.WriteFile(p, []byte("kind = \"serial\"\n"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	_, err := s.Load("broken")
	if err == nil {
		t.Fatal("expected an error")
	}
	if errors.Is(err, ErrNotFound) {
		t.Fatalf("a parse failure must not be reported as not-found: %v", err)
	}
}

func TestValidateNameMatchesSSHRules(t *testing.T) {
	// Serial and ssh profile names are used interchangeably by callers, so the
	// two must accept exactly the same strings.
	// These mirror sshconfig's rule: a leading digit is allowed, a leading
	// dash or underscore is not. The two must agree exactly.
	valid := []string{"board", "board-1", "Board_2", "a", "A1", "1board"}
	for _, name := range valid {
		if err := ValidateName(name); err != nil {
			t.Errorf("name %q should be valid, got %v", name, err)
		}
	}

	invalid := []string{"", " ", "-board", "_board", "has space", "a/b", "..\x00"}
	for _, name := range invalid {
		if err := ValidateName(name); err == nil {
			t.Errorf("name %q should be invalid", name)
		}
	}
}

func TestValidateNameReservesInternal(t *testing.T) {
	for _, name := range []string{"internal", "Internal", "INTERNAL"} {
		if err := ValidateName(name); err == nil {
			t.Errorf("name %q should be reserved", name)
		}
	}
}
