package serial

// rxNormalizer converts lone LF bytes to CRLF, the way a PTY would, so a serial
// transcript renders correctly in a terminal that treats LF as "move down"
// rather than "return to column 0".
//
// It carries state across calls deliberately. A device's CR and LF arrive as
// separate bytes and may land in different reads: a chunk ending in CR followed
// by a chunk starting with LF is one line ending, and converting each chunk
// independently would either double the CR or drop it.
//
// A lone CR is left alone. Progress bars and REPLs overwrite their line by
// emitting CR without LF, and rewriting those would corrupt the display.
type rxNormalizer struct {
	// pendingCR records that the previous chunk ended with CR, so a leading LF
	// in the next chunk is recognized as the other half of one line ending.
	pendingCR bool
}

// Transform normalizes a chunk of device output.
//
// The returned slice is a fresh allocation; the input is not modified.
func (n *rxNormalizer) Transform(chunk []byte) []byte {
	if len(chunk) == 0 {
		return nil
	}

	out := make([]byte, 0, len(chunk)+8)
	for i := 0; i < len(chunk); i++ {
		b := chunk[i]

		if b == '\n' {
			if n.pendingCR {
				// CR LF across a read boundary: the CR is already in the output
				// from the previous chunk. Emit the LF and clear the flag, so the
				// pair is neither doubled nor split.
				n.pendingCR = false
				out = append(out, '\n')
				continue
			}
			// A lone LF becomes CRLF.
			out = append(out, '\r', '\n')
			continue
		}

		// Any byte other than LF ends a pending CR: the CR was lone.
		n.pendingCR = false
		out = append(out, b)
		if b == '\r' {
			// Remember it in case the next byte (in this chunk or the next) is LF.
			n.pendingCR = true
		}
	}
	return out
}

// Flush returns any byte held back by a trailing CR.
//
// It must be called once the stream ends. Transform emits CR immediately rather
// than holding it, so the only state Flush clears is the cross-boundary flag —
// but a caller that stops reading without flushing would still be leaving the
// normalizer in a state that claims one more byte is coming. Calling it makes
// the end explicit and lets the flag be reset for a reused normalizer.
//
// The returned slice is empty in practice because CR was already written; the
// helper exists so the "stream ended" transition is a named step rather than an
// omission, and so a future change that does hold bytes has one place to release
// them.
func (n *rxNormalizer) Flush() []byte {
	n.pendingCR = false
	return nil
}
