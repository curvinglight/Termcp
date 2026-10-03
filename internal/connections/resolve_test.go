package connections

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/serial"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
)

func deviceFor() string {
	if runtime.GOOS == "windows" {
		return "COM3"
	}
	return "/dev/ttyUSB0"
}

func serialConfig() []byte {
	return []byte("kind = \"serial\"\ndevice = \"" + deviceFor() + "\"\nbaud = 115200\n")
}

func sshConfig() []byte {
	// A remote entry needs a credential of some kind; these tests are about
	// resolution, not authentication, so the value is arbitrary.
	return []byte("kind = \"remote\"\nhost = \"example.com\"\nuser = \"root\"\npassword = \"secret\"\n")
}

// newResolver builds both stores under one data dir, the layout the real
// program uses.
func newResolver(t *testing.T) (*Resolver, string) {
	t.Helper()
	dir := t.TempDir()
	return &Resolver{
		SSH:    sshconfig.NewStore(dir),
		Serial: serial.NewStore(dir),
	}, dir
}

func TestResolveSSHProfile(t *testing.T) {
	r, _ := newResolver(t)
	if err := r.SSH.Save("box", sshConfig()); err != nil {
		t.Fatalf("save ssh profile: %v", err)
	}

	got, err := r.Resolve("box")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Kind != KindSSH {
		t.Fatalf("Kind = %q, want %q", got.Kind, KindSSH)
	}
	if got.SSH == nil {
		t.Fatal("SSH entry should be populated")
	}
	if got.Serial != nil {
		t.Fatal("Serial entry should be nil for an ssh profile")
	}
}

func TestResolveSerialProfile(t *testing.T) {
	r, _ := newResolver(t)
	if err := r.Serial.Save("board", serialConfig()); err != nil {
		t.Fatalf("save serial profile: %v", err)
	}

	got, err := r.Resolve("board")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Kind != KindSerial {
		t.Fatalf("Kind = %q, want %q", got.Kind, KindSerial)
	}
	if got.Serial == nil {
		t.Fatal("Serial entry should be populated")
	}
	if got.SSH != nil {
		t.Fatal("SSH entry should be nil for a serial profile")
	}
}

func TestResolveAmbiguous(t *testing.T) {
	// A name in both directories must be an error, not a preference: whichever
	// one won, the other profile would be silently unreachable.
	r, _ := newResolver(t)
	if err := r.SSH.Save("shared", sshConfig()); err != nil {
		t.Fatalf("save ssh profile: %v", err)
	}
	if err := r.Serial.Save("shared", serialConfig()); err != nil {
		t.Fatalf("save serial profile: %v", err)
	}

	_, err := r.Resolve("shared")
	if !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("Resolve = %v, want ErrAmbiguous", err)
	}
	// Both locations belong in the message: the fix is to delete one, and that
	// needs to know where they are.
	if !strings.Contains(err.Error(), "ssh_configs") || !strings.Contains(err.Error(), "serial_configs") {
		t.Errorf("ambiguity error should name both paths, got %v", err)
	}
}

func TestResolveNotFound(t *testing.T) {
	r, _ := newResolver(t)
	_, err := r.Resolve("ghost")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Resolve = %v, want ErrNotFound", err)
	}
}

func TestResolveEmptyName(t *testing.T) {
	r, _ := newResolver(t)
	if _, err := r.Resolve("   "); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Resolve(blank) = %v, want ErrNotFound", err)
	}
}

func TestResolveSerialDisabled(t *testing.T) {
	// With --no-serial the serial store is nil. A serial name then resolves to
	// not-found rather than erroring on the missing store.
	dir := t.TempDir()
	r := &Resolver{SSH: sshconfig.NewStore(dir)}

	if _, err := r.Resolve("board"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Resolve = %v, want ErrNotFound when serial is disabled", err)
	}
}

func TestResolveSSHDisabled(t *testing.T) {
	dir := t.TempDir()
	r := &Resolver{Serial: serial.NewStore(dir)}

	if _, err := r.Resolve("board"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Resolve = %v, want ErrNotFound when ssh is disabled", err)
	}
}

func TestResolveNilStores(t *testing.T) {
	r := &Resolver{}
	if _, err := r.Resolve("anything"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Resolve = %v, want ErrNotFound with no stores", err)
	}
}

func TestResolveInternalProfile(t *testing.T) {
	// "internal" is the built-in ssh loopback entry, served by the ssh store
	// without a file on disk.
	r, _ := newResolver(t)
	got, err := r.Resolve("internal")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Kind != KindSSH {
		t.Fatalf("Kind = %q, want %q", got.Kind, KindSSH)
	}
}

func TestResolveBrokenProfileIsNotNotFound(t *testing.T) {
	// A profile that exists but does not parse is a different problem from one
	// that does not exist, and the caller has to be able to tell them apart.
	r, dir := newResolver(t)

	p := filepath.Join(dir, "serial_configs", "broken", "config.toml")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := os.WriteFile(p, []byte("kind = \"serial\"\n"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	_, err := r.Resolve("broken")
	if err == nil {
		t.Fatal("expected an error for a broken profile")
	}
	if errors.Is(err, ErrNotFound) {
		t.Fatalf("a parse failure must not be reported as not-found: %v", err)
	}
}

func TestResolveIsCaseSensitiveOnNames(t *testing.T) {
	// Names are stored as written; only the collision check folds case. A
	// differently-cased lookup is a different name.
	r, _ := newResolver(t)
	if err := r.Serial.Save("Board", serialConfig()); err != nil {
		t.Fatalf("save: %v", err)
	}

	if _, err := r.Resolve("Board"); err != nil {
		t.Fatalf("the exact name should resolve: %v", err)
	}
	if _, err := r.Resolve("board"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a differently-cased name = %v, want ErrNotFound", err)
	}
}
