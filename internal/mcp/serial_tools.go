package mcp

import (
	"context"
	"fmt"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/open-mcp-ai/termcp/internal/serial"
	"github.com/open-mcp-ai/termcp/internal/serial/port"
)

// registerSerialTools adds the serial tools. They are only registered when a
// serial store is present, so an instance started with --no-serial simply does
// not advertise them rather than advertising tools that refuse to work.
func (s *Server) registerSerialTools(mcpServer *mcpserver.MCPServer) {
	mcpServer.AddTool(s.newTool("serial_ports",
		mcpgo.WithDescription("List this machine's serial ports: name, stable by-id path where available, USB vendor/product ids. "+
			"Read-only; use the by-id path in a profile when one is offered, because ttyUSB numbers change with plug order."),
	), withLogging("serial_ports", s.handleSerialPorts))

	mcpServer.AddTool(s.newTool("serial_config",
		mcpgo.WithDescription("Serial connection profiles: action=list returns the full contents of every profile. "+
			"Notes: tx_eol=\"none\" means Enter sends a bare CR, NOT that no line ending is sent. "+
			"flow_control must be \"none\" (the transport cannot honour it). "+
			"Only one profile may claim a given device."),
		mcpgo.WithString("action", mcpgo.Required(), mcpgo.Enum("list", "read", "create", "update", "delete")),
		mcpgo.WithString("name", mcpgo.Description("Profile name (required for every action except list)")),
		mcpgo.WithString("config", mcpgo.Description("Full profile TOML (required for create/update; update replaces the profile entirely)")),
	), withLogging("serial_config", s.handleSerialConfigOps))

	mcpServer.AddTool(s.newTool("serial_ctl",
		mcpgo.WithDescription("Control a running serial session's modem lines and buffers. "+
			"dtr/rts drive an output line (the usual way to reset a board into a bootloader); break holds the line in the break state; "+
			"flush discards buffered bytes; status reads the input lines. These change device state, so they are reviewed like input when review mode is on."),
		mcpgo.WithString("action", mcpgo.Required(), mcpgo.Enum("dtr", "rts", "break", "flush", "status")),
		mcpgo.WithString("shell_id", mcpgo.Required()),
		mcpgo.WithString("level", mcpgo.Description("high|low (dtr and rts)")),
		mcpgo.WithNumber("duration_ms", mcpgo.Description("Break duration in milliseconds (break)")),
		mcpgo.WithString("direction", mcpgo.Description("in|out|both (flush)")),
	), withLogging("serial_ctl", s.handleSerialCtl))
}

// handleSerialPorts lists the machine's serial ports.
//
// It is a read-only capability and is deliberately not gated behind
// --mcp-manage-serial-configs: listing what is plugged in changes nothing.
func (s *Server) handleSerialPorts(_ context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.serialConfigs == nil {
		return toolError(CodeSerialDisabled, "%s", "serial support is not enabled on this instance"), nil
	}
	infos, err := port.List()
	if err != nil {
		return toolError(serialErrCode(err), "%s", err.Error()), nil
	}
	return jsonResult(map[string]any{"ports": infos}), nil
}

// handleSerialConfigOps serves the serial profile store.
//
// list returns full profile contents, unlike ssh_config(action=list) which
// returns names only. The asymmetry is intentional: a serial profile holds no
// credentials, so there is nothing to withhold, and showing the settings is what
// makes the tool usable for checking a device configuration.
func (s *Server) handleSerialConfigOps(_ context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.serialConfigs == nil {
		return toolError(CodeSerialDisabled, "%s", "serial support is not enabled on this instance"), nil
	}

	args := request.GetArguments()
	action := strings.ToLower(strings.TrimSpace(getString(args, "action", "list")))
	name := strings.TrimSpace(getString(args, "name", ""))

	switch action {
	case "list":
		names, err := s.serialConfigs.List()
		if err != nil {
			return toolError(serialErrCode(err), "%s", err.Error()), nil
		}
		profiles := make(map[string]string, len(names))
		for _, n := range names {
			raw, rerr := s.serialConfigs.ReadRaw(n)
			if rerr != nil {
				// One unreadable profile must not hide the rest; report it in
				// place so the caller can see which one is broken.
				profiles[n] = fmt.Sprintf("<unreadable: %v>", rerr)
				continue
			}
			profiles[n] = string(raw)
		}
		return jsonResult(map[string]any{"profiles": profiles, "kind": serial.Kind}), nil

	case "read":
		if name == "" {
			return toolError(CodeInvalidArgument, "%s", "name is required"), nil
		}
		raw, err := s.serialConfigs.ReadRaw(name)
		if err != nil {
			return toolError(serialErrCode(err), "%s", err.Error()), nil
		}
		return jsonResult(map[string]any{"name": name, "config": string(raw)}), nil

	case "create", "update", "delete":
		if !s.serialConfigWrites {
			return toolError(CodeOperationFailed, "%s",
				"serial config write actions are disabled; start termcp with --mcp-manage-serial-configs"), nil
		}
	default:
		return toolError(CodeInvalidArgument, "%s", fmt.Sprintf("unknown action %q", action)), nil
	}

	if name == "" {
		return toolError(CodeInvalidArgument, "%s", "name is required"), nil
	}

	switch action {
	case "delete":
		if err := s.serialConfigs.Delete(name); err != nil {
			return toolError(serialErrCode(err), "%s", err.Error()), nil
		}
		return successResult(), nil

	case "create", "update":
		// create and update are the same operation: a profile is replaced
		// whole. Distinguishing them would mean "update" got a merge rule, and
		// a merge rule is how a setting someone removed comes back.
		raw := getString(args, "config", "")
		if strings.TrimSpace(raw) == "" {
			return toolError(CodeInvalidArgument, "%s", "config is required for create/update"), nil
		}
		if err := s.serialConfigs.Save(name, []byte(raw)); err != nil {
			return toolError(serialErrCode(err), "%s", err.Error()), nil
		}
		return successResult(), nil
	}

	return toolError(CodeInternalError, "%s", "unreachable: action not handled"), nil
}

// handleSerialCtl drives a serial session's modem lines and buffers.
//
// These actions change device state — DTR and break can reset a board — so they
// go through the same review gate as terminal input when review mode is on. The
// read-only status action does not: it changes nothing, and gating it would make
// the operator wait for approval to look at a pin.
func (s *Server) handleSerialCtl(_ context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.serialConfigs == nil {
		return toolError(CodeSerialDisabled, "%s", "serial support is not enabled on this instance"), nil
	}

	args := request.GetArguments()
	action := strings.ToLower(strings.TrimSpace(getString(args, "action", "")))
	shellID := strings.TrimSpace(getString(args, "shell_id", ""))
	if shellID == "" {
		return toolError(CodeInvalidArgument, "%s", "shell_id is required"), nil
	}

	shell, bad := s.requireShell(shellID)
	if bad != nil {
		return bad, nil
	}

	ctrl := shell.Controller()
	if ctrl == nil {
		return toolError(CodeSerialUnsupported, "%s",
			fmt.Sprintf("shell '%s' is not a serial session; serial_ctl works on serial devices only", shellID)), nil
	}

	// A state-changing action is a write, and review mode gates writes: the same
	// reason it gates input applies twice over here, because toggling DTR can
	// reset the device.
	if action != "status" && shell.RequiresApproval() {
		return reviewPendingResult(false), nil
	}

	switch action {
	case "dtr", "rts":
		level := strings.ToLower(strings.TrimSpace(getString(args, "level", "")))
		if level != "high" && level != "low" {
			return toolError(CodeInvalidArgument, "%s", `level must be "high" or "low"`), nil
		}
		on := level == "high"
		var err error
		if action == "dtr" {
			err = ctrl.SetDTR(on)
		} else {
			err = ctrl.SetRTS(on)
		}
		if err != nil {
			return toolError(serialErrCode(err), "%s", err.Error()), nil
		}
		return jsonResult(map[string]any{"ok": true, "action": action, "level": level}), nil

	case "break":
		ms := int(getFloat64(args, "duration_ms", 250))
		if ms <= 0 || ms > 60_000 {
			return toolError(CodeInvalidArgument, "%s", "duration_ms must be between 1 and 60000"), nil
		}
		if err := ctrl.Break(time.Duration(ms) * time.Millisecond); err != nil {
			return toolError(serialErrCode(err), "%s", err.Error()), nil
		}
		return jsonResult(map[string]any{"ok": true, "action": "break", "duration_ms": ms}), nil

	case "flush":
		direction := strings.ToLower(strings.TrimSpace(getString(args, "direction", "both")))
		switch direction {
		case "in":
			if err := ctrl.FlushInput(); err != nil {
				return toolError(serialErrCode(err), "%s", err.Error()), nil
			}
		case "out":
			if err := ctrl.FlushOutput(); err != nil {
				return toolError(serialErrCode(err), "%s", err.Error()), nil
			}
		case "both":
			if err := ctrl.FlushInput(); err != nil {
				return toolError(serialErrCode(err), "%s", err.Error()), nil
			}
			if err := ctrl.FlushOutput(); err != nil {
				return toolError(serialErrCode(err), "%s", err.Error()), nil
			}
		default:
			return toolError(CodeInvalidArgument, "%s", `direction must be "in", "out" or "both"`), nil
		}
		return jsonResult(map[string]any{"ok": true, "action": "flush", "direction": direction}), nil

	case "status":
		cts, dsr, ri, dcd, err := ctrl.ModemStatus()
		if err != nil {
			return toolError(serialErrCode(err), "%s", err.Error()), nil
		}
		return jsonResult(map[string]any{
			"cts": cts, "dsr": dsr, "ri": ri, "dcd": dcd,
		}), nil

	default:
		return toolError(CodeInvalidArgument, "%s", fmt.Sprintf("unknown action %q", action)), nil
	}
}
