package connections

import (
	"fmt"

	"github.com/open-mcp-ai/termcp/internal/serial"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
)

// Wire installs the cross-directory name checks on both stores.
//
// Without this, each store only knows its own directory, so the same name can
// be saved as both an ssh and a serial profile. That pair would then be
// ambiguous to Resolve, and the ambiguity would have been created by the
// program rather than by a person editing files by hand.
//
// The checks are mutual: an ssh save consults the serial directory and vice
// versa. Either store may be nil (an instance without serial support, or one
// without ssh), in which case only the remaining store is checked.
func Wire(ssh *sshconfig.Store, serialStore *serial.Store) {
	if ssh != nil && serialStore != nil {
		ssh.SetNameChecker(func(name string) error {
			return rejectIfSerialExists(serialStore, name)
		})
		serialStore.SetNameChecker(func(name string) error {
			return rejectIfSSHExists(ssh, name)
		})
		return
	}

	// Only one store is present, so there is nothing it could collide with;
	// clearing the hook keeps a re-Wire from leaving a stale checker behind.
	if ssh != nil {
		ssh.SetNameChecker(nil)
	}
	if serialStore != nil {
		serialStore.SetNameChecker(nil)
	}
}

// rejectIfSerialExists reports an error when the name is already a serial profile.
func rejectIfSerialExists(store *serial.Store, name string) error {
	if _, err := store.Load(name); err == nil {
		return fmt.Errorf("name %q is already a serial profile (%s); "+
			"a name used by both kinds is ambiguous", name, store.ConfigDir(name)+"/config.toml")
	}
	return nil
}

// rejectIfSSHExists reports an error when the name is already an ssh profile.
func rejectIfSSHExists(store *sshconfig.Store, name string) error {
	// "internal" needs no check here: it is a virtual ssh profile that Load
	// always answers for, and the serial side already refuses the name outright
	// (see serial.ValidateName). Testing it here would report the reserved name
	// as a collision, which is the wrong reason.
	if _, err := store.Load(name); err == nil {
		return fmt.Errorf("name %q is already an ssh profile (%s); "+
			"a name used by both kinds is ambiguous", name, store.ConfigDir(name)+"/config.toml")
	}
	return nil
}
