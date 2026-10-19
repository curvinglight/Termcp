package mcp

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/open-mcp-ai/termcp/internal/directive"
)

// The term argument table.
//
// One declaration per argument, and every place that publishes an argument reads
// it: the tool schema (term_register.go), docs/api.md's parameter table (§16, via
// cmd/termdoc), and the tests that check the two agree. Before this file they were
// three hand-kept copies, and they had already drifted: `structured_only` shipped
// in the schema and was missing from the doc, so a caller reading the REST
// reference could not discover the switch that halves a large reply.
//
// The rule this encodes is the same one the command space already follows — the
// directive table is the single source `help` renders from — extended to the
// arguments, which were the last hand-copied description on the surface.
//
// What stays hand-written here: the prose (a description is a sentence, not a
// derivation) and the parse call itself. What moved out: the defaults, the value
// ranges, and the per-argument shape the schema and the doc both need.

// argKind is the value type an argument carries.
type argKind int

const (
	argString argKind = iota
	argNumber
	argBool
	argEnum
)

// termArgDecl is one argument of the term tool.
type termArgDecl struct {
	Name string
	Kind argKind

	// Desc is the sentence a caller reads in the schema and in the docs. It is
	// written once and published verbatim in both, which is the whole point: a
	// description that lives in one place cannot disagree with itself.
	Desc string

	// DocNote is the doc's extra column, when the table has something to add that
	// the schema's one-liner cannot (a measured cost, a warning). Optional.
	DocNote string

	// Default and its rendering. Def is the value the parser uses when the
	// argument is absent; DefText is how it is spelled for a reader ("false",
	// "truncate", "8192"). They are separate because a float64 default prints as
	// 2 rather than 2.0 and an empty string default is a real, meaningful default
	// that must still be visible.
	Def     any
	DefText string

	// Enum lists the accepted values for argEnum.
	Enum []string
}

// termArgDecls is THE table. Order is publication order: the schema and the doc
// table both read it top to bottom, so a reader scanning either one meets the
// arguments in the same sequence.
var termArgDecls = []termArgDecl{
	{
		Name:    "cmd",
		Kind:    argString,
		Desc:    "One terminal line, e.g. `run a1f3:1 ls -la`. Empty = guide: usage summary plus what is available to act on now.",
		DocNote: "Empty or omitted returns the guide (where am I, what can I do, what is here) — never an error",
		Def:     "",
		DefText: "",
	},
	{
		Name:    "timeout",
		Kind:    argNumber,
		Desc:    fmt.Sprintf("Time axis: max seconds to wait. A pipe returns early when its process exits; a pty waits the window out. 0 = do not wait. Default: %d.", defaultTimeoutSeconds),
		DocNote: fmt.Sprintf("Seconds to wait (default %d; a pipe returns as soon as its process exits)", defaultTimeoutSeconds),
		Def:     defaultTimeoutSeconds,
		DefText: strconv.Itoa(defaultTimeoutSeconds),
	},
	{
		Name:    "bytes_limit",
		Kind:    argNumber,
		Desc:    fmt.Sprintf("Space axis: max raw bytes returned. 0 = handle only, consume nothing. Default: %d.", defaultBytesLimit),
		DocNote: fmt.Sprintf("Max bytes per slot (default %d; `0` = the handle only, no bytes)", defaultBytesLimit),
		Def:     defaultBytesLimit,
		DefText: strconv.Itoa(defaultBytesLimit),
	},
	{
		Name:    "lines_limit",
		Kind:    argNumber,
		Desc:    fmt.Sprintf("Space axis: max complete lines returned. 0 = no line limit. Default: %d.", defaultLinesLimit),
		DocNote: fmt.Sprintf("Max lines per slot (default %d; `0` = no line limit)", defaultLinesLimit),
		Def:     defaultLinesLimit,
		DefText: strconv.Itoa(defaultLinesLimit),
	},
	{
		Name:    "total_bytes_limit",
		Kind:    argNumber,
		Desc:    fmt.Sprintf("Whole reply: absolute cap across all slots. Default: %d.", defaultTotalBytesLimit),
		DocNote: fmt.Sprintf("Absolute cap for the whole reply (default %d)", defaultTotalBytesLimit),
		Def:     defaultTotalBytesLimit,
		DefText: strconv.Itoa(defaultTotalBytesLimit),
	},
	{
		Name:    "on_time_overflow",
		Kind:    argEnum,
		Enum:    []string{"truncate", "fold"},
		Desc:    "Time window overflow: truncate = return what arrived, fold = return no bytes. Default: truncate.",
		DocNote: "`truncate` (default) or `fold` (return no bytes this time)",
		Def:     "truncate",
		DefText: "truncate",
	},
	{
		Name:    "on_bytes_overflow",
		Kind:    argEnum,
		Enum:    []string{"truncate", "fold"},
		Desc:    "Byte window overflow: truncate = return the head, fold = return no bytes. Default: truncate.",
		DocNote: "`truncate` (default) or `fold` (return no bytes this time)",
		Def:     "truncate",
		DefText: "truncate",
	},
	{
		Name:    "on_lines_overflow",
		Kind:    argEnum,
		Enum:    []string{"truncate", "fold"},
		Desc:    "Line window overflow: truncate = return the first N lines, fold = return no bytes. Default: truncate.",
		DocNote: "`truncate` (default) or `fold` (return no bytes this time)",
		Def:     "truncate",
		DefText: "truncate",
	},
	{
		Name:    "pipe_to",
		Kind:    argString,
		Desc:    "Send this command's stdout into another handle's stdin; those bytes stay out of the reply body. Default: empty (stdout stays in the reply).",
		DocNote: "A handle whose stdin receives this reply's bytes; they leave the body",
		Def:     "",
		DefText: "",
	},
	{
		Name:    "learn_mode",
		Kind:    argBool,
		Desc:    "Attach the matched directive's one-line usage to its slot (once per directive). Default: false.",
		DocNote: "Attach each directive's usage line once per reply",
		Def:     false,
		DefText: "false",
	},
	{
		Name: "structured_only",
		Kind: argBool,
		// The body is the one part of a reply billed twice when both projections
		// go out (the rendered text and structuredContent carry the same bytes).
		// The switch is explicit rather than automatic because a model reads the
		// text form most comfortably, and the redundancy only hurts when the
		// payload is large — a judgment the caller is making anyway when it sets a
		// window on this very call.
		Desc:    "Send only the JSON projection, dropping the rendered text. Both carry the same bytes, so this halves a large reply's cost; a client that renders text will see none. Default: false.",
		DocNote: "Send only the JSON projection, dropping the rendered text. Both carry the same bytes, so this halves a large reply's cost (measured: −49%); a client that renders text sees none. Default `false`",
		Def:     false,
		DefText: "false",
	},
}

// termArgDeclFor finds one argument's declaration.
func termArgDeclFor(name string) (termArgDecl, bool) {
	for _, d := range termArgDecls {
		if d.Name == name {
			return d, true
		}
	}
	return termArgDecl{}, false
}

// parseTermArgs reads the window parameters and range-checks them. A value
// outside its domain is an argument error, not a silent substitution: a caller
// that typed 0 where it meant 8192 should be told, not quietly given a different
// read.
//
// The defaults come from termArgDecls, so an argument added to the table is
// parsed with the default the table publishes — the two cannot drift, and a
// default that exists in the schema but not in the parser (or the reverse) is not
// expressible.
func parseTermArgs(args map[string]any) (termArgs, *directive.Error) {
	w := termArgs{
		TimeoutSeconds:  getFloat64(args, "timeout", numDefault("timeout")),
		BytesLimit:      int(getFloat64(args, "bytes_limit", numDefault("bytes_limit"))),
		LinesLimit:      int(getFloat64(args, "lines_limit", numDefault("lines_limit"))),
		TotalBytesLimit: int(getFloat64(args, "total_bytes_limit", numDefault("total_bytes_limit"))),
		OnTimeOverflow:  overflowPolicy(args, "on_time_overflow"),
		OnBytesOverflow: overflowPolicy(args, "on_bytes_overflow"),
		OnLinesOverflow: overflowPolicy(args, "on_lines_overflow"),
		PipeTo:          strings.TrimSpace(getString(args, "pipe_to", "")),
		LearnMode:       getBool(args, "learn_mode", false),
		StructuredOnly:  getBool(args, "structured_only", false),
	}
	switch {
	case w.TimeoutSeconds < 0 || w.TimeoutSeconds > 3600:
		return termArgs{}, &directive.Error{Msg: "timeout must be between 0 and 3600 seconds"}
	case w.BytesLimit < 0:
		return termArgs{}, &directive.Error{Msg: "bytes_limit must be >= 0 (0 = handle only, no bytes)"}
	case w.LinesLimit < 0:
		return termArgs{}, &directive.Error{Msg: "lines_limit must be >= 0 (0 = no line limit)"}
	case w.TotalBytesLimit <= 0:
		return termArgs{}, &directive.Error{Msg: "total_bytes_limit must be > 0: it is the absolute cap for the whole reply"}
	case w.OnTimeOverflow == "":
		return termArgs{}, &directive.Error{Msg: "on_time_overflow must be truncate or fold"}
	case w.OnBytesOverflow == "":
		return termArgs{}, &directive.Error{Msg: "on_bytes_overflow must be truncate or fold"}
	case w.OnLinesOverflow == "":
		return termArgs{}, &directive.Error{Msg: "on_lines_overflow must be truncate or fold"}
	}
	return w, nil
}

// numDefault reads a numeric argument's default from the table.
//
// It panics on a missing or mistyped entry rather than falling back to a literal:
// the table is a compile-time constant set, so a mismatch is a programming error,
// and a silent fallback here would recreate exactly the drift this file removes —
// a default that exists in the parser and nowhere a caller can read it.
func numDefault(name string) float64 {
	d, ok := termArgDeclFor(name)
	if !ok {
		panic("no termArgDecls entry for " + name)
	}
	switch v := d.Def.(type) {
	case int:
		return float64(v)
	case float64:
		return v
	}
	panic("termArgDecls entry " + name + " has a non-numeric default")
}

// overflowPolicy normalises an overflow behaviour. truncate is the default
// because fold (return nothing this time) has to be asked for: a caller that runs
// `ls` on a huge directory wants the head of it, not an empty reply.
func overflowPolicy(args map[string]any, key string) string {
	v := strings.TrimSpace(strings.ToLower(getString(args, key, "")))
	switch v {
	case "", "truncate":
		return "truncate"
	case "fold":
		return "fold"
	}
	return "" // invalid; parseTermArgs turns this into a 400
}

// docTableRows renders the argument table for docs/api.md §16.
//
// This is what cmd/termdoc writes between the markers, and why the doc can no
// longer disagree with the schema about which arguments exist. Editing it by hand
// is pointless — TestTermDocTableIsGenerated fails until it is regenerated — so
// the only way to change what a caller reads is to change the table above.
func docTableRows() string {
	var b strings.Builder
	for _, d := range termArgDecls {
		note := d.DocNote
		if note == "" {
			note = d.Desc
		}
		b.WriteString("| `" + d.Name + "` | " + note + " |\n")
	}
	return b.String()
}

// The generated blocks in docs/api.md.
//
// A marker pair rather than a "replace the section" rule: the section holds prose
// that is written by hand (why the windows exist, what a slot is) and exactly one
// table that must not be. The markers say which is which, so a paragraph can be
// rewritten without the generator overwriting it and the table can be regenerated
// without the generator guessing where it ends.
const (
	termDocBegin = "<!-- BEGIN termdoc: args -->"
	termDocEnd   = "<!-- END termdoc: args -->"
)

// RenderTermDoc replaces the generated argument table in a docs/api.md body.
//
// It reports whether anything changed, so the caller can decide between a no-op
// and a rewrite (and the -check mode between success and failure) without reading
// the file twice.
//
// A missing or unpaired marker is NOT an error here: the function returns the
// input unchanged and says so, and the test that calls it asserts the markers
// exist. Reporting it as an error would make this function's contract "the doc is
// well-formed", which is a different question from "here is the rendered table".
func RenderTermDoc(doc string) (string, bool) {
	begin := strings.Index(doc, termDocBegin)
	if begin < 0 {
		return doc, false
	}
	afterBegin := begin + len(termDocBegin)
	end := strings.Index(doc[afterBegin:], termDocEnd)
	if end < 0 {
		return doc, false
	}
	end += afterBegin

	replacement := termDocBegin + "\n" + docTableRows() + termDocEnd
	current := doc[begin : end+len(termDocEnd)]
	if current == replacement {
		return doc, false
	}
	return doc[:begin] + replacement + doc[end+len(termDocEnd):], true
}
