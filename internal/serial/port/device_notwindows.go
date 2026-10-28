//go:build !windows

package port

// windowsDeviceExists has no meaning off Windows; DeviceExists selects the Unix
// os.Stat path by GOOS and never calls this. It exists so the Windows-only file
// can stay free of a build tag on the caller side.
func windowsDeviceExists(string) (bool, error) { return false, nil }
