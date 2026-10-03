package session

import (
	"bytes"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/serial"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// serialTestShell builds a bare running serial shell over a fake channel.
//
// A serial ChildShell is reachable without a Session behind it (logInput is
// nil-safe when parent is nil), so the fixture is the shell alone: ID, the
// channel to write into, running status, and the serial flags that select the
// tx_eol path.
func serialTestShell(fc *fakeChannel, txEOL string, mode api.SessionMode) *ChildShell {
	return &ChildShell{
		ID:          "serial-1",
		execSession: fc,
		Status:      api.SessionRunning,
		serial:      true,
		txEOL:       txEOL,
		enterCRLF:   false, // always false for serial: one conversion, decided by tx_eol
		mode:        mode,
		done:        make(chan struct{}),
	}
}

// TestSerialBothEnterPathsAgree is the acceptance test for design §9.1: the two
// ways a serial shell receives Enter — shell_input with press_enter and
// shell_key("enter") — must put the same bytes on the wire for every tx_eol
// setting, because the profile promises one line ending and an agent picks its
// input path freely.
//
// The equality is byte-exact, which is also what proves the conversion ran
// once: "none" must produce a bare CR (the existing terminal behaviour), not a
// CR that passed through untouched while the other paths converted.
//
// The pty-mode row exists because KeyBytes returns a bare CR when the shell
// carries pty mode while appendEnter always sends LF — if a pty-mode shell ever
// reaches a serial session (entry points refuse mode, but a shell constructed
// elsewhere could carry one), both inputs must still be converted rather than
// let crlf/lf degrade to a bare CR on one path only.
func TestSerialBothEnterPathsAgree(t *testing.T) {
	cases := []struct {
		name  string
		txEOL string
		want  string
		mode  api.SessionMode
	}{
		{name: "cr", txEOL: serial.TxEOLCR, want: "\r"},
		{name: "crlf", txEOL: serial.TxEOLCRLF, want: "\r\n"},
		{name: "lf", txEOL: serial.TxEOLLF, want: "\n"},
		// "none" is not "send nothing": it is the bare CR a terminal already
		// sends, and the test pins it so the name cannot drift into meaning.
		{name: "none", txEOL: serial.TxEOLNone, want: "\r"},
		{name: "crlf in pty mode", txEOL: serial.TxEOLCRLF, want: "\r\n", mode: api.ModePTY},
		{name: "lf in pty mode", txEOL: serial.TxEOLLF, want: "\n", mode: api.ModePTY},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Path 1: shell_input with press_enter.
			fc1 := newFakeChannel()
			cs1 := serialTestShell(fc1, tc.txEOL, tc.mode)
			if err := cs1.SendTerminalBytesFrom([]byte("cmd"), true, InputFromAI); err != nil {
				t.Fatalf("SendTerminalBytesFrom: %v", err)
			}
			want1 := append([]byte("cmd"), tc.want...)
			if !bytes.Equal(fc1.written, want1) {
				t.Errorf("shell_input wrote %q, want %q", fc1.written, want1)
			}

			// Path 2: shell_key("enter"), same setting, fresh shell so the
			// assertion is on this write alone.
			fc2 := newFakeChannel()
			cs2 := serialTestShell(fc2, tc.txEOL, tc.mode)
			if err := cs2.PressKeyFrom("enter", 1, InputFromAI); err != nil {
				t.Fatalf("PressKeyFrom: %v", err)
			}
			want2 := []byte(tc.want)
			if !bytes.Equal(fc2.written, want2) {
				t.Errorf("shell_key wrote %q, want %q", fc2.written, want2)
			}
		})
	}
}

// TestSerialTextWithoutEnterIsUntouched pins the other half of applyTxEOL:
// text the caller did not end with Enter is sent exactly as given. A conversion
// that ran on plain text would rewrite a byte stream the agent deliberately
// shaped (a partial line, a paste whose endings it chose).
func TestSerialTextWithoutEnterIsUntouched(t *testing.T) {
	fc := newFakeChannel()
	cs := serialTestShell(fc, serial.TxEOLCRLF, "")
	raw := []byte("partial line\ninterior")
	if err := cs.SendTerminalBytesFrom(raw, false, InputFromAI); err != nil {
		t.Fatalf("SendTerminalBytesFrom: %v", err)
	}
	if !bytes.Equal(fc.written, raw) {
		t.Errorf("text without Enter wrote %q, want %q", fc.written, raw)
	}
}
