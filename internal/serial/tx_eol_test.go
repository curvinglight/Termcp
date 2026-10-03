package serial

import (
	"bytes"
	"testing"
)

func TestEnterBytesForEachSetting(t *testing.T) {
	cases := map[string]string{
		TxEOLCR:   "\r",
		TxEOLCRLF: "\r\n",
		TxEOLLF:   "\n",
		// "none" sends a bare CR. The name reads like "no line ending" but the
		// meaning is "no conversion" — the byte the terminal already sends.
		TxEOLNone: "\r",
	}
	for setting, want := range cases {
		if got := string(enterBytes(setting)); got != want {
			t.Errorf("enterBytes(%q) = %q, want %q", setting, got, want)
		}
	}
}

func TestEnterBytesUnknownFallsBackToCR(t *testing.T) {
	// Validation rejects unknown values, so this is a safety net rather than a
	// documented behaviour.
	if got := string(enterBytes("bogus")); got != "\r" {
		t.Fatalf("enterBytes(bogus) = %q, want %q", got, "\r")
	}
}

func TestReplaceEnterBareLF(t *testing.T) {
	// A serial ChildShell keeps enterCRLF false, so a bare Enter arrives as LF.
	// Every setting replaces it.
	cases := map[string]string{
		TxEOLCR:   "\r",
		TxEOLCRLF: "\r\n",
		TxEOLLF:   "\n",
		TxEOLNone: "\r",
	}
	for setting, want := range cases {
		got := ReplaceEnter([]byte{'\n'}, setting)
		if string(got) != want {
			t.Errorf("tx_eol=%q: got %q, want %q", setting, got, want)
		}
	}
}

func TestReplaceEnterCRLFInput(t *testing.T) {
	// Defensive: if a CRLF ever reaches here, the whole ending is replaced, not
	// just the LF, or the CR would be left behind.
	cases := map[string]string{
		TxEOLCR:   "\r",
		TxEOLCRLF: "\r\n",
		TxEOLLF:   "\n",
		TxEOLNone: "\r",
	}
	for setting, want := range cases {
		got := ReplaceEnter([]byte{'\r', '\n'}, setting)
		if string(got) != want {
			t.Errorf("tx_eol=%q: got %q, want %q", setting, got, want)
		}
	}
}

func TestReplaceEnterKeepsPrecedingText(t *testing.T) {
	got := ReplaceEnter([]byte("AT\r\n"), TxEOLLF)
	if string(got) != "AT\n" {
		t.Fatalf("got %q, want %q", got, "AT\n")
	}
}

func TestReplaceEnterDoesNotTouchInnerNewlines(t *testing.T) {
	// shell_input text is not line-ending converted: only the Enter that ends
	// the call is. A pasted multi-line block keeps the newlines it was typed
	// with, and only the final one is replaced.
	got := ReplaceEnter([]byte("a\nb\r\n"), TxEOLCR)
	if string(got) != "a\nb\r" {
		t.Fatalf("got %q, want %q", got, "a\nb\r")
	}
}

func TestReplaceEnterLeavesPlainTextAlone(t *testing.T) {
	// shell_input without press_enter sends text with no line ending at all.
	for _, setting := range []string{TxEOLCR, TxEOLCRLF, TxEOLLF, TxEOLNone} {
		got := ReplaceEnter([]byte("AT"), setting)
		if string(got) != "AT" {
			t.Errorf("tx_eol=%q: got %q, want %q", setting, got, "AT")
		}
	}
}

func TestReplaceEnterEmptyInputProducesLineEnding(t *testing.T) {
	// A bare Enter with no text is a line ending on its own.
	for _, setting := range []string{TxEOLCR, TxEOLCRLF, TxEOLLF, TxEOLNone} {
		got := ReplaceEnter(nil, setting)
		if len(got) == 0 {
			t.Errorf("tx_eol=%q: an empty input with Enter must still send a line ending", setting)
		}
	}
}

func TestReplaceEnterDoesNotAliasInputBuffer(t *testing.T) {
	// The caller reuses its buffer; the result must not point into it.
	input := []byte("AT\r\n")
	got := ReplaceEnter(input, TxEOLLF)

	input[0] = 'X'
	if bytes.HasPrefix(got, []byte("X")) {
		t.Fatal("the returned slice aliases the input buffer")
	}
}

func TestBothEnterPathsAgree(t *testing.T) {
	// The design makes this a hard requirement: shell_input(press_enter) and
	// shell_key(enter) both produce an Enter, and both must send the same byte.
	//
	// Both paths reach replaceEnter after the session has already appended its
	// own ending (appendEnter with enterCRLF=false produces a bare LF, as does
	// KeyBytes for Enter), so the input here carries that LF. A serial profile
	// never keeps enterCRLF true, so LF is the only ending that can arrive.
	for _, setting := range []string{TxEOLCR, TxEOLCRLF, TxEOLLF, TxEOLNone} {
		fromAppendEnter := []byte("cmd\n") // shell_input(press_enter=true)
		fromKeyBytes := []byte("cmd\n")    // shell_key("enter")

		gotInput := ReplaceEnter(fromAppendEnter, setting)
		gotKey := ReplaceEnter(fromKeyBytes, setting)
		if !bytes.Equal(gotInput, gotKey) {
			t.Errorf("tx_eol=%q: the two Enter paths disagree: %q vs %q",
				setting, gotInput, gotKey)
		}

		want := append([]byte("cmd"), enterBytes(setting)...)
		if !bytes.Equal(gotInput, want) {
			t.Errorf("tx_eol=%q: got %q, want %q", setting, gotInput, want)
		}
	}
}
