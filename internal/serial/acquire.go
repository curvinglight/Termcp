package serial

import (
	"fmt"
	"sync"

	"github.com/open-mcp-ai/termcp/internal/serial/port"
)

// acquireGate serializes device claims within one process.
//
// Two profiles can name the same physical device through different paths
// (/dev/serial/by-id/... and /dev/ttyUSB0 are one port). Checking "is it free"
// and then opening is a race when both checks happen before either open: both
// would see it free. The gate makes the check and the claim one step, and the
// operating system's own exclusivity stays as a second line of defence rather
// than the only one.
//
// It is per-process by nature. A device held by another program is not visible
// here; that case still surfaces as a busy error from the open itself.
type acquireGate struct {
	mu      sync.Mutex
	claimed map[string]string // canonical device -> profile name that holds it
}

var deviceGate = &acquireGate{claimed: map[string]string{}}

// Acquire claims a device for a profile name and returns a release function.
//
// The release function is idempotent: it is handed to the session's resource
// scope, which may run it more than once on overlapping teardown paths.
func Acquire(device, profileName string) (release func(), err error) {
	canonical := port.CanonicalDevice(device)
	if canonical == "" {
		return nil, fmt.Errorf("%w: device is required", port.ErrInvalidConfig)
	}

	deviceGate.mu.Lock()
	if holder, taken := deviceGate.claimed[canonical]; taken {
		deviceGate.mu.Unlock()
		return nil, fmt.Errorf("%w: %s is already in use by profile %q",
			port.ErrBusy, device, holder)
	}
	deviceGate.claimed[canonical] = profileName
	deviceGate.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			deviceGate.mu.Lock()
			// Only clear the entry if this holder still owns it. A release that
			// arrives after the device was re-claimed (close then reopen) must
			// not free the new owner's claim.
			if deviceGate.claimed[canonical] == profileName {
				delete(deviceGate.claimed, canonical)
			}
			deviceGate.mu.Unlock()
		})
	}, nil
}

// ReleaseAllForTest clears every claim. It exists so tests do not depend on
// execution order, and is not used outside them.
func ReleaseAllForTest() {
	deviceGate.mu.Lock()
	deviceGate.claimed = map[string]string{}
	deviceGate.mu.Unlock()
}
