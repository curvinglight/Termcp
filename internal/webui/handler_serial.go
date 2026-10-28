package webui

import (
	"net/http"

	"github.com/open-mcp-ai/termcp/internal/serial/port"
)

// handleSerialPorts lists this machine's serial ports for the profile form's
// device picker. Read-only: listing what is plugged in changes nothing.
//
// The by-id path is the value a profile should store — ttyUSB numbers change
// with plug order and the by-id name does not — so the row carries both and
// the form prefers the stable one when it is offered.
func (h *Handler) handleSerialPorts(w http.ResponseWriter, r *http.Request) {
	infos, err := port.List()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	type portRow struct {
		Name       string `json:"name"`
		StableName string `json:"stable_name,omitempty"`
		IsUSB      bool   `json:"usb,omitempty"`
		VID        string `json:"vid,omitempty"`
		PID        string `json:"pid,omitempty"`
		Serial     string `json:"serial,omitempty"`
		Product    string `json:"product,omitempty"`
	}
	rows := make([]portRow, 0, len(infos))
	for _, in := range infos {
		rows = append(rows, portRow{
			Name:       in.Name,
			StableName: in.StableName,
			IsUSB:      in.IsUSB,
			VID:        in.VID,
			PID:        in.PID,
			Serial:     in.SerialNumber,
			Product:    in.Product,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ports": rows})
}
