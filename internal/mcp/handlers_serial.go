package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/open-mcp-ai/termcp/internal/serial"
	"github.com/open-mcp-ai/termcp/internal/serial/port"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// The serial surface. Two internal operations back the directives:
//
//	serial_ports — the machine's ports (ls port), read-only
//	serial_ctl   — modem lines and buffers (set <channel> …), reviewed
//
// and one branch of session_start: a host profile with kind = "serial" opens a
// device, not a connection. Everything here goes through the same callTool
// path the directives use, so admission, review and namespace checks apply
// exactly as they do to every other operation.

// errSerialRefusal builds the refusal an SSH-only operation gives a serial
// session. It is a distinct error (not a formatted message) so the handler can
// answer serial_unsupported and the caller can tell "no SSH features" apart
// from "the session is not running".
type serialRefusal struct{ what string }

func (e *serialRefusal) Error() string { return e.what }

// startSerialSession opens a serial profile as a session.
//
// command, args, mode and a size are refused rather than ignored: a serial
// device has no process to run and no shell to select, so accepting the
// parameters would mean silently discarding what the caller asked for, and the
// session would not be what the request described.
func (s *Server) startSerialSession(ctx context.Context, args map[string]any, profileName string, ent *sshconfig.Entry) (*mcpgo.CallToolResult, error) {
	if strings.TrimSpace(getString(args, "command", "")) != "" || len(getStringSlice(args, "args")) > 0 {
		return toolError(CodeInvalidArgument, "%s",
			"a serial session has no process to run; command/args are not supported"), nil
	}
	if mode := strings.TrimSpace(getString(args, "mode", "")); mode != "" {
		return toolError(CodeInvalidArgument, "%s",
			"a serial session has no shell mode; mode is not supported (the device is one stream)"), nil
	}
	for _, k := range []string{"rows", "cols"} {
		if _, ok := args[k]; ok {
			return toolError(CodeInvalidArgument, "%s",
				"a serial device has no terminal to size; rows/cols are not supported"), nil
		}
	}

	spec, err := sshconfig.SerialFromEntry(ent)
	if err != nil {
		return toolError(sshConfigErrCode(err), "%s", err.Error()), nil
	}

	sessName := strings.TrimSpace(getString(args, "name", ""))
	if sessName == "" {
		sessName = profileName
	}

	sess, err := s.sessMgr.Create(session.Config{
		Name:      sessName,
		SSHConfig: profileName,
		// Rows/Cols stay zero: a serial device has no terminal, so there is no
		// size to report. The front-end xterm manages its own layout.
		Serial: &session.SerialConfig{
			ProfileName: profileName,
			Spec:        spec,
		},
		Approval:  ent.DefaultApproval,
		Namespace: callerNamespace(ctx),
		Ctx:       ctx,
	})
	if err != nil {
		if errors.Is(err, serial.ErrFlowControl) {
			return toolError(CodeInvalidArgument, "%s", err.Error()), nil
		}
		if errors.Is(err, port.ErrBusy) {
			return toolError(CodeConflict, "%s", err.Error()), nil
		}
		if errors.Is(err, port.ErrPermission) || errors.Is(err, port.ErrNotFound) {
			return toolError(CodeConnectionFailed, "%s", err.Error()), nil
		}
		return toolError(CodeConnectionFailed, "%s", err.Error()), nil
	}

	result := map[string]any{
		"session_id": sess.ID,
		"shell_id":   sess.PrimaryShellID(),
		"ssh_config": profileName,
		"index":      sess.PrimaryShellIndex(),
		"kind":       string(api.KindSerial),
		"device":     spec.Device,
		"baud":       spec.Baud,
	}
	return jsonResult(result), nil
}

// requireSSHSession resolves a session and refuses it when the transport is
// serial. SSH-only features (forwards, SFTP) hang off the SSH client, and the
// callers used to answer a serial session with session_not_running — sending
// the caller looking for a dead session instead of at the transport.
func (s *Server) requireSSHSession(sessionID string) (*session.Session, *mcpgo.CallToolResult) {
	sess, bad := s.requireSession(sessionID)
	if bad != nil {
		return nil, bad
	}
	if !sess.SupportsSSH() {
		return nil, toolError(CodeSerialUnsupported, "%s",
			fmt.Sprintf("session '%s' is a serial session; this operation needs an SSH connection", sessionID))
	}
	return sess, nil
}

// handleSerialPorts lists this machine's serial ports. Read-only: listing what
// is plugged in changes nothing, so the capability is the operator's own.
func (s *Server) handleSerialPorts(_ context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	infos, err := port.List()
	if err != nil {
		return toolError(CodeOperationFailed, "%s", err.Error()), nil
	}
	out := make([]map[string]any, 0, len(infos))
	for _, in := range infos {
		m := map[string]any{"name": in.Name}
		if in.StableName != "" {
			m["stable_name"] = in.StableName
		}
		if in.IsUSB {
			m["usb"] = map[string]any{"vid": in.VID, "pid": in.PID}
			if in.SerialNumber != "" {
				m["usb"].(map[string]any)["serial"] = in.SerialNumber
			}
			if in.Product != "" {
				m["usb"].(map[string]any)["product"] = in.Product
			}
			if in.Manufacturer != "" {
				m["usb"].(map[string]any)["manufacturer"] = in.Manufacturer
			}
		}
		out = append(out, m)
	}
	return jsonResult(map[string]any{"ports": out}), nil
}

// handleSerialCtl drives a serial device's modem lines and buffers. The
// capability row is GateReview, so a queued call lands here only after a human
// approved it — the same executor replays it through handlerFor.
func (s *Server) handleSerialCtl(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	shellID := getString(args, "shell_id", "")
	if shellID == "" {
		return toolError(CodeInvalidArgument, "%s", "shell_id is required"), nil
	}
	action := getString(args, "action", "")
	shell, bad := s.requireShell(shellID)
	if bad != nil {
		return bad, nil
	}
	ctrl := shell.Controller()
	if ctrl == nil {
		return toolError(CodeSerialUnsupported, "%s",
			fmt.Sprintf("shell '%s' is not a serial channel; modem lines and buffers are a serial-device capability", shellID)), nil
	}

	switch action {
	case "dtr", "rts":
		level := strings.ToLower(strings.TrimSpace(getString(args, "level", "")))
		if level != "high" && level != "low" {
			return toolError(CodeInvalidArgument, "%s", "level must be high or low"), nil
		}
		var err error
		if action == "dtr" {
			err = ctrl.SetDTR(level == "high")
		} else {
			err = ctrl.SetRTS(level == "high")
		}
		if err != nil {
			return toolError(CodeOperationFailed, "%s", err.Error()), nil
		}
		return jsonResult(map[string]any{"ok": true, "action": action, "level": level}), nil

	case "break":
		ms := int(getFloat64(args, "duration_ms", 250))
		if ms < 0 || ms > 10_000 {
			return toolError(CodeInvalidArgument, "%s", "duration_ms must be between 0 and 10000"), nil
		}
		if err := ctrl.Break(time.Duration(ms) * time.Millisecond); err != nil {
			return toolError(CodeOperationFailed, "%s", err.Error()), nil
		}
		return jsonResult(map[string]any{"ok": true, "action": "break", "duration_ms": ms}), nil

	case "flush":
		dir := strings.ToLower(strings.TrimSpace(getString(args, "direction", "both")))
		var err error
		switch dir {
		case "in":
			err = ctrl.FlushInput()
		case "out":
			err = ctrl.FlushOutput()
		case "both", "":
			err = errors.Join(ctrl.FlushInput(), ctrl.FlushOutput())
		default:
			return toolError(CodeInvalidArgument, "%s", "direction must be in, out or both"), nil
		}
		if err != nil {
			return toolError(CodeOperationFailed, "%s", err.Error()), nil
		}
		return jsonResult(map[string]any{"ok": true, "action": "flush", "direction": dir}), nil

	case "status":
		cts, dsr, ri, dcd, err := ctrl.ModemStatus()
		if err != nil {
			return toolError(CodeOperationFailed, "%s", err.Error()), nil
		}
		return jsonResult(map[string]any{
			"ok": true, "action": "status",
			"cts": cts, "dsr": dsr, "ri": ri, "dcd": dcd,
		}), nil

	default:
		return toolError(CodeInvalidArgument, "%s",
			"action must be dtr, rts, break, flush or status"), nil
	}
}

// serialPortsListText renders `ls port`: one port per line, the by-id path
// preferred because ttyUSB numbers change with plug order while the by-id name
// does not.
func serialPortsListText(ports []map[string]any) string {
	var b strings.Builder
	for _, p := range ports {
		name, _ := p["name"].(string)
		line := name
		if stable, ok := p["stable_name"].(string); ok && stable != "" {
			line += "\t" + stable
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}
