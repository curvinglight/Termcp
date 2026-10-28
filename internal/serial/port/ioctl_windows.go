//go:build windows

package port

import (
	"errors"

	"golang.org/x/sys/windows"
)

// isUnsupportedIoctl reports whether err is Windows' "this operation is not
// supported on this device" failure.
//
// The Windows serial implementation reaches the driver through DeviceIoControl,
// and a device without modem control answers ERROR_INVALID_FUNCTION or
// ERROR_NOT_SUPPORTED rather than a wrapped PortError.
func isUnsupportedIoctl(err error) bool {
	return errors.Is(err, windows.ERROR_INVALID_FUNCTION) ||
		errors.Is(err, windows.ERROR_NOT_SUPPORTED)
}
