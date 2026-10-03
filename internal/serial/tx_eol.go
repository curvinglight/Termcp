package serial

// enterBytes returns the bytes for one Enter keypress.
//
// This is the single place the Enter byte is decided for a serial session. There
// are two ways a serial shell receives an Enter — shell_input with press_enter,
// and shell_key("enter") — and they must agree, so both go through here rather
// than each doing its own substitution.
//
// tx_eol = "none" means "do not convert": the Enter byte is the bare CR a
// terminal already sends. It does NOT mean "send no line ending at all", and the
// name is documented that way wherever a user can see it.
func enterBytes(txEOL string) []byte {
	switch txEOL {
	case TxEOLCRLF:
		return []byte{'\r', '\n'}
	case TxEOLLF:
		return []byte{'\n'}
	case TxEOLCR, TxEOLNone:
		// Both send CR. "none" is the existing terminal behaviour, "cr" is the
		// documented serial default; they are the same byte.
		return []byte{'\r'}
	default:
		// Validation rejects anything else, so this is the safe fallback rather
		// than a silent surprise.
		return []byte{'\r'}
	}
}

// ReplaceEnter swaps the line ending produced by the session layer for the one
// this profile wants.
//
// The session's appendEnter/KeyBytes turn Enter into "\n" or "\r\n" depending on
// the target shell family — a distinction that has no meaning for a serial
// device, which is neither unix nor Windows. Rather than let that inference run
// and then correct it (which would apply the conversion twice and get tx_eol =
// "none" wrong), the serial path discards whatever was produced and substitutes
// its own ending. A serial ChildShell keeps enterCRLF false so the input here is
// always a bare LF.
//
// It is exported because the session package owns the write paths and has to
// apply the same rule to both of them.
func ReplaceEnter(data []byte, txEOL string) []byte {
	enter := enterBytes(txEOL)

	switch len(data) {
	case 0:
		return enter

	case 1:
		if data[0] != '\n' {
			return data
		}
		return enter

	default:
		// A trailing "\r\n" or "\n" is the line ending; replace it and keep any
		// text before it. Only the ending is touched — the text must not be
		// searched for newlines, because a multi-line paste ends each of its
		// lines the way the terminal already sent it.
		if data[len(data)-2] == '\r' && data[len(data)-1] == '\n' {
			out := make([]byte, 0, len(data)-1+len(enter))
			out = append(out, data[:len(data)-2]...)
			return append(out, enter...)
		}
		if data[len(data)-1] == '\n' {
			out := make([]byte, 0, len(data)-1+len(enter))
			out = append(out, data[:len(data)-1]...)
			return append(out, enter...)
		}
		return data
	}
}
