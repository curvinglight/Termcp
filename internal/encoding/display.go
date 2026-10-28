package encoding

import (
	"encoding/base64"
	"encoding/hex"
	"unicode/utf8"
)

// The display encodings a profile may name. They are declared here rather than
// beside the profile schema because they are a display-layer vocabulary, not a
// storage one: a serial profile aliases them for its `display` TOML key so the
// two cannot drift apart, and the value a profile stores is exactly the value
// EncodeForDisplay switches on.
const (
	// Auto passes valid UTF-8 through and escapes the rest.
	Auto = "auto"
	// Text presents bytes as a string whatever their validity.
	Text = "text"
	// Hex presents bytes as lowercase hex.
	Hex = "hex"
	// B64 presents bytes as standard base64 with padding.
	B64 = "b64"
)

// EncodeForDisplay renders data for display under enc, the encoding a profile
// selected. Every output outlet goes through this one function — MCP
// shell_output, the Web UI terminal stream, the history range endpoints and the
// scrollback rail — because the point of a display field is that the same bytes
// look the same wherever they surface. Converting in each outlet separately is
// how one copy ends up hexed, another replacement-chars, and a third raw.
//
// It is the presentation step and the last one: storage keeps raw bytes and the
// internal read path hands them over untouched.
//
// It runs AFTER any charset decode and composes with ANSI stripping:
//
//   - hex and b64 produce ASCII with no escape byte left in the string, so a
//     strip is a no-op and the caller sees the bytes it asked for — asking for
//     hex and silently deleting the escape sequences from the middle would be
//     neither;
//   - auto/text leave the terminal's own bytes, so stripping behaves exactly as
//     it did before this function existed.
//
// Values:
//
//	auto            valid UTF-8 passes through; anything else is escaped by
//	                EncodeText, so a misconfigured baud rate reaches the client
//	                as \xHH rather than being silently rewritten to U+FFFD by
//	                the JSON layer
//	text            bytes as a string, valid UTF-8 or not — the profile
//	                asserting the device speaks text
//	hex             lowercase hex
//	b64             standard base64 with padding
//	"" or unknown   treated as text
//
// The empty and unknown cases are the same case: an encoding is validated when
// the profile is saved, so an unexpected value can only be a record written by
// another version or a session built by a test, and the safe answer there is
// the behaviour that existed before this function did — bytes out, unaltered.
func EncodeForDisplay(data []byte, enc string) string {
	switch enc {
	case Auto:
		if utf8.Valid(data) {
			return string(data)
		}
		return EncodeText(data)
	case Hex:
		return hex.EncodeToString(data)
	case B64:
		return base64.StdEncoding.EncodeToString(data)
	case Text, "":
		return string(data)
	default:
		return string(data)
	}
}

// DisplayValid reports whether name is a display encoding a profile may store.
// The empty value is valid: it means "not set", which every reader normalises
// to Text.
func DisplayValid(name string) bool {
	switch name {
	case "", Auto, Text, Hex, B64:
		return true
	}
	return false
}
