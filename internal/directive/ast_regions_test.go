package directive

import (
	"strings"
	"testing"

	"mvdan.cc/sh/v3/syntax"
)

// These pin the regions the parser knows about, after the pass stopped deciding them on
// its own.
//
// The rule they all follow is one sentence: a reference is a `$` the grammar reported, and
// what the grammar did not report the caller deliberately kept from being parsed. Each case
// below is that sentence landing somewhere a hand-rolled rule used to answer differently —
// a `&` read as a separator, an arithmetic expression read as a word list, a heredoc body
// read as a line, a flag value read as shell.
func TestRegionsTheParserAlreadyKnows(t *testing.T) {
	bs := string(rune(92))
	cases := []struct {
		name    string
		src     string
		sub     Subst
		want    string
		wantErr string
	}{
		{
			// Stmt.Semicolon is documented as "position of ';', '&', or '|&'", and trimming
			// it the way a ';' is trimmed dropped the backgrounding: the shell ran the
			// command in the foreground and the call blocked until it finished.
			name: "a backgrounding ampersand is part of the line",
			src:  "run s1:1 sleep 100 &",
			want: "sleep 100 &",
		},
		{
			// The `;` ends the termcp statement, so it is termcp's punctuation and never the
			// shell's: `echo hi;` is not a command line the caller wrote.
			name: "a semicolon still is not",
			src:  "run s1:1 echo hi; ls host",
			want: "echo hi",
		},
		{
			// A quoted word is not an operand. The parser reports the region as an
			// ArithmExp, so nothing has to guess that `$((` opened an expression.
			name: "an arithmetic expansion takes a bare operand",
			src:  "run s1:1 echo $(( $1 + 1 ))",
			sub:  Subst{Pos: []string{"2"}},
			want: "echo $(( 2 + 1 ))",
		},
		{
			name: "a named value in arithmetic is bare as well",
			src:  "run s1:1 echo $(( $N * 2 ))",
			sub:  Subst{Named: map[string]string{"N": "21"}},
			want: "echo $(( 21 * 2 ))",
		},
		{
			// Redirect.Hdoc IS the body, terminator excluded — the parser read it, so its
			// bounds are not a scan for a line that looks like the terminator.
			name: "a here-document body is text, not a word",
			src:  "run s1:1 cat <<EOF\n$1\nEOF\n",
			sub:  Subst{Pos: []string{"echo C"}},
			want: "cat <<EOF\necho C\nEOF",
		},
		{
			// A QUOTED DELIMITER IS THE CALLER SAYING "EXPAND NOTHING HERE", and the grammar
			// answers accordingly: no nodes inside the body, so no references, so the text
			// arrives as written. The same rule as everywhere else in this pass rather than a
			// heredoc special case — which is what makes it predictable.
			// "NO REFERENCE" IS NOW PROVED BY THE CHECK THAT EXISTS FOR IT: the value the
			// caller passed is reported unused, which is the same fact the old `want` asserted
			// (nothing was substituted) stated by the rule that a value nothing names is
			// refused rather than dropped.
			name:    "a quoted delimiter leaves the body alone",
			src:     "run s1:1 cat <<'EOF'\n$1\nEOF\n",
			sub:     Subst{Pos: []string{"echo C"}},
			wantErr: "never references $1",
		},
		{
			name: "the tab-stripping form too",
			src:  "run s1:1 cat <<-EOF\n\t$1\n\tEOF\n",
			sub:  Subst{Pos: []string{"echo C"}},
			want: "cat <<-EOF\n\techo C\n\tEOF",
		},
		{
			// A VARIABLE IS A HOLE IN THE DSL, NOT AN OPERAND OF A SHELL LINE. Writing a
			// shell statement around it is the one shape the protocol does not have: as data
			// the value becomes one quoted word and the caller's statement changes, as
			// syntax the caller's DATA becomes commands. Refusing says so; both insertions
			// would have been a quiet guess about which the caller meant.
			name:    "a reference beside a word in a shell line is refused",
			src:     "run s1:1 echo $1",
			sub:     Subst{Pos: []string{"a; b"}},
			wantErr: "cannot sit inside a shell line",
		},
		{
			name:    "an escaped dollar is not a reference",
			src:     "run s1:1 echo $" + bs + "1",
			sub:     Subst{Pos: []string{"a; b"}},
			wantErr: "never references $1",
		},
		{
			name:    "a single-quoted dollar is not a reference either",
			src:     "run s1:1 echo '$1'",
			sub:     Subst{Pos: []string{"a; b"}},
			wantErr: "never references $1",
		},
		{
			// A REDIRECT TARGET IS PART OF THE SHELL LINE, so a reference there is mixing
			// too — the value would be a filename, not a statement. Put the redirect inside
			// the value: cmd `run s1:1 $1`, args ["echo hi > '/tmp/a b'"].
			name:    "a reference in a redirect target is refused",
			src:     "run s1:1 echo hi > $1",
			sub:     Subst{Pos: []string{"/tmp/a b"}},
			wantErr: "cannot sit inside a shell line",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, err := ParseWithSubst(c.src, c.sub)
			if c.wantErr != "" {
				if err == nil {
					t.Fatalf("ParseWithSubst(%q) was accepted; it mixes DSL and shell", c.src)
				}
				if !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("ParseWithSubst(%q) failed with %v, want %q", c.src, err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseWithSubst(%q): %v", c.src, err)
			}
			if len(f.Statements) == 0 {
				t.Fatalf("%q parsed into no statements", c.src)
			}
			if got := f.Statements[0].Payload; got != c.want {
				t.Errorf("payload of %q:\n got: %q\nwant: %q", c.src, got, c.want)
			}
		})
	}
}

// TestTheAmpersandReachesTheTerminal is the end-to-end half of the first case above: the
// payload is what gets typed into the shell, so a dropped `&` is a foreground command
// however clean the parse looked.
func TestTheAmpersandReachesTheTerminal(t *testing.T) {
	f, err := ParseWithSubst("run s1:1 sleep 100 &", Subst{})
	if err != nil {
		t.Fatal(err)
	}
	payload := f.Statements[0].Payload
	if !strings.HasSuffix(payload, "&") {
		t.Fatalf("payload %q does not background the command; the caller asked for `&`", payload)
	}
}

// TestHeredocDelimiterQuotingComesFromTheAST pins the last byte-level syntax decision this
// pass used to make.
//
// What makes a heredoc body literal is that any part of the DELIMITER was quoted, and the
// pass used to find that out by scanning the delimiter's source slice for a quote or a
// backslash. The parser had already classified every part, so the question is a node type:
// SglQuoted or DblQuoted for a quote, a backslash inside a Lit for an escape, an ordinary
// Lit for neither. The cases below are the spellings where those differ.
func TestHeredocDelimiterQuotingComesFromTheAST(t *testing.T) {
	bs := string(rune(92))
	cases := []struct {
		name  string
		src   string
		quote bool
	}{
		{"bare delimiter", "run s1:1 cat <<EOF\n$1\nEOF\n", false},
		{"single-quoted delimiter", "run s1:1 cat <<'EOF'\n$1\nEOF\n", true},
		{"double-quoted delimiter", "run s1:1 cat <<\"EOF\"\n$1\nEOF\n", true},
		{"escaped delimiter", "run s1:1 cat <<" + bs + "EOF\n$1\nEOF\n", true},
		{"quote in the middle of the word", "run s1:1 cat <<E\"O\"F\n$1\nEOF\n", true},
		{"escape in the middle of the word", "run s1:1 cat <<E" + bs + "OF\n$1\nEOF\n", true},
		{"tab-stripping bare delimiter", "run s1:1 cat <<-EOF\n\t$1\n\tEOF\n", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, err := syntax.NewParser().Parse(strings.NewReader(c.src), "")
			if err != nil {
				t.Fatalf("Parse(%q): %v", c.src, err)
			}
			found := false
			syntax.Walk(f, func(n syntax.Node) bool {
				r, ok := n.(*syntax.Redirect)
				if !ok || r.Hdoc == nil {
					return true
				}
				found = true
				if got := wordIsQuoted(r.Word); got != c.quote {
					t.Errorf("wordIsQuoted(%q) = %v, want %v", c.src, got, c.quote)
				}
				return true
			})
			if !found {
				t.Fatalf("%q has no heredoc to inspect", c.src)
			}
		})
	}
}
