package encoding

import (
	"golang.org/x/text/encoding"
)

// StreamingDecoder decodes a byte stream in chunks under one charset, keeping
// the trailing incomplete multi-byte character buffered until the chunk that
// completes it arrives.
//
// It exists for the live terminal stream. A burst boundary can fall in the
// middle of a GBK pair or a UTF-16 unit, and decoding each chunk independently
// would corrupt exactly the character that spans the boundary — the same class
// of damage trimIncomplete fixes for positional reads, except a stream cannot
// throw the tail away: those bytes belong to the NEXT chunk. The decoder holds
// them instead.
//
// UTF-8 needs no decoder (the terminal's own decoder is stateful across
// writes); this type is for the charsets only the server can translate.
type StreamingDecoder struct {
	name string
	dec  *encoding.Decoder
	tail []byte
}

// NewStreamingDecoder returns a chunk decoder for name. A name the charset
// table does not know (including "" and "utf-8") yields nil: the caller passes
// the bytes through, the way UTF-8 travels today.
func NewStreamingDecoder(name string) *StreamingDecoder {
	enc := charsets[name]
	if enc == nil {
		return nil
	}
	return &StreamingDecoder{name: name, dec: enc.NewDecoder()}
}

// Name reports the charset this decoder translates.
func (d *StreamingDecoder) Name() string { return d.name }

// Decode appends chunk to the held tail, translates every complete character,
// and keeps the trailing incomplete one for the next call. The returned bytes
// are always valid UTF-8 (for the charsets this table maps), ready for the
// terminal to paint.
func (d *StreamingDecoder) Decode(chunk []byte) []byte {
	if d == nil {
		return chunk
	}
	buf := append(d.tail, chunk...)
	cut := streamTail(d.name, buf)
	if cut < 0 || cut > len(buf) {
		cut = 0
	}
	complete := buf[:len(buf)-cut]
	d.tail = append(d.tail[:0], buf[len(buf)-cut:]...)
	if len(complete) == 0 {
		return nil
	}
	out, err := d.dec.Bytes(complete)
	if err != nil {
		// A decode failure inside a stream is not recoverable by retrying: the
		// bytes are shown as they arrived rather than dropped, so the operator
		// sees the damage instead of a hole.
		return complete
	}
	return out
}

// streamTail reports how many trailing bytes of buf are an incomplete
// character, for the streaming decoder. It differs from IncompleteTail in the
// one case that matters at a burst boundary: the WHOLE buffer may be an
// incomplete character (a one-byte chunk carrying a GBK lead), which
// IncompleteTail cannot answer because its search never strips the entire
// window. Here that answer is the whole point, so the search allows an empty
// prefix — and is capped at three held bytes, one less than the longest
// sequence any charset this table maps can form.
//
// A tail that never resolves (garbage that no prefix explains) is returned as
// 0 rather than held forever: the bytes decode with the visible damage, the
// same policy roundTrips enforces everywhere else.
func streamTail(name string, buf []byte) int {
	norm, known := KnownCharset(name)
	if !known {
		return 0
	}
	if norm == "utf-8" {
		return incompleteUTF8Tail(buf)
	}
	enc := charsets[norm]
	if roundTrips(enc, buf) {
		return 0
	}
	for back := 1; back <= 3 && back <= len(buf); back++ {
		if roundTrips(enc, buf[:len(buf)-back]) {
			return back
		}
	}
	if len(buf) <= 3 {
		// Nothing but the empty prefix round-trips: every byte is part of one
		// unfinished character (a one-byte chunk carrying a GBK lead is the
		// ordinary case).
		return len(buf)
	}
	return 0
}
