//go:build darwin && !cgo

package port

import (
	"go.bug.st/serial"
)

// enumerateDetailed lists ports by name only.
//
// This build has no enumerator package: go.bug.st/serial's only macOS
// implementation of it (usb_darwin.go) uses cgo to reach IOKit, and the library
// ships no pure-Go fallback — referencing the package under CGO_ENABLED=0 on
// darwin fails with "undefined: nativeGetDetailedPortsList". Because that
// failure is in the dependency, it cannot be worked around; the import itself
// has to be excluded, which is what this build tag does.
//
// The cost is USB detail: no VID, PID, serial number or product string, so a
// by-id path is unavailable too. Ports are still listed and still usable by
// name. This is the trade for keeping Termcp's CGO_ENABLED=0 policy on macOS.
func enumerateDetailed() ([]Info, error) {
	names, err := serial.GetPortsList()
	if err != nil {
		return nil, err
	}

	infos := make([]Info, 0, len(names))
	for _, name := range names {
		infos = append(infos, Info{
			Name:       name,
			StableName: stableNameFor(name),
			// IsUSB stays false: without the enumerator there is no way to tell
			// a USB adapter from an on-board UART, and guessing would be worse
			// than saying nothing.
		})
	}
	return infos, nil
}
