package webui

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/open-mcp-ai/termcp/internal/connections"
	"github.com/open-mcp-ai/termcp/internal/locator"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// resolveResponse is the body of GET /api/resolve. Only the fields meaningful
// for the resolved kind are set, so a client can switch on "kind" alone.
type resolveResponse struct {
	Kind      string `json:"kind"` // "entry" | "session" | "shell"
	Entry     string `json:"entry,omitempty"`
	SSHConfig string `json:"ssh_config,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	ShellID   string `json:"shell_id,omitempty"`
	Index     int    `json:"index,omitempty"`
	Name      string `json:"name,omitempty"`
	Status    string `json:"status,omitempty"`
}

// handleResolve turns a termcp:// locator into concrete ids so scripts and
// agents can use it with the plain REST endpoints (the MCP tools accept
// locators directly; this is the HTTP equivalent).
//
//	GET /api/resolve?url=termcp://rock64        → connection profile
//	GET /api/resolve?url=termcp://#<sid>        → session
//	GET /api/resolve?url=termcp://#<sid>:2      → shell channel 2 (1-based)
func (h *Handler) handleResolve(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("url")
	p, err := locator.Parse(raw)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	switch p.Kind {
	case locator.KindEntry:
		h.resolveEntry(w, p)
	case locator.KindSession, locator.KindShell:
		h.resolveSessionOrShell(w, p)
	default:
		http.Error(w, fmt.Sprintf("unsupported locator kind for %q", raw), http.StatusBadRequest)
	}
}

// resolveEntry maps termcp://<entry> to the profile name used by
// POST /api/sessions. Unknown profiles are a 404 with the actionable reason.
//
// The lookup goes through the shared connections.Resolver, the same
// implementation session_start and the Web UI's own session creation use, so a
// termcp:// locator names a serial profile exactly as it does an ssh one
// (design §3.6) instead of reporting it missing.
func (h *Handler) resolveEntry(w http.ResponseWriter, p *locator.Parsed) {
	if h.SSH == nil && h.Serial == nil {
		http.Error(w, "ssh config store not configured", http.StatusServiceUnavailable)
		return
	}
	if p.Entry == "internal" && h.NoInternal {
		http.Error(w, `connection profile "internal" is disabled on this instance`, http.StatusNotFound)
		return
	}
	resolved, err := h.resolver().Resolve(p.Entry)
	if err != nil {
		switch {
		case errors.Is(err, connections.ErrAmbiguous):
			// Present in both directories: neither profile is reachable by this
			// name, and picking one would hide the other.
			http.Error(w, err.Error(), http.StatusConflict)
		case errors.Is(err, connections.ErrNotFound):
			http.Error(w, fmt.Sprintf("connection profile %q not found (list profiles with GET /api/connections)", p.Entry), http.StatusNotFound)
		default:
			// The profile exists but does not parse: say what is wrong with it
			// rather than claiming there is nothing there.
			http.Error(w, err.Error(), http.StatusBadRequest)
		}
		return
	}
	if resolved.Kind == connections.KindSSH && resolved.SSH.Kind == sshconfig.KindInternal && h.NoInternal {
		http.Error(w, `connection profile "internal" is disabled on this instance`, http.StatusNotFound)
		return
	}
	// SSHConfig carries the profile name for both kinds: it is the field
	// POST /api/sessions reads, and that endpoint resolves it by name through
	// the same resolver, so a serial name lands back here correctly.
	writeJSON(w, http.StatusOK, resolveResponse{
		Kind:      "entry",
		Entry:     p.Entry,
		SSHConfig: p.Entry,
	})
}

// resolveSessionOrShell resolves the session part first (live sessions, then
// closed/restored DEAD ones kept in the registry), then the optional shell
// channel index. A shell locator on a closed session is rejected (409): its
// process is gone, so the channel is read-only via output-range.
func (h *Handler) resolveSessionOrShell(w http.ResponseWriter, p *locator.Parsed) {
	if sess := h.Sessions.Get(p.SessionID); sess != nil {
		info := sess.Info()
		resp := resolveResponse{
			Kind:      "session",
			SessionID: sess.ID,
			Name:      info.Name,
			Status:    string(info.Status),
		}
		if p.Kind == locator.KindSession {
			writeJSON(w, http.StatusOK, resp)
			return
		}
		// Shell channel: only live sessions expose an operable channel.
		if info.Status != api.SessionRunning {
			http.Error(w, fmt.Sprintf("session %q is closed (status %s); shell channels are read-only — use GET /api/shells/{id}/output-range or shell_output", p.SessionID, info.Status), http.StatusConflict)
			return
		}
		// Shell channel: 1-based creation order, matching the Web UI tabs
		// (shell-1, shell-2, …). No index = the primary (first) shell.
		idx := p.Index
		if idx == 0 {
			idx = 1
		}
		cs, ok := sess.ShellByIndex(idx)
		if !ok {
			http.Error(w, fmt.Sprintf("shell index %d out of range (session %q has %d shell(s))", idx, sess.ID, len(sess.ListChildShells())), http.StatusNotFound)
			return
		}
		info = cs.Info()
		resp.Kind = "shell"
		resp.ShellID = info.ID
		resp.Index = idx
		resp.Name = info.Name
		resp.Status = string(info.Status)
		writeJSON(w, http.StatusOK, resp)
		return
	}

	http.Error(w, fmt.Sprintf("session %q not found (list live sessions with GET /api/sessions)", p.SessionID), http.StatusNotFound)
}
