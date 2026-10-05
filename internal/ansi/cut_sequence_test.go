package ansi

import (
	"strings"
	"testing"
)

// TestStrip_MarksCutSequence covers the case a read boundary creates: max_bytes
// cuts the raw bytes, and the strip that follows has nothing but a fragment of
// an escape sequence to work with.
//
// Before, the fragment was emitted as ordinary text ("006;1") and a full-screen
// redrawing bootloader produced that on nearly every page, with nothing to say
// the page was incomplete. Worse, a cut OSC was matched against the *next*
// page's terminator, which deleted the real text in between.
func TestStrip_MarksCutSequence(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		wantKept string // text that must survive the strip
		wantCut  bool   // whether the page should be marked as cut
	}{
		{name: "CSI cut in parameters", in: "a\x1b[006;1", wantKept: "a", wantCut: true},
		{name: "CSI cut early", in: "a\x1b[01", wantKept: "a", wantCut: true},
		{name: "CSI cut after bracket", in: "a\x1b[", wantKept: "a", wantCut: true},
		{name: "OSC cut in payload", in: "a\x1b]0;title-half", wantKept: "a", wantCut: true},
		{name: "OSC cut after bracket", in: "a\x1b]", wantKept: "a", wantCut: true},
		{name: "complete CSI", in: "a\x1b[006;126Hb", wantKept: "ab", wantCut: false},
		{name: "complete SGR", in: "a\x1b[31mRED\x1b[0m", wantKept: "aRED", wantCut: false},
		{name: "complete OSC with BEL", in: "a\x1b]2;title\x07rest", wantKept: "arest", wantCut: false},
		{name: "complete OSC with ST", in: "a\x1b]2;title\x1b\\rest", wantKept: "arest", wantCut: false},
		// A device may genuinely print these characters. Without an escape byte
		// there is nothing to mark, which is the whole reason the cut is
		// detected by the escape and not by the digits.
		{name: "device printed digits", in: "a006;1", wantKept: "a006;1", wantCut: false},
		{name: "plain text", in: "plain", wantKept: "plain", wantCut: false},
		{name: "empty", in: "", wantKept: "", wantCut: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Strip(tc.in)
			if !strings.Contains(got, tc.wantKept) {
				t.Errorf("Strip(%q) = %q, want it to contain %q", tc.in, got, tc.wantKept)
			}
			marked := strings.Contains(got, CutMarker)
			if marked != tc.wantCut {
				t.Errorf("Strip(%q) marker present = %v, want %v (got %q)", tc.in, marked, tc.wantCut, got)
			}
		})
	}
}

// TestStrip_KeepsBareEscape pins the one case that must NOT be marked. A lone
// ESC at the end of a page is a real 2-byte sequence whose second half has not
// been read yet; Strip has always passed it through, and calling it a cut would
// put a marker on ordinary output.
func TestStrip_KeepsBareEscape(t *testing.T) {
	got := Strip("a\x1b")
	if got != "a\x1b" {
		t.Errorf("Strip(%q) = %q, want %q unchanged", "a\x1b", got, "a\x1b")
	}
	if strings.Contains(got, CutMarker) {
		t.Error("a bare ESC must not be reported as a cut sequence")
	}
}

// TestStrip_CutKeepsTheFragment checks the fragment is preserved rather than
// dropped. It is real output that only looks like noise because this page
// stopped early, and the buffer still holds it: discarding it would lose bytes
// the caller can go and re-read.
func TestStrip_CutKeepsTheFragment(t *testing.T) {
	got := Strip("a\x1b[006;1")
	if !strings.Contains(got, "006;1") {
		t.Errorf("Strip dropped the cut fragment: %q", got)
	}
	if !strings.HasSuffix(got, CutMarker) {
		t.Errorf("Strip(%q) = %q, want the marker last", "a\x1b[006;1", got)
	}
}

// TestSplitIncompleteTail pins the split itself, which is what tells a caller
// whether to re-read from start_offset.
func TestSplitIncompleteTail(t *testing.T) {
	tests := []struct {
		in           string
		wantComplete string
		wantCut      string
	}{
		{in: "a\x1b[006;1", wantComplete: "a", wantCut: "\x1b[006;1"},
		{in: "a\x1b[006;126Hb", wantComplete: "a\x1b[006;126Hb", wantCut: ""},
		{in: "a\x1b", wantComplete: "a\x1b", wantCut: ""},
		{in: "no escapes", wantComplete: "no escapes", wantCut: ""},
		{in: "\x1b[31mred", wantComplete: "\x1b[31mred", wantCut: ""},
	}
	for _, tc := range tests {
		complete, cut := SplitIncompleteTail(tc.in)
		if complete != tc.wantComplete || cut != tc.wantCut {
			t.Errorf("SplitIncompleteTail(%q) = (%q, %q), want (%q, %q)",
				tc.in, complete, cut, tc.wantComplete, tc.wantCut)
		}
	}
}

// TestStrip_CutMarkerSurvivesCompact is the reason CutMarker is plain text. The
// pipeline is Compact(Strip(...)), and Compact drops every control byte below
// 0x20 except CR, LF and TAB — so a marker written as an escape sequence would
// arrive headless. This test fails if anyone re-encodes it.
func TestStrip_CutMarkerSurvivesCompact(t *testing.T) {
	got := Compact(Strip("a\x1b[006;1"))
	if !strings.Contains(got, CutMarker) {
		t.Errorf("Compact(Strip(cut)) = %q, want it to still contain %q", got, CutMarker)
	}
}

// TestStrip_CutKeepsTextAcrossPages documents what a cut actually does to the
// following page, because the effect is the opposite of the one it looks like.
//
// The live reader strips each page independently, and the cut fragment's ESC is
// consumed on the page it appears on: the leftover "006;12" is plain digits by
// then, so the next page's real text is NOT swallowed. What does happen is
// debris: the fragment's characters are mixed into the visible output as if the
// device had typed them, and a cursor-addressing page ("006;126H") turns a
// prompt into "6Hgrub> " — which is how a bootloader prompt stops looking like
// a prompt.
//
// The fix is the marker, not the retention: it tells the reader this page ended
// mid-sequence so it can resume from start_offset and see the sequence whole.
func TestStrip_CutKeepsTextAcrossPages(t *testing.T) {
	page1 := "boot: \x1b[006;12"
	page2 := "6Hgrub> echo MARK1"

	_, cut := SplitIncompleteTail(page1)
	if cut == "" {
		t.Fatal("page 1 should have been detected as cut")
	}

	got1 := Strip(page1)
	if !strings.Contains(got1, "boot:") {
		t.Errorf("page 1 lost its text: %q", got1)
	}
	if !strings.Contains(got1, CutMarker) {
		t.Errorf("page 1 is not marked as cut: %q", got1)
	}
	// The debris is still there, which is the thing the marker explains.
	if !strings.Contains(got1, "006;12") {
		t.Errorf("page 1 should retain the fragment: %q", got1)
	}

	got2 := Strip(page2)
	if !strings.Contains(got2, "grub>") {
		t.Errorf("page 2 lost real text: %q", got2)
	}
	// The "6H" is the tail of the address the cut sequence was carrying. It is
	// debris from the previous page, not device output, and the marker is what
	// lets a reader recognise that.
	if !strings.Contains(got2, "6H") {
		t.Errorf("page 2 debris changed unexpectedly: %q", got2)
	}
}
