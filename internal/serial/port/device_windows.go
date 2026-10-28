//go:build windows

package port

import (
	"strings"

	"golang.org/x/sys/windows"
)

// windowsDeviceExists probes a COM port by opening it and closing it again.
//
// os.Stat cannot answer this: a COM port is not a filesystem path, and
// \\.\COM3 is a device-namespace name the file APIs do not stat. Opening is the
// only honest probe, so the port is opened and immediately released.
//
// A busy port reports success rather than failure: something else holding the
// device means the device is present, which is the question being asked. Only
// "the device vanished" is reported as not existing.
func windowsDeviceExists(d string) (bool, error) {
	name := normalizeWindowsPortName(d)

	path, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return false, err
	}

	handle, err := windows.CreateFile(
		path,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		0, // no sharing: the same exclusivity a real open would request
		nil,
		windows.OPEN_EXISTING,
		0,
		0,
	)
	if err != nil {
		// Distinguish "present but unavailable" from "gone". A sharing
		// violation or access denial says the device is there.
		if isDevicePresentError(err) {
			return true, nil
		}
		return false, nil
	}
	_ = windows.CloseHandle(handle)
	return true, nil
}

// normalizeWindowsPortName adds the \\.\ prefix the Win32 device namespace
// needs for port numbers above 9. COM1..COM9 answer to the bare name, COM10+
// do not, and the prefix works for both.
func normalizeWindowsPortName(d string) string {
	name := strings.TrimSpace(d)
	if strings.HasPrefix(name, `\\.\`) {
		return name
	}
	return `\\.\` + name
}

// isDevicePresentError reports whether a failed open means the device exists but
// could not be claimed, as opposed to not existing at all.
func isDevicePresentError(err error) bool {
	return err == windows.ERROR_ACCESS_DENIED ||
		err == windows.ERROR_SHARING_VIOLATION ||
		err == windows.ERROR_BUSY
}
