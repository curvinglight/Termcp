package serial

import "testing"

func TestRxNormalizeLoneLF(t *testing.T) {
	var n rxNormalizer
	if got := string(n.Transform([]byte("a\nb"))); got != "a\r\nb" {
		t.Fatalf("got %q, want %q", got, "a\r\nb")
	}
}

func TestRxNormalizeExistingCRLFUnchanged(t *testing.T) {
	// A device already sending CRLF must not get a second CR.
	var n rxNormalizer
	if got := string(n.Transform([]byte("a\r\nb"))); got != "a\r\nb" {
		t.Fatalf("got %q, want %q", got, "a\r\nb")
	}
}

func TestRxNormalizeLoneCRPreserved(t *testing.T) {
	// Progress bars and REPLs overwrite a line with CR and no LF. Rewriting that
	// would break the display, so a CR with no LF following it is left alone.
	var n rxNormalizer
	if got := string(n.Transform([]byte("progress\r"))); got != "progress\r" {
		t.Fatalf("got %q, want %q", got, "progress\r")
	}
}

func TestRxNormalizeCRSplitAcrossChunks(t *testing.T) {
	// The case that requires state: CR ends one read, LF starts the next. They
	// are one line ending, so the result must contain exactly one CRLF.
	var n rxNormalizer
	first := n.Transform([]byte("line\r"))
	second := n.Transform([]byte("\nnext"))

	got := string(first) + string(second)
	if got != "line\r\nnext" {
		t.Fatalf("got %q, want %q", got, "line\r\nnext")
	}
}

func TestRxNormalizeCRSplitThenPlainText(t *testing.T) {
	// A CR at the end of a chunk with ordinary text after it was a lone CR, so
	// it stays as-is and the text is converted normally.
	var n rxNormalizer
	first := n.Transform([]byte("a\r"))
	second := n.Transform([]byte("b\n"))

	got := string(first) + string(second)
	if got != "a\rb\r\n" {
		t.Fatalf("got %q, want %q", got, "a\rb\r\n")
	}
}

func TestRxNormalizeLFMustNotGetDoubleCR(t *testing.T) {
	// The failure mode of a stateless normalizer: the CR half already produced
	// output, and the LF half adds another because it did not know.
	var n rxNormalizer
	first := n.Transform([]byte("\r\n"))
	second := n.Transform([]byte("\n"))

	got := string(first) + string(second)
	if got != "\r\n\r\n" {
		t.Fatalf("got %q, want %q (one CRLF per line ending)", got, "\r\n\r\n")
	}
}

func TestRxNormalizeFlushClearsPendingState(t *testing.T) {
	// After a stream ends, the normalizer must not carry a claim that an LF is
	// still coming. Flush makes the end explicit.
	var n rxNormalizer
	n.Transform([]byte("x\r"))
	n.Flush()

	// A fresh stream's first LF is a lone LF again, so it gets a CR.
	if got := string(n.Transform([]byte("\n"))); got != "\r\n" {
		t.Fatalf("after Flush, a lone LF should normalize to %q, got %q", "\r\n", got)
	}
}

func TestRxNormalizeTrailingCRIsEmitted(t *testing.T) {
	// A transcript whose last byte is a lone CR must keep it. Holding the CR
	// back to see whether an LF follows is only safe if it is emitted when the
	// stream ends; this normalizer emits it immediately, which is the simplest
	// way to guarantee that.
	var n rxNormalizer
	got := string(n.Transform([]byte("done\r")))
	if got != "done\r" {
		t.Fatalf("got %q, want %q (the trailing CR must not be dropped)", got, "done\r")
	}

	if extra := n.Flush(); len(extra) != 0 {
		t.Fatalf("Flush returned %q; the CR was already emitted", extra)
	}
}

func TestRxNormalizeEmptyChunk(t *testing.T) {
	var n rxNormalizer
	if got := n.Transform(nil); got != nil {
		t.Fatalf("Transform(nil) = %q, want nil", got)
	}
	if got := n.Transform([]byte{}); got != nil {
		t.Fatalf("Transform(empty) = %q, want nil", got)
	}
}

func TestRxNormalizeDoesNotMutateInput(t *testing.T) {
	var n rxNormalizer
	input := []byte("a\nb")
	original := string(input)

	n.Transform(input)

	if string(input) != original {
		t.Fatalf("Transform modified its input: %q, want %q", input, original)
	}
}

func TestRxNormalizeOnlyLFs(t *testing.T) {
	var n rxNormalizer
	if got := string(n.Transform([]byte("no newlines here"))); got != "no newlines here" {
		t.Fatalf("got %q, want the input unchanged", got)
	}
}
