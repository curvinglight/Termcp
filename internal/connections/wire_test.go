package connections

import (
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/serial"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
)

func TestNameRulesAgreeBetweenKinds(t *testing.T) {
	// A serial profile and an ssh profile are addressed by the same name in the
	// same call sites, so the two stores must accept exactly the same strings.
	//
	// serial cannot import sshconfig to share the rule (session imports serial,
	// and sshconfig imports session), so the two regexes are kept identical by
	// this test. If one is edited without the other, this fails.
	candidates := []string{
		"board", "board-1", "Board_2", "a", "A1", "1board",
		"", " ", "-board", "_board", "has space", "a/b",
		"internal", "Internal", "INTERNAL",
		strings.Repeat("x", 64), strings.Repeat("x", 65),
	}

	for _, name := range candidates {
		sshErr := sshconfig.ValidateName(name)
		serialErr := serial.ValidateName(name)

		// "internal" is the one intended difference: the ssh store allows the
		// reserved name (it IS that profile), while a serial profile may not
		// take it.
		if strings.EqualFold(strings.TrimSpace(name), serial.ReservedName) {
			if serialErr == nil {
				t.Errorf("name %q: serial must refuse the reserved name", name)
			}
			continue
		}

		if (sshErr == nil) != (serialErr == nil) {
			t.Errorf("name %q: ssh says %v, serial says %v; the two rules must agree",
				name, sshErr, serialErr)
		}
	}
}

func TestWirePreventsSSHSavingOverSerialName(t *testing.T) {
	// The serial profile is saved first; the ssh save must then be refused, or
	// the instance would hold an ambiguous pair it created itself.
	r, _ := newResolver(t)
	Wire(r.SSH, r.Serial)

	if err := r.Serial.Save("shared", serialConfig()); err != nil {
		t.Fatalf("initial serial save: %v", err)
	}

	err := r.SSH.Save("shared", sshConfig())
	if err == nil {
		t.Fatal("saving an ssh profile over a serial name should be refused")
	}
	if !strings.Contains(err.Error(), "serial profile") {
		t.Fatalf("error should say the name is a serial profile, got %v", err)
	}
}

func TestWirePreventsSerialSavingOverSSHName(t *testing.T) {
	r, _ := newResolver(t)
	Wire(r.SSH, r.Serial)

	if err := r.SSH.Save("shared", sshConfig()); err != nil {
		t.Fatalf("initial ssh save: %v", err)
	}

	err := r.Serial.Save("shared", serialConfig())
	if err == nil {
		t.Fatal("saving a serial profile over an ssh name should be refused")
	}
	if !strings.Contains(err.Error(), "ssh profile") {
		t.Fatalf("error should say the name is an ssh profile, got %v", err)
	}
}

func TestWirePreventsRenameIntoOtherKind(t *testing.T) {
	r, _ := newResolver(t)
	Wire(r.SSH, r.Serial)

	if err := r.Serial.Save("board", serialConfig()); err != nil {
		t.Fatalf("serial save: %v", err)
	}
	if err := r.SSH.Save("other", sshConfig()); err != nil {
		t.Fatalf("ssh save: %v", err)
	}

	if err := r.SSH.Rename("other", "board"); err == nil {
		t.Fatal("renaming an ssh profile onto a serial name should be refused")
	}
	// The refused rename must leave both profiles intact.
	if _, err := r.SSH.Load("other"); err != nil {
		t.Fatalf("the ssh profile should survive a refused rename: %v", err)
	}
	if _, err := r.Serial.Load("board"); err != nil {
		t.Fatalf("the serial profile should be untouched: %v", err)
	}
}

func TestWireAllowsDistinctNames(t *testing.T) {
	r, _ := newResolver(t)
	Wire(r.SSH, r.Serial)

	if err := r.Serial.Save("board", serialConfig()); err != nil {
		t.Fatalf("serial save: %v", err)
	}
	if err := r.SSH.Save("box", sshConfig()); err != nil {
		t.Fatalf("a distinct ssh name should save, got %v", err)
	}
}

func TestWireLeavesReservedNameToSerialValidation(t *testing.T) {
	// "internal" is a virtual ssh profile that Load always answers for. The
	// serial store must refuse the name for its own reason (reserved), not
	// report it as a collision with the built-in entry.
	r, _ := newResolver(t)
	Wire(r.SSH, r.Serial)

	err := r.Serial.Save(serial.ReservedName, serialConfig())
	if err == nil {
		t.Fatal("the reserved name should be refused")
	}
	if strings.Contains(err.Error(), "already an ssh profile") {
		t.Fatalf("the reserved name should be refused as reserved, not as a collision: %v", err)
	}
}

func TestWireWithOnlySSHStore(t *testing.T) {
	// An instance without serial support wires only the ssh store; there is
	// nothing for a name to collide with, and saving must still work.
	dir := t.TempDir()
	ssh := sshconfig.NewStore(dir)
	Wire(ssh, nil)

	if err := ssh.Save("box", sshConfig()); err != nil {
		t.Fatalf("save with no serial store: %v", err)
	}
}

func TestWireIsIdempotent(t *testing.T) {
	// Re-wiring must not stack checks or leave a stale one behind.
	r, _ := newResolver(t)
	Wire(r.SSH, r.Serial)
	Wire(r.SSH, r.Serial)

	if err := r.Serial.Save("board", serialConfig()); err != nil {
		t.Fatalf("serial save after re-wire: %v", err)
	}
	if err := r.SSH.Save("board", sshConfig()); err == nil {
		t.Fatal("the conflict check should still fire after a second Wire")
	}

	// Dropping the serial store must also drop the check, so the ssh store is
	// usable on its own again.
	Wire(r.SSH, nil)
	if err := r.SSH.Save("board", sshConfig()); err != nil {
		t.Fatalf("after unwiring, the ssh name should be free: %v", err)
	}
}
