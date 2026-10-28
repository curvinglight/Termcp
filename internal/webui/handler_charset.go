package webui

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
)

// handleSessionCharset switches the charset a session's bytes decode from —
// the terminal window's selector, made durable for the session's lifetime.
//
// It is a runtime property, not a profile edit: a restarted session returns to
// its profile's choice, the same way a review-mode switch does. "auto" and ""
// are legal values (re-arm the sniff; return to the profile), and anything
// else must be a name the encoding table maps — a selector that offered a
// typo'd name would silently turn decoding off.
func (h *Handler) handleSessionCharset(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if h.Sessions.Get(id) == nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	var body struct {
		Charset string `json:"charset"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if err := h.Sessions.SetCharsetOverride(id, body.Charset); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"charset": h.Sessions.CharsetFor(id)})
}
