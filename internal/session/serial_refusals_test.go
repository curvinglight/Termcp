package session

import (
	"errors"
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/pkg/api"
)

// TestSerialSessionRefusesSecondChannel pins the refusal a serial session gives
// when something asks for another channel (design §9.1).
//
// The error must be ErrSerialSingleChannel by identity, not by message: the MCP
// layer maps the sentinel to serial_single_channel, and an agent branching on
// that code must not depend on English prose. The message check matters just as
// much in the other direction — the fall-through path reports "sub-shell
// multiplexing requires an SSH connection", which is about internal loopback
// sessions and would send a reader hunting for an SSH config that has nothing
// to do with a serial device.
func TestSerialSessionRefusesSecondChannel(t *testing.T) {
	s := &Session{Session: api.Session{
		Kind:   api.KindSerial,
		Status: api.SessionRunning,
	}}

	_, err := s.CreateChildShell("", nil, false, 24, 80, "")
	if !errors.Is(err, ErrSerialSingleChannel) {
		t.Fatalf("CreateChildShell error = %v, want ErrSerialSingleChannel", err)
	}
	if strings.Contains(err.Error(), "sub-shell multiplexing") {
		t.Errorf("serial refusal reused the loopback message: %v", err)
	}
}

// TestSerialResizePtyIsUnsupported pins the other serial refusal: a serial
// device has no terminal to resize. The serial branch must answer before the
// mode checks, so the error says why (serial), not that the shell is in pipe
// mode — a distinction the MCP layer maps to serial_no_pty.
func TestSerialResizePtyIsUnsupported(t *testing.T) {
	cs := &ChildShell{serial: true, Status: api.SessionRunning, mode: api.ModePipe}
	err := cs.ResizePty(24, 80)
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("ResizePty error = %v, want ErrUnsupported", err)
	}
	if !strings.Contains(err.Error(), "serial") {
		t.Errorf("ResizePty refusal does not name the serial reason: %v", err)
	}
}
