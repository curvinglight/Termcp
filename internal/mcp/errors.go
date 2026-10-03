package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/open-mcp-ai/termcp/internal/connections"
	"github.com/open-mcp-ai/termcp/internal/forward"
	"github.com/open-mcp-ai/termcp/internal/serial"
	"github.com/open-mcp-ai/termcp/internal/serial/port"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
)

// errMissingSSHConfig is returned when the caller omits ssh_config.
// session_start deliberately refuses to fall back to a default profile so a
// forgotten ssh_config cannot silently open a shell on the wrong host.
var errMissingSSHConfig = errors.New("ssh_config is required")

// errInvalidResourceLocator is returned when ssh_config is written in termcp://
// syntax but is malformed, or names a session/shell instead of an entry.
var errInvalidResourceLocator = errors.New("invalid resource locator")

// sshConfigErrCode maps an ssh_config store failure onto its tool error code.
func sshConfigErrCode(err error) string {
	if errors.Is(err, errMissingSSHConfig) || errors.Is(err, errInvalidResourceLocator) {
		return CodeInvalidArgument
	}
	if errors.Is(err, sshconfig.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
		return CodeSSHConfigNotFound
	}
	return CodeOperationFailed
}

// forwardErrCode maps a forward-manager failure onto its tool error code.
func forwardErrCode(err error) string {
	if errors.Is(err, forward.ErrNotFound) {
		return CodeForwardNotFound
	}
	return CodeOperationFailed
}

// Tool error codes. Like a function's (value, error) return, a failed tool
// result carries a dedicated machine-readable status so callers can branch on
// the failure kind instead of string-matching the message. Codes are stable
// snake_case identifiers; success results never carry one.
const (
	CodeInvalidArgument     = "invalid_argument"
	CodeSessionNotFound     = "session_not_found"
	CodeShellNotFound       = "shell_not_found"
	CodeSessionNotRunning   = "session_not_running"
	CodeReaderNotRegistered = "reader_not_registered"
	CodeHistoryNotFound     = "history_not_found"
	CodeForwardNotFound     = "forward_not_found"
	CodeSSHConfigNotFound   = "ssh_config_not_found"
	CodeRuleNotFound        = "rule_not_found"
	CodeConflict            = "conflict"
	CodeNotConfigured       = "not_configured"
	CodeConnectionFailed    = "connection_failed"
	CodeOperationFailed     = "operation_failed"
	CodeInternalError       = "internal_error"

	// Serial codes. They exist so a caller can tell "this device is busy" from
	// "this session has no SSH" without reading prose, and so the same condition
	// reports the same code wherever it is hit.
	CodeSerialPortBusy           = "serial_port_busy"
	CodeSerialPortNotFound       = "serial_port_not_found"
	CodeSerialPermissionDenied   = "serial_permission_denied"
	CodeSerialUnsupportedFormat  = "serial_unsupported_format"
	CodeSerialFlowControl        = "serial_flow_control_unsupported"
	CodeSerialDeviceGone         = "serial_device_gone"
	CodeSerialSingleChannel      = "serial_single_channel"
	CodeSerialNoPTY              = "serial_no_pty"
	CodeSerialUnsupported        = "serial_unsupported"
	CodeSerialDisabled           = "serial_disabled"
	CodeSerialCommandUnsupported = "serial_command_unsupported"
	CodeProfileAmbiguous         = "profile_name_ambiguous"
)

// serialErrCode maps a serial port or profile failure onto its tool error code.
//
// It is a single mapping rather than a message per call site: the same failure
// reaching the user through a different tool should read the same way, and a
// caller branching on the code should get one answer per condition.
func serialErrCode(err error) string {
	switch {
	case err == nil:
		return ""
	// The argument-shape failures come from the shared resolver path, so they
	// keep the codes the ssh-only path already used. Mapping them here rather
	// than at each call site is what keeps a malformed locator reported as an
	// invalid argument whichever profile kind it names.
	case errors.Is(err, errMissingSSHConfig), errors.Is(err, errInvalidResourceLocator):
		return CodeInvalidArgument
	case errors.Is(err, serial.ErrNotFound):
		return CodeSerialPortNotFound
	case errors.Is(err, connections.ErrAmbiguous):
		return CodeProfileAmbiguous
	case errors.Is(err, connections.ErrNotFound):
		return CodeSSHConfigNotFound
	case errors.Is(err, port.ErrBusy):
		return CodeSerialPortBusy
	case errors.Is(err, port.ErrNotFound):
		return CodeSerialPortNotFound
	case errors.Is(err, port.ErrPermission):
		return CodeSerialPermissionDenied
	case errors.Is(err, port.ErrDeviceGone):
		return CodeSerialDeviceGone
	case errors.Is(err, port.ErrUnsupportedFormat):
		return CodeSerialUnsupportedFormat
	case errors.Is(err, port.ErrInvalidConfig):
		return CodeInvalidArgument
	case errors.Is(err, session.ErrSerialSingleChannel):
		return CodeSerialSingleChannel
	case errors.Is(err, session.ErrUnsupported):
		return CodeSerialUnsupported
	default:
		return CodeOperationFailed
	}
}

// toolError builds a failed tool result whose text content is a JSON object
// with a dedicated error_code field plus a human-readable message:
//
//	{"error_code":"shell_not_found","error":"Shell 'abc' not found"}
//
// The dedicated field lets agents branch on the failure kind (the moral
// equivalent of a typed error) without parsing prose. Only failed results
// carry it; success results keep their normal payload.
func toolError(code, format string, args ...any) *mcpgo.CallToolResult {
	msg := format
	if len(args) > 0 {
		msg = fmt.Sprintf(format, args...)
	}
	payload, err := json.Marshal(struct {
		ErrorCode string `json:"error_code"`
		Error     string `json:"error"`
	}{ErrorCode: code, Error: msg})
	if err != nil {
		// json.Marshal of two strings cannot fail; keep a safe fallback.
		payload = []byte(fmt.Sprintf(`{"error_code":%q,"error":"failed to encode tool error"}`, CodeInternalError))
	}
	return &mcpgo.CallToolResult{
		IsError: true,
		Content: []mcpgo.Content{
			mcpgo.NewTextContent(string(payload)),
		},
	}
}
