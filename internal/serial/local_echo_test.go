package serial

import (
	"testing"
	"time"
)

// newEchoChannel builds a channel with local echo on and no device traffic, so
// every byte the transcript produces is an echo of what the test typed.
func newEchoChannel() (*Channel, *fakePort) {
	fake := newFakePort()
	c := &Channel{
		handle:      fake,
		spec:        &Spec{LocalEcho: true},
		stdout:      newPipeReader(),
		done:        make(chan struct{}),
		readersDone: make(chan struct{}),
	}
	return c, fake
}

// drainEcho reads exactly n bytes from the transcript. The transcript is
// append-only with no length marker, so a test that knows what it asked for
// reads that much and fails if fewer arrive — a silent stall would otherwise
// look like a pass.
func drainEcho(t *testing.T, c *Channel, n int) string {
	t.Helper()
	buf := make([]byte, 0, n)
	tmp := make([]byte, 512)
	for len(buf) < n {
		got := make(chan int, 1)
		go func() {
			k, _ := c.StdoutReader().Read(tmp)
			got <- k
		}()
		select {
		case k := <-got:
			if k == 0 {
				t.Fatalf("transcript ended after %d of %d bytes: %q", len(buf), n, buf)
			}
			buf = append(buf, tmp[:k]...)
		case <-time.After(time.Second):
			t.Fatalf("transcript stalled at %d of %d bytes: %q", len(buf), n, buf)
		}
	}
	return string(buf)
}

// write sends input through the real WriteStdin path.
func write(t *testing.T, c *Channel, in string) {
	t.Helper()
	if _, err := c.WriteStdin([]byte(in)); err != nil {
		t.Fatalf("WriteStdin(%q): %v", in, err)
	}
}

func TestLocalEchoBackspaceErases(t *testing.T) {
	c, fake := newEchoChannel()
	write(t, c, "AT\x7f")

	// The terminal rubs out one cell the way every terminal does: back, blank,
	// back. "T" is gone from the screen; "A" is not.
	want := "AT\b \b"
	if got := drainEcho(t, c, len(want)); got != want {
		t.Errorf("transcript = %q, want %q", got, want)
	}
	// The device still receives the real DEL — this is a display rewrite, not a
	// protocol one, and a device that acts on backspace must see it.
	if got := string(fake.written()); got != "AT\x7f" {
		t.Errorf("device received %q, want %q", got, "AT\x7f")
	}
}

func TestLocalEchoCtrlHIsBackspace(t *testing.T) {
	// 0x08 is the other code for backspace; a terminal may send either.
	c, _ := newEchoChannel()
	write(t, c, "AT\x08")

	want := "AT\b \b"
	if got := drainEcho(t, c, len(want)); got != want {
		t.Errorf("transcript = %q, want %q", got, want)
	}
}

func TestLocalEchoEnterEndsTheLine(t *testing.T) {
	c, fake := newEchoChannel()
	write(t, c, "AT\r\x7f")

	// Enter was echoed and the remembered line dropped with it, so the
	// backspace that follows has nothing left to erase — it belongs to the
	// device's reply, not to "AT".
	want := "AT\r"
	if got := drainEcho(t, c, len(want)); got != want {
		t.Errorf("transcript = %q, want %q", got, want)
	}
	if got := string(fake.written()); got != "AT\r\x7f" {
		t.Errorf("device received %q, want %q", got, "AT\r\x7f")
	}
}

func TestLocalEchoCtrlUKillsTheLine(t *testing.T) {
	c, fake := newEchoChannel()
	write(t, c, "AT+CSQ\x15")

	erase := "\b\b\b\b\b\b      \b\b\b\b\b\b"
	want := "AT+CSQ" + erase
	if got := drainEcho(t, c, len(want)); got != want {
		t.Errorf("transcript = %q, want %q", got, want)
	}
	if got := string(fake.written()); got != "AT+CSQ\x15" {
		t.Errorf("device received %q, want %q", got, "AT+CSQ\x15")
	}
}

func TestLocalEchoCtrlWKillsOneWord(t *testing.T) {
	c, _ := newEchoChannel()
	write(t, c, "AT TEST\x17")

	// Four cells: "TEST" goes, the space before it stays — ctrl+w after a space
	// must not take the previous word with it.
	want := "AT TEST" + "\b\b\b\b    \b\b\b\b"
	got := drainEcho(t, c, len(want))
	if got != want {
		t.Fatalf("after first ctrl+w: transcript = %q, want %q", got, want)
	}

	// The line is now "AT "; the next one strips the trailing space and then
	// the word, which is what a shell does with the same key.
	write(t, c, "\x17")
	want2 := "\b\b\b   \b\b\b"
	if got2 := drainEcho(t, c, len(want2)); got2 != want2 {
		t.Errorf("after second ctrl+w: transcript = %q, want %q", got2, want2)
	}
}

func TestLocalEchoWideRuneErasesTwoCells(t *testing.T) {
	c, fake := newEchoChannel()
	write(t, c, "中\x7f")

	// 中 occupies two cells, so erasing it has to walk back over both — one
	// backspace would leave half the glyph on screen.
	want := "中" + "\b\b  \b\b"
	if got := drainEcho(t, c, len(want)); got != want {
		t.Errorf("transcript = %q, want %q", got, want)
	}
	if got := string(fake.written()); got != "中\x7f" {
		t.Errorf("device received %q, want %q", got, "中\x7f")
	}
}

func TestLocalEchoOvertypeAfterEditing(t *testing.T) {
	c, _ := newEchoChannel()
	// Erasing then retyping must not accumulate: the line state, not the raw
	// byte count, is what the next erase works from.
	write(t, c, "AT\x7fX\x7f")

	want := "AT\b \b" + "X\b \b"
	if got := drainEcho(t, c, len(want)); got != want {
		t.Errorf("transcript = %q, want %q", got, want)
	}
}

// TestRuneWidth pins the widths the erase sequences are counted in. A wrong
// answer here is a half-erased glyph on screen, which is the defect this whole
// file exists to prevent.
func TestRuneWidth(t *testing.T) {
	for _, tc := range []struct {
		r    rune
		want int
	}{
		{'A', 1},
		{' ', 1},
		{'é', 1},
		{'中', 2},    // CJK ideograph
		{'한', 2},    // Hangul syllable
		{'ａ', 2},    // fullwidth latin small letter a
		{'　', 2},    // ideographic space
		{'\x1b', 1}, // ESC draws no cell of its own
		{'😀', 2},    // wide emoji
	} {
		if got := runeWidth(tc.r); got != tc.want {
			t.Errorf("runeWidth(%q) = %d, want %d", tc.r, got, tc.want)
		}
	}
}

// TestLocalEchoControlsDoNotShiftTheLine keeps a control that draws nothing out
// of the line state: counting it would make a later backspace erase a cell that
// was never drawn.
func TestLocalEchoControlsDoNotShiftTheLine(t *testing.T) {
	c, _ := newEchoChannel()
	write(t, c, "\x1b[A\x7f") // an arrow key, then backspace over "A"

	want := "\x1b[A\b \b"
	if got := drainEcho(t, c, len(want)); got != want {
		t.Errorf("transcript = %q, want %q", got, want)
	}
}
