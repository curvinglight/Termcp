package session

import (
	"fmt"
	"os"

	"github.com/open-mcp-ai/termcp/internal/encoding"
)

// GetChildShell searches all sessions for a child shell with the given ID.
// Returns nil if no matching child shell is found.
func (m *Manager) GetChildShell(id string) *ChildShell {
	var found *ChildShell
	m.sessions.Range(func(_, v any) bool {
		if cs := v.(*Session).GetChildShell(id); cs != nil {
			found = cs
			return false
		}
		return true
	})
	return found
}

// GetByShellID returns the parent session that owns the given shell_id.
func (m *Manager) GetByShellID(shellID string) *Session {
	var found *Session
	m.sessions.Range(func(_, v any) bool {
		s := v.(*Session)
		if s.GetChildShell(shellID) != nil {
			found = s
			return false
		}
		return true
	})
	return found
}

// GetSessionByShellID returns the session that owns a shell_id, searching both
// live in-memory shells and retained DEAD/restored shell snapshots. This keeps
// output-range resolvable for read-only DEAD views after a transport teardown
// or restart when no live ChildShell object exists for the id.
// DisplayEncodingFor resolves the presentation encoding for an id that may name
// a session or one of its shells, including a shell whose session is DEAD or
// restored (no live ChildShell, only the retained snapshot).
//
// The outlets that convert raw bytes — the range endpoints, the scrollback
// rail, the MCP tail and offset reads — hold an id, not a session, and they
// must all land on the same answer as the live path or a replayed transcript
// would render differently from the live one it came from.
//
// "text" for an unknown id: those callers are about to fail the lookup they
// care about, and an encoding is not what they are validating.
func (m *Manager) DisplayEncodingFor(id string) string {
	if s := m.Get(id); s != nil {
		return s.DisplayEncoding()
	}
	if s := m.GetSessionByShellID(id); s != nil {
		return s.DisplayEncoding()
	}
	return encoding.Text
}

// CharsetFor resolves the charset an id's bytes decode from: the session's
// effective choice (override → profile → default). The id may name a session or
// a shell; "auto" is a real answer here — the caller that can sniff (the
// transcript) handles it, the caller that cannot (the streaming decoder) asks
// for the latch.
//
// "" for an unknown id: those callers are about to fail the lookup they care
// about, and a charset is not what they are validating.
func (m *Manager) CharsetFor(id string) string {
	if s := m.Get(id); s != nil {
		return s.EffectiveCharset()
	}
	if s := m.GetSessionByShellID(id); s != nil {
		return s.EffectiveCharset()
	}
	return ""
}

// LatchCharsetFor records the charset the auto sniff decided on for an id's
// session, so the live stream does not re-decide from one burst to the next.
func (m *Manager) LatchCharsetFor(id, cs string) {
	s := m.Get(id)
	if s == nil {
		s = m.GetSessionByShellID(id)
	}
	if s == nil {
		return
	}
	s.LatchCharset(cs)
}

// LatchedCharset returns the charset a previous auto sniff decided on for an
// id's session, or "" when the sniff has not resolved yet.
func (m *Manager) LatchedCharset(id string) string {
	s := m.Get(id)
	if s == nil {
		s = m.GetSessionByShellID(id)
	}
	if s == nil {
		return ""
	}
	return s.latchedCharset()
}

// SetCharsetOverride installs the runtime charset for an id's session. "auto"
// and "" are legal (auto re-arms the sniff; "" returns to the profile's
// choice); anything else must be a name the encoding table maps, so a typo in
// a selector cannot silently turn decoding off.
func (m *Manager) SetCharsetOverride(id, cs string) error {
	switch {
	case cs == "" || cs == "auto":
	case encoding.CharsetValid(cs):
	default:
		return fmt.Errorf("charset must be %q, %q or one of: %s", "auto", "utf-8", encoding.SupportedCharsets())
	}
	s := m.Get(id)
	if s == nil {
		s = m.GetSessionByShellID(id)
	}
	if s == nil {
		return os.ErrNotExist
	}
	s.SetCharsetOverride(cs)
	return nil
}

func (m *Manager) GetSessionByShellID(shellID string) *Session {
	var found *Session
	m.sessions.Range(func(_, v any) bool {
		s := v.(*Session)
		if s.GetChildShell(shellID) != nil {
			found = s
			return false
		}
		if _, ok := s.shellHistory.Load(shellID); ok {
			found = s
			return false
		}
		return true
	})
	return found
}

// CloseChildShell terminates a child shell by its ID and removes it from the owning
// parent session's map. Returns found=false if no such child shell exists.
// The parent session and its SSH connection are unaffected.
func (m *Manager) CloseChildShell(id string) (bool, error) {
	var parent *Session
	m.sessions.Range(func(_, v any) bool {
		s := v.(*Session)
		if s.GetChildShell(id) != nil {
			parent = s
			return false
		}
		return true
	})
	if parent == nil {
		return false, nil
	}
	err := parent.CloseChildShell(id)
	// Persist the updated per-shell snapshot right away so a restart cannot
	// resurrect the closed shell from the shell manifest; notify the UI so closed
	// shells disappear from tabs immediately. Only this parent is written: a shell
	// belongs to one session, so no other session's manifests can be affected.
	m.persistOne(parent.ID)
	m.notifyListChange()
	return true, err
}

// ArchiveChildShell stops a child shell by its ID and KEEPS its record, unlike
// CloseChildShell which erases it. Returns found=false if no such shell is live.
//
// The parent and its SSH transport are unaffected: only the channel ends.
func (m *Manager) ArchiveChildShell(id string) (bool, error) {
	var parent *Session
	m.sessions.Range(func(_, v any) bool {
		s := v.(*Session)
		if s.GetChildShell(id) != nil {
			parent = s
			return false
		}
		return true
	})
	if parent == nil {
		return false, nil
	}
	err := parent.ArchiveChildShell(id)
	// The snapshot now carries the archived shell, so it must be written out: a
	// restart has to find the channel in the manifest, not just in memory. This is
	// the one persist call whose job is to CREATE an entry rather than remove one.
	m.persistOne(parent.ID)
	m.notifyListChange()
	return true, err
}

// resolveShellID fills in the session's primary shell when a caller passes an
// empty shellID. Log files are addressed per shell, so an empty id would name a
// path that does not exist and read back nothing; callers that only hold a
// session id (DEAD/restored sessions) depend on this translation.
func (m *Manager) resolveShellID(sessionID, shellID string) string {
	if shellID != "" {
		return shellID
	}
	s := m.Get(sessionID)
	if s == nil {
		return ""
	}
	if cs := s.PrimaryShell(); cs != nil {
		return cs.ID
	}
	// DEAD/restored sessions have no live shell objects; the snapshot holds the
	// shells as they were when the session went down.
	if sh := s.SnapshotShells(); len(sh) > 0 {
		return sh[0].ID
	}
	return ""
}
