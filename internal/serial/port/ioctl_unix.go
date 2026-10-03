//go:build !windows

package port

import (
	"errors"
	"syscall"
)

// isUnsupportedIoctl reports whether err is the platform's "this ioctl is not
// supported here" failure.
//
// SetDTR, SetRTS and Break reach the kernel through ioctls, and a device that
// has no modem lines (a pseudo-terminal, for instance) answers ENOTTY. The
// library passes the raw syscall error through rather than wrapping it in a
// PortError, so it has to be recognized here.
func isUnsupportedIoctl(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	return errno == syscall.ENOTTY
}
