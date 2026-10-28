//go:build darwin && cgo

package port

import (
	"go.bug.st/serial/enumerator"
)

// enumerateDetailed lists ports with USB descriptors. On macOS this requires
// cgo: the enumerator's macOS implementation links IOKit and CoreFoundation.
// Without cgo the package does not compile, which is why this file and
// enumerate_darwin_nocgo.go exist as a pair — see the latter for the fallback.
func enumerateDetailed() ([]Info, error) {
	details, err := enumerator.GetDetailedPortsList()
	if err != nil {
		return nil, err
	}

	infos := make([]Info, 0, len(details))
	for _, d := range details {
		infos = append(infos, Info{
			Name:         d.Name,
			StableName:   stableNameFor(d.Name),
			IsUSB:        d.IsUSB,
			VID:          d.VID,
			PID:          d.PID,
			SerialNumber: d.SerialNumber,
			Manufacturer: d.Manufacturer,
			Product:      d.Product,
		})
	}
	return infos, nil
}
