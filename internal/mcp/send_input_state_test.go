package mcp

import (
	"context"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// startInteractiveShell opens a pty session and returns its shell_id.
func startInteractiveShell(t *testing.T, s *Server) string {
	t.Helper()
	startResult, err := s.handleStartSession(context.Background(), makeRequest(map[string]any{
		"command":    testShell(),
		"args":       testInteractiveShellArgs(),
		"mode":       "pty",
		"ssh_config": "internal",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if startResult.IsError {
		t.Fatalf("start_session error: %s", startResult.Content[0].(mcpgo.TextContent).Text)
	}
	shellID, _ := parseResult(t, startResult)["shell_id"].(string)
	if shellID == "" {
		t.Fatal("start_session returned no shell_id")
	}
	return shellID
}

// sendInput calls shell_input and returns the parsed reply.
func sendInput(t *testing.T, s *Server, shellID, text string) map[string]any {
	t.Helper()
	res, err := s.handleSendInput(context.Background(), makeRequest(map[string]any{
		"shell_id": shellID,
		"text":     text,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("shell_input error: %s", res.Content[0].(mcpgo.TextContent).Text)
	}
	return parseResult(t, res)
}

// TestSendInputReportsLineOpen pins the reply that closes the S-6 hole: a
// bare {"success":true} cannot distinguish "typed a line" from "sent a
// command", and an agent that reads it as the latter concatenates the next
// shell_input onto the same line on the target.
func TestSendInputReportsLineOpen(t *testing.T) {
	s := newTestServer(t)
	shellID := startInteractiveShell(t, s)

	m := sendInput(t, s, shellID, "echo FIRST")
	if m["success"] != true {
		t.Errorf("success = %v, want true", m["success"])
	}
	if m["line_open"] != true {
		t.Errorf("after typing without enter, line_open = %v, want true", m["line_open"])
	}
	if got := m["bytes_written"]; got != float64(len("echo FIRST")) {
		t.Errorf("bytes_written = %v, want %d", got, len("echo FIRST"))
	}

	// A second call with no enter in between is exactly the S-6 sequence. The
	// flag cannot prevent the concatenation, but it is what lets the caller see
	// the line is still open.
	m = sendInput(t, s, shellID, "echo SECOND")
	if m["line_open"] != true {
		t.Errorf("after a second unterminated input, line_open = %v, want true", m["line_open"])
	}

	// shell_key(enter) closes the line.
	keyRes, err := s.handlePressKey(context.Background(), makeRequest(map[string]any{
		"shell_id": shellID,
		"key":      "enter",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if keyRes.IsError {
		t.Fatalf("shell_key error: %s", keyRes.Content[0].(mcpgo.TextContent).Text)
	}

	// The next shell_input opens a fresh line, so the flag must be true again
	// rather than stuck from the previous line.
	m = sendInput(t, s, shellID, "echo THIRD")
	if m["line_open"] != true {
		t.Errorf("after enter, a new input reports line_open = %v, want true", m["line_open"])
	}
}

// TestSendInputBytesWrittenCountsBytesNotRunes guards the field's unit. The
// field exists so a caller can compare it against the length of an echo it can
// see, and a count in runes would not compare against anything the device sent.
func TestSendInputBytesWrittenCountsBytesNotRunes(t *testing.T) {
	s := newTestServer(t)
	shellID := startInteractiveShell(t, s)

	// 4 runes, 8 bytes. A rune count would report 4.
	m := sendInput(t, s, shellID, "命令输入")
	if got := m["bytes_written"]; got != float64(len("命令输入")) {
		t.Errorf("bytes_written = %v, want %d bytes", got, len("命令输入"))
	}
}

// TestSendInputKeepsSuccessField pins the compatibility half of the change: the
// reply gained fields, it did not change shape. A client that only reads
// "success" must keep working.
func TestSendInputKeepsSuccessField(t *testing.T) {
	s := newTestServer(t)
	shellID := startInteractiveShell(t, s)

	m := sendInput(t, s, shellID, "echo hi")
	if _, ok := m["success"]; !ok {
		t.Error("shell_input reply dropped the success field")
	}
	if _, ok := m["line_open"]; !ok {
		t.Error("shell_input reply has no line_open field")
	}
	if _, ok := m["bytes_written"]; !ok {
		t.Error("shell_input reply has no bytes_written field")
	}
}
