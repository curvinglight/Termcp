package mcp

import (
	"strings"
	"testing"
	"time"
)

// TestAmpersandRunsStatementsConcurrently pins that `&` is not decorative.
//
// It used to be: the shell parser reports `a & b` as two statements and records the
// parallelism in a Background flag that NOTHING read, so the line ran sequentially
// while reading as parallel. A silent semantic reversal is worse than a refusal.
//
// THE MEASUREMENT IS THE WINDOW COUNT, NOT THE PICTURE OF WALL CLOCK. A pty `run`
// waits its whole timeout whatever happens inside, so two parallel runs cost one
// window and two sequential runs cost two. That ratio is the signal; an absolute
// duration would only measure how busy the runner was.
func TestAmpersandRunsStatementsConcurrently(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	const window = 2.0

	start := time.Now()
	seq := termText(t, s, map[string]any{
		"cmd":     "run " + h + " echo ONE; run " + h + " echo TWO",
		"timeout": window,
	})
	sequential := time.Since(start)
	if n := strings.Count(seq, "TERM/1 "); n != 2 {
		t.Fatalf("the sequential baseline must produce 2 slots, got %d", n)
	}

	start = time.Now()
	par := termText(t, s, map[string]any{
		"cmd":     "run " + h + " echo ONE & run " + h + " echo TWO",
		"timeout": window,
	})
	parallel := time.Since(start)
	if n := strings.Count(par, "TERM/1 "); n != 2 {
		t.Fatalf("the parallel batch must still produce one slot per statement, got %d:\n%s", n, par)
	}

	// Parallel is about half the sequential cost. The bound is 70% rather than 50% so
	// a loaded runner cannot flake it, while a sequential run (100%) fails clearly.
	if parallel > sequential*7/10 {
		t.Errorf("`&` ran sequentially: parallel=%v sequential=%v", parallel.Round(time.Millisecond), sequential.Round(time.Millisecond))
	}
}

// TestParallelSlotsKeepCommandOrder pins that a batch reports its slots in the order
// the caller wrote them. Slots are addressed positionally, so completion order would
// make the reply's meaning depend on which command happened to finish first.
func TestParallelSlotsKeepCommandOrder(t *testing.T) {
	s := newTestServer(t)
	h1 := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h1})
	h2 := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h2})

	out := termText(t, s, map[string]any{
		"cmd":     "run " + h1 + " echo FIRST & run " + h2 + " echo SECOND",
		"timeout": float64(2),
	})
	i1 := strings.Index(out, "FIRST")
	i2 := strings.Index(out, "SECOND")
	if i1 < 0 || i2 < 0 {
		t.Fatalf("both outputs must be present:\n%s", out)
	}
	if i1 > i2 {
		t.Errorf("slots were collected by completion order, not command order:\n%s", out)
	}
}

// TestSemicolonStillSeparatesSequentially pins that `;` did not become `&`:
// the separator is not a concurrency operator.
func TestSemicolonStillSeparatesSequentially(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	out := termText(t, s, map[string]any{
		"cmd":     "run " + h + " echo ONE; run " + h + " echo TWO",
		"timeout": float64(2),
	})
	if n := strings.Count(out, "TERM/1 "); n != 2 {
		t.Errorf("expected 2 slots, got %d:\n%s", n, out)
	}
	if !strings.Contains(out, "ONE") || !strings.Contains(out, "TWO") {
		t.Errorf("both statements must run:\n%s", out)
	}
}
