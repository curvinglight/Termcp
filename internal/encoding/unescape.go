package encoding

import (
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// UnescapeJSON turns the \uXXXX escapes a JSON producer left in a string into the
// UTF-8 the escapes stand for.
//
// WHY THIS IS NEEDED AT ALL: the escapes are not termcp's. Chrome's DevTools
// protocol serialises every non-ASCII character that way, so a page title in
// Chinese arrives as `\u4e2d\u6587` — twelve bytes of escape for two characters,
// and unreadable without decoding them first. Passing the frame through verbatim
// is the honest thing for a byte channel to do, and it is exactly what made the
// browser door worse than a client that parses JSON and re-serialises it: the
// model paid double the tokens to read something it then had to decode in its
// head.
//
// ONLY \u ESCAPES ARE TOUCHED. Every other escape (\n, \t, \", \\) stays as it is,
// because the caller is reading a frame, not a string, and rewriting its
// whitespace would change what the peer said. A \\uXXXX (an escaped backslash
// followed by the letters uXXXX) is content, not an escape, and is left alone.
//
// Surrogate pairs are combined. A lone surrogate cannot be represented in UTF-8,
// so it is left as the escape it was rather than replaced with U+FFFD, which would
// silently change the peer's frame.
func UnescapeJSON(s string) string {
	if !strings.Contains(s, `\u`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		c := s[i]
		if c != '\\' {
			b.WriteByte(c)
			i++
			continue
		}
		if i+1 >= len(s) {
			b.WriteByte(c)
			i++
			continue
		}
		// A doubled backslash is one literal backslash: whatever follows it is
		// content, so the `\u` inside `\\u4e2d` must not be decoded.
		if s[i+1] == '\\' {
			b.WriteString(`\\`)
			i += 2
			continue
		}
		r, size, ok := decodeUEscape(s[i:])
		if !ok {
			b.WriteByte(c)
			i++
			continue
		}
		b.WriteRune(r)
		i += size
	}
	return b.String()
}

// decodeUEscape decodes the escape at the start of s, which must begin with \u.
// size is how many bytes of s the escape occupied: 6, or 12 for a surrogate pair.
func decodeUEscape(s string) (rune, int, bool) {
	if len(s) < 6 || s[0] != '\\' || s[1] != 'u' {
		return 0, 0, false
	}
	v, ok := hex4(s[2:6])
	if !ok {
		return 0, 0, false
	}
	if !utf16.IsSurrogate(rune(v)) {
		if !utf8.ValidRune(rune(v)) {
			return 0, 0, false
		}
		return rune(v), 6, true
	}
	// A high surrogate is only half a character; the low half follows as its own
	// escape. A low surrogate with no high one, or a high one with no low, is not
	// decodable and stays as written.
	if v < 0xD800 || v > 0xDBFF || len(s) < 12 || s[6] != '\\' || s[7] != 'u' {
		return 0, 0, false
	}
	lo, ok := hex4(s[8:12])
	if !ok || lo < 0xDC00 || lo > 0xDFFF {
		return 0, 0, false
	}
	return utf16.DecodeRune(rune(v), rune(lo)), 12, true
}

func hex4(s string) (uint32, bool) {
	if len(s) < 4 {
		return 0, false
	}
	var v uint32
	for i := 0; i < 4; i++ {
		c := s[i]
		var d uint32
		switch {
		case c >= '0' && c <= '9':
			d = uint32(c - '0')
		case c >= 'a' && c <= 'f':
			d = uint32(c-'a') + 10
		case c >= 'A' && c <= 'F':
			d = uint32(c-'A') + 10
		default:
			return 0, false
		}
		v = v<<4 | d
	}
	return v, true
}
