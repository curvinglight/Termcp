package ansi

import (
	"regexp"
)

var ansiRe = regexp.MustCompile(
	`\x1b(?:` +
		`[\[(][0-?]*[ -/]*[@-~]` + // CSI sequences
		`|\].*?(?:\x1b\\|\x07)` + // OSC sequences
		`|[()][Bb0UK]` + // Character set
		`|[ -/]*[0-~]` + // 2-byte sequences
		`)`)

// incompleteTailRe matches a trailing escape sequence that was cut short.
//
// It is anchored at the end and describes the part of a CSI or OSC sequence
// that has arrived *without* its final byte: the introducer, then any parameter
// and intermediate bytes, then nothing. A complete sequence is not matched,
// because its final byte [@-~] (or the OSC terminator) is what the pattern
// deliberately does not allow before the end of string.
//
// The introducer is required, so a lone ESC at the end is NOT a cut: that is a
// real 2-byte sequence whose second half has not been read yet, and Strip has
// always passed it through untouched. Treating it as cut would replace a
// correct behaviour with a marker on ordinary output.
//
// The OSC branch excludes both terminators, so a title sequence that did finish
// (with BEL or ST) is complete and left alone.
var incompleteTailRe = regexp.MustCompile(`\x1b(?:\[[0-?]*[ -/]*|\][^\x07\x1b]*)$`)

// CutMarker closes a page that stopped in the middle of an escape sequence.
//
// It is bracketed and carries no trailing newline. The leading bracket is what
// makes it findable: the fragment it follows is a run of digits and semicolons
// ("006;12") with no boundary of its own, so a bare marker would run into it —
// "006;12[cut mid-escape]" reads as one token. Nothing may follow it, because
// callers detect a cut by testing for the marker at the end of the output. The
// newline matters because the pipeline is Compact(Strip(...)) and one here
// would be counted as a returned line, making a cut page report one line more
// than it holds.
const CutMarker = " [cut mid-escape]"

// Strip removes ANSI escape codes from text.
//
// A sequence cut short by a read boundary is reported rather than silently
// mangled: its trailing bytes are left in the text and CutMarker is appended, so
// a caller paging the stream can tell "the device ended here" from "this page
// stopped in the middle of a sequence" and resume from the right offset.
//
// The bytes are not dropped, because they cannot be told apart from a device
// that genuinely printed "006;1" — and dropping them would make this page look
// like a clean end of output when it is only the end of a read.
func Strip(text string) string {
	complete, incomplete := SplitIncompleteTail(text)
	stripped := ansiRe.ReplaceAllString(complete, "")
	if incomplete != "" {
		return stripped + incomplete + CutMarker
	}
	return stripped
}

// SplitIncompleteTail separates a trailing escape sequence that a read boundary
// cut short from the rest of the text.
//
// complete is everything up to the cut; incomplete is the fragment itself, empty
// when the text ends on a complete sequence or contains no escape at all.
//
// Callers that need to know whether a page was cut use incomplete != "". The
// fragment is returned rather than discarded because it is still real output:
// the buffer holds every byte, and only the page boundary is an artefact of how
// the caller read it.
func SplitIncompleteTail(text string) (complete, incomplete string) {
	loc := incompleteTailRe.FindStringIndex(text)
	if loc == nil {
		return text, ""
	}
	return text[:loc[0]], text[loc[0]:]
}
