package mcpbridge

import (
	"context"

	"github.com/charmbracelet/ssh"

	"github.com/open-mcp-ai/termcp/internal/sshserver"
)

// RegisterBuiltin teaches an internal SSH server to serve BuiltinCommand
// in-process: a session that asks for it is handed to this relay, and the
// relay's own stdin/stdout/stderr are the SSH channel's. No process is spawned
// and no PATH is consulted, which is the whole reason an extension can reach an
// HTTP MCP server without a second binary to install.
//
// It lives here rather than in the sshserver because the transport should not
// know what a relay is: sshserver carries commands, this package is the one that
// knows how to be one.
func RegisterBuiltin(s *sshserver.Server) {
	if s == nil {
		return
	}
	s.RegisterCommand(BuiltinCommand, func(ctx context.Context, sess ssh.Session, args []string) int {
		return CommandHandler(ctx, sess, sess, sess.Stderr(), args)
	})
}
