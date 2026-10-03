package encoding

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode/utf8"
)

// The display encodings a profile may name. They are declared here rather than
// beside the profile schema because they are a display-layer vocabulary, not a
// storage one: internal/serial aliases them for its `encoding` TOML key so the
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
// scrollback rail — because the point of an encoding field is that the same
// bytes look the same wherever they surface. Converting in each outlet
// separately is how one copy ends up hexed, another replacement-chars, and a
// third raw.
//
// This is the presentation step and the last one: storage keeps raw bytes and
// the internal read path hands them over untouched (see design §3.2).
//
// It runs before ANSI stripping rather than after, which is what makes the two
// independent options compose:
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
//	                the JSON layer (design §9.4)
//	text            bytes as a string, valid UTF-8 or not — the profile
//	                asserting the device speaks text. A payload that is
//	                therefore not valid UTF-8 reaches the JSON layer as it
//	                stands and is substituted there; that is the assertion
//	                being wrong rather than the reader, and the reason auto
//	                is the default and escapes instead
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

// EncodeText encodes a byte slice as text with \xHH escapes for non-printable bytes.
func EncodeText(data []byte) string {
	var b strings.Builder
	for _, c := range data {
		switch {
		case c >= 0x20 && c <= 0x7E:
			b.WriteByte(c)
		case c == '\n':
			b.WriteString("\\n")
		case c == '\r':
			b.WriteString("\\r")
		case c == '\t':
			b.WriteString("\\t")
		default:
			fmt.Fprintf(&b, "\\x%02x", c)
		}
	}
	return b.String()
}

// DecodeText decodes a text string with \xHH, \n, \r, \t escapes back to bytes.
func DecodeText(s string) []byte {
	var result []byte
	i := 0
	for i < len(s) {
		if s[i] == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case 'n':
				result = append(result, '\n')
				i += 2
				continue
			case 'r':
				result = append(result, '\r')
				i += 2
				continue
			case 't':
				result = append(result, '\t')
				i += 2
				continue
			case 'x':
				if i+3 < len(s) {
					var b byte
					fmt.Sscanf(s[i+2:i+4], "%02x", &b)
					result = append(result, b)
					i += 4
					continue
				}
			case '\\':
				result = append(result, '\\')
				i += 2
				continue
			}
		}
		result = append(result, s[i])
		i++
	}
	return result
}

// HexDecode decodes a hex string to bytes. Returns an error for malformed hex.
func HexDecode(s string) ([]byte, error) {
	if len(s)%2 != 0 {
		return nil, fmt.Errorf("hex string has odd length %d", len(s))
	}
	result := make([]byte, 0, len(s)/2)
	for i := 0; i < len(s); i += 2 {
		var b byte
		if _, err := fmt.Sscanf(s[i:i+2], "%02x", &b); err != nil {
			return nil, fmt.Errorf("invalid hex at position %d: %w", i, err)
		}
		result = append(result, b)
	}
	return result, nil
}
