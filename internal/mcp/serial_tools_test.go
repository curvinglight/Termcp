package mcp

import (
	"errors"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/connections"
	"github.com/open-mcp-ai/termcp/internal/serial"
	"github.com/open-mcp-ai/termcp/internal/serial/port"
	"github.com/open-mcp-ai/termcp/internal/session"
)

// TestSerialErrCodeMapping pins the whole serial error-code table at once.
//
// The table is the contract design §7 asks for: one code per condition, no
// matter which tool the condition surfaced through. Asserting it as a table
// (rather than per call site) is what keeps a new sentinel from being added to
// the mapper by accident-order or, worse, left falling through to
// operation_failed — a caller branching on serial_port_busy must get the same
// answer whether the failure came from a session start, a config save, or a
// control call.
func TestSerialErrCodeMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"single channel", session.ErrSerialSingleChannel, CodeSerialSingleChannel},
		{"no pty capability", session.ErrUnsupported, CodeSerialUnsupported},
		{"device in use", port.ErrBusy, CodeSerialPortBusy},
		{"no such port", port.ErrNotFound, CodeSerialPortNotFound},
		{"profile missing", serial.ErrNotFound, CodeSerialPortNotFound},
		{"permission", port.ErrPermission, CodeSerialPermissionDenied},
		{"unplugged", port.ErrDeviceGone, CodeSerialDeviceGone},
		{"unknown format", port.ErrUnsupportedFormat, CodeSerialUnsupportedFormat},
		{"bad profile", port.ErrInvalidConfig, CodeInvalidArgument},
		{"flow control", serial.ErrFlowControl, CodeSerialFlowControl},
		{"ambiguous profile", connections.ErrAmbiguous, CodeProfileAmbiguous},
		{"unknown connection", connections.ErrNotFound, CodeSSHConfigNotFound},
		{"plain failure", errors.New("something broke"), CodeOperationFailed},
		{"nil", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := serialErrCode(tc.err); got != tc.want {
				t.Errorf("serialErrCode(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// TestSerialErrCodeWrapping proves the mapper matches through a %w wrap. Real
// call sites wrap (a profile validation error carries the profile name), and a
// mapper that only matched the bare sentinel would downgrade every wrapped
// failure to operation_failed.
func TestSerialErrCodeWrapping(t *testing.T) {
	wrapped := errors.Join(errors.New("profile esp32"), port.ErrBusy)
	if got := serialErrCode(wrapped); got != CodeSerialPortBusy {
		t.Errorf("wrapped ErrBusy mapped to %q, want %q", got, CodeSerialPortBusy)
	}
}
