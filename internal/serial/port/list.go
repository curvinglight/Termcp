package port

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// List enumerates this machine's serial ports.
//
// Detail depends on the platform: USB descriptor fields are filled where the
// OS can report them and left empty otherwise (macOS without cgo — see
// enumerate_darwin_nocgo.go). Callers must treat every field except Name as
// optional.
func List() ([]Info, error) {
	infos, err := enumerateDetailed()
	if err != nil {
		return nil, normalizeEnumerateError(err)
	}
	// A nil slice would serialize as null; an empty list is the truthful answer.
	if infos == nil {
		infos = []Info{}
	}
	return infos, nil
}

// DeviceExists reports whether a device path currently resolves to something.
//
// It is deliberately not os.Stat at the call site: the check differs by
// platform, and on Windows a COM port is not a filesystem path that Stat can
// answer for. Callers use this to notice a device that disappeared under an
// open handle.
//
// A false result with a nil error means the device is gone; an error means the
// check itself failed and the device's state is unknown.
func DeviceExists(device string) (bool, error) {
	d := strings.TrimSpace(device)
	if d == "" {
		return false, nil
	}

	if runtime.GOOS == "windows" {
		return windowsDeviceExists(d)
	}

	// Unix: a device node is a filesystem entry. Stat follows the symlink, so a
	// by-id alias reports the state of the tty it points at — which is what
	// "is the device still there" means.
	if _, err := os.Stat(d); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// stableNameFor returns a path that survives renumbering, or "" when the
// platform offers none.
//
// On Linux, udev publishes /dev/serial/by-id/<id> as a symlink whose target is
// the kernel-assigned /dev/ttyUSB<n>. The kernel number depends on plug order,
// so a profile bound to it can silently attach to the wrong adapter, while the
// by-id name follows the device. The reverse mapping (ttyUSB0 -> its by-id name)
// is built by scanning the alias directory for a link resolving to this device.
func stableNameFor(name string) string {
	if runtime.GOOS != "linux" {
		// macOS cu.* names are already stable per device; Windows COM numbers
		// are not, and have no alias to offer.
		return ""
	}

	target := filepath.Clean(name)
	for _, dir := range []string{"/dev/serial/by-id", "/dev/serial/by-path"} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			alias := filepath.Join(dir, entry.Name())
			resolved, err := filepath.EvalSymlinks(alias)
			if err != nil {
				continue
			}
			if filepath.Clean(resolved) == target {
				return alias
			}
		}
	}
	return ""
}

// normalizeEnumerateError keeps enumeration failures from surfacing raw library
// text. A failure to list ports is never one of the open errors (nothing is
// being opened), so it is reported as unsupported rather than misclassified as
// busy or missing.
func normalizeEnumerateError(err error) error {
	if err == nil {
		return nil
	}
	return normalizeOpenError(err)
}
