package mcp

import (
	"context"
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// maxNotifyMessageLen caps the message shown in the Web UI toast.
const maxNotifyMessageLen = 2000

// handleNotifyUser posts a user-facing notification to every open termcp Web UI
// tab (toast + browser system notification) and optionally highlights the card
// of the given session. It reaches the human at the browser; nothing in termcp
// wakes an agent, which is what the retired reverse-wake-up rules tried to do.
func (s *Server) handleNotifyUser(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	message := strings.TrimSpace(getString(args, "message", ""))
	if message == "" {
		return toolError(CodeInvalidArgument, "%s", "message is required"), nil
	}
	if r := []rune(message); len(r) > maxNotifyMessageLen {
		message = string(r[:maxNotifyMessageLen])
	}

	level := strings.TrimSpace(getString(args, "level", "info"))
	if level == "" {
		level = "info"
	}
	switch level {
	case "info", "success", "warn", "error":
	default:
		return toolError(CodeInvalidArgument, "%s", "level must be info, success, warn, or error"), nil
	}

	title := strings.TrimSpace(getString(args, "title", ""))
	if title == "" {
		title = "termcp"
	}

	durationSec := int(getFloat64(args, "duration_seconds", 10))
	if durationSec < 0 {
		durationSec = 0
	}
	if durationSec > 600 {
		durationSec = 600
	}

	sessionID := strings.TrimSpace(getString(args, "session_id", ""))
	if sessionID != "" {
		sess, bad := s.requireSession(sessionID)
		if bad != nil {
			return bad, nil
		}
		// The resolved id is what the Web UI matches when it highlights a card.
		sessionID = sess.ID
	}

	delivered := 0
	if s.uiNotify != nil {
		delivered = s.uiNotify(level, title, message, sessionID, durationSec)
	}
	out := map[string]any{"ok": true, "delivered": delivered, "title": title, "level": level}
	if sessionID != "" {
		out["session_id"] = sessionID
	}
	if delivered == 0 {
		out["hint"] = "no Web UI tab is open; the notification was not displayed"
	}
	return jsonResult(out), nil
}
