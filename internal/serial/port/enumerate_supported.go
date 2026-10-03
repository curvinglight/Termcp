//go:build !darwin

package port

import (
	"go.bug.st/serial/enumerator"
)

// enumerateDetailed lists ports with USB descriptors where the platform can
// provide them.
//
// The enumerator package is pure Go on Linux, Windows and BSD, so importing it
// unconditionally on those platforms costs nothing and keeps VID/PID available
// for telling adapters apart.
//
// The darwin split is not a preference: the enumerator's only macOS
// implementation is cgo, and under CGO_ENABLED=0 it does not compile at all
// (no file provides nativeGetDetailedPortsList). See enumerate_darwin_nocgo.go.
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
