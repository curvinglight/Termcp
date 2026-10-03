package encoding

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// The presentation rules of design §3.2 in one table: what each profile value
// does to the same handful of payloads, including the case the whole feature
// exists for — bytes that are not valid UTF-8 reaching the client as \xHH
// rather than as the U+FFFD the JSON layer would silently substitute.
func TestEncodeForDisplay(t *testing.T) {
	valid := []byte("temp=25\r\n")          // plain ASCII, valid UTF-8
	multibyte := []byte("温度: 25\r\n")       // valid UTF-8, non-ASCII
	binary := []byte{0x00, 0xff, 0x1b, '['} // never valid UTF-8
	truncated := []byte("ok\xff tail")      // valid text with one bad byte

	tests := []struct {
		name string
		enc  string
		in   []byte
		want string
	}{
		{"auto passes ascii through", Auto, valid, "temp=25\r\n"},
		{"auto passes valid utf8 through", Auto, multibyte, "温度: 25\r\n"},
		{"auto passes empty through", Auto, nil, ""},
		// The reason auto exists: an unescaped control byte would be mangled by
		// JSON, and a replacement character hides what the device actually sent.
		{"auto escapes binary", Auto, binary, "\\x00\\xff\\x1b["},
		{"auto escapes a single invalid byte", Auto, truncated, "ok\\xff tail"},
		// auto keys on UTF-8 validity, not on whether the bytes look like text:
		// a NUL or a raw newline is valid UTF-8 and passes through. Only a byte
		// no UTF-8 decoder accepts triggers the escape, and then the escape
		// keeps the line structure readable — a newline byte inside an escaped
		// payload would be indistinguishable from the payload's own.
		{"auto passes valid control bytes", Auto, []byte{'a', '\n', 0x00}, "a\n\x00"},
		{"auto escape names the newline", Auto, []byte{'a', '\n', 0xff}, "a\\n\\xff"},

		// text is the profile asserting the device speaks text: bytes out as
		// they are, validity not its business.
		{"text passes ascii", Text, valid, "temp=25\r\n"},
		{"text passes invalid utf8 unchanged", Text, binary, string(binary)},

		{"hex", Hex, binary, "00ff1b5b"},
		{"hex is lowercase", Hex, []byte{0xAB, 0xCD}, "abcd"},
		{"b64 is standard with padding", B64, binary, "AP8bWw=="},
		{"b64 of ascii", B64, []byte("foobar"), "Zm9vYmFy"},

		// Empty and unknown are the same case by design: an encoding is
		// validated when the profile is saved, so anything unexpected is a
		// record from another version, and the safe answer is the behaviour
		// every session had before this function existed.
		{"empty behaves as text", "", binary, string(binary)},
		{"unknown behaves as text", "utf16", binary, string(binary)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := EncodeForDisplay(tc.in, tc.enc)
			if got != tc.want {
				t.Errorf("EncodeForDisplay(%v, %q) = %q, want %q", tc.in, tc.enc, got, tc.want)
			}
		})
	}
}

// The encodings that promise the client receives the bytes the device sent —
// auto, hex and b64 — must survive a JSON round trip unchanged, for every
// payload. That is what escaping exists for: design §9.4 checks it by hand
// against a real misconfigured baud rate, and this is the same statement
// without the hardware.
func TestNonLossyEncodingsSurviveJSONRoundTrip(t *testing.T) {
	payloads := [][]byte{
		nil,
		[]byte("hello\r\n"),
		[]byte{0x00, 0xff, 0xfe, 0xc3, 0x28}, // invalid UTF-8, including a truncated rune
		{0x1b, '[', '3', '1', 'm'},
	}
	for _, enc := range []string{Auto, Hex, B64} {
		for _, p := range payloads {
			out := EncodeForDisplay(p, enc)
			if !utf8.ValidString(out) {
				t.Fatalf("enc %q produced invalid UTF-8 for %v: %q", enc, p, out)
			}
			assertJSONRoundTrip(t, enc, out)
			// The substitution the escaping exists to avoid.
			if strings.Contains(out, "�") {
				t.Errorf("enc %q replaced bytes with U+FFFD: %q", enc, out)
			}
		}
	}
}

// text — and the empty and unknown values, which behave as text — is the
// profile's own assertion that its device speaks text: bytes go out as they
// are. Valid text must pass through a JSON round trip exactly; that is the
// contract, and it is what every SSH session has always relied on.
//
// What it deliberately does NOT promise: a payload that is not valid UTF-8 is
// handed to the JSON layer, which substitutes U+FFFD for what it cannot
// decode. That is the assertion being wrong rather than the reader — the
// reason auto is the default and escapes instead.
func TestTextPassesValidUTF8ThroughJSONUnchanged(t *testing.T) {
	payloads := [][]byte{
		nil,
		[]byte("hello\r\n"),
		[]byte("温度: 25\r\n"),
		{0x1b, '[', '3', '1', 'm'},
	}
	for _, enc := range []string{Text, "", "bogus"} {
		for _, p := range payloads {
			out := EncodeForDisplay(p, enc)
			if out != string(p) {
				t.Errorf("enc %q altered valid text: got %q, want %q", enc, out, string(p))
				continue
			}
			assertJSONRoundTrip(t, enc, out)
		}
	}
}

func assertJSONRoundTrip(t *testing.T, enc, out string) {
	t.Helper()
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("enc %q json.Marshal(%q): %v", enc, out, err)
	}
	var back string
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("enc %q json.Unmarshal: %v", enc, err)
	}
	if back != out {
		t.Errorf("enc %q round trip changed the value: sent %q, got %q", enc, out, back)
	}
}

// hex and b64 leave no ESC byte in the result, which is what lets the outlets
// encode before stripping ANSI: the strip finds nothing to do and the caller
// keeps the bytes it asked for.
func TestHexAndBase64HideTheEscapeByte(t *testing.T) {
	esc := []byte{0x1b, '[', '2', 'J'}
	for _, enc := range []string{Hex, B64} {
		out := EncodeForDisplay(esc, enc)
		if strings.ContainsRune(out, 0x1b) {
			t.Errorf("enc %q left a raw ESC byte in %q", enc, out)
		}
	}
}

// EncodeText is what auto falls back to; assert the two stay in step, since a
// change to one silently changes what a binary device's output looks like.
func TestAutoFallbackUsesEncodeText(t *testing.T) {
	// Invalid UTF-8, so auto takes the escape path at all: every byte of a
	// payload of valid UTF-8 passes through untouched instead.
	in := []byte{0x41, 0x0a, 0xff, 0x43}
	if got, want := EncodeForDisplay(in, Auto), EncodeText(in); got != want {
		t.Errorf("auto fallback = %q, EncodeText = %q", got, want)
	}
}
