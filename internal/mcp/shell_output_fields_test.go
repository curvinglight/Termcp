package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/ansi"
	"github.com/open-mcp-ai/termcp/internal/clock"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// archivedShellWithBytes creates a DEAD session restored from disk and appends
// raw to its byte log, returning the session id to read from.
//
// The bytes go in through the log rather than a live shell because a test needs
// escape sequences at exact offsets, and a real shell will not emit a
// bootloader-style cursor address on demand. It also keeps the test identical on
// all three CI platforms.
func archivedShellWithBytes(t *testing.T, raw string) (*Server, string) {
	t.Helper()
	s, store, sessMgr, msgMgr := newTestServerWithHistory(t)

	sessID := "archived-ansi"
	shellID := "shell-ansi"

	meta := api.Session{
		ID:        sessID,
		Name:      "ANSI test",
		Status:    api.SessionExited,
		CreatedAt: clock.Now() - 60_000,
		Shells:    []api.Session{{ID: shellID, Name: "main", Status: api.SessionExited}},
	}
	if err := store.SaveSession(meta); err != nil {
		t.Fatalf("SaveSession failed: %v", err)
	}
	for _, sh := range meta.Shells {
		if err := store.SaveShell(meta.ID, sh); err != nil {
			t.Fatalf("SaveShell failed: %v", err)
		}
	}
	if err := sessMgr.RestoreDead(); err != nil {
		t.Fatalf("RestoreDead failed: %v", err)
	}
	if _, err := msgMgr.AppendOutput(sessID, shellID, []byte(raw)); err != nil {
		t.Fatalf("AppendOutput failed: %v", err)
	}
	return s, sessID
}

// readPage reads one page with an explicit offset, the mode a caller uses to
// page deterministically and the one a cut sequence forces.
func readPage(t *testing.T, s *Server, shellID string, offset, maxBytes int, stripANSI bool) map[string]any {
	t.Helper()
	res, err := s.handleReadOutput(context.Background(), makeRequest(map[string]any{
		"shell_id":   shellID,
		"offset":     float64(offset),
		"max_bytes":  float64(maxBytes),
		"strip_ansi": stripANSI,
	}))
	if err != nil || res.IsError {
		t.Fatalf("shell_output(offset=%d) error: %v", offset, err)
	}
	return parseResult(t, res)
}

// TestShellOutput_ReportsAnsiTruncated pins the flag that makes a cut sequence
// legible.
//
// The bytes are never lost — they are in the log and an offset read returns
// them — but the visible tail of a cut page is sequence debris that reads like
// device output. A bootloader prompt whose cursor address was cut renders as
// "6Hgrub>", which is what turned a truncated command into a confident
// misdiagnosis of the wrong subsystem.
func TestShellOutput_ReportsAnsiTruncated(t *testing.T) {
	const raw = "boot: \x1b[006;126Hgrub> echo MARK1\r\n"
	// The page ends before the sequence's final "H".
	cut := strings.Index(raw, "\x1b[006;126H") + len("\x1b[006;12")

	s, sessID := archivedShellWithBytes(t, raw)

	m := readPage(t, s, sessID, 0, cut, true)
	if !m["ansi_truncated"].(bool) {
		t.Errorf("page ending mid-sequence reported ansi_truncated=false; output=%q", m["output"])
	}
	if out := m["output"].(string); !strings.Contains(out, ansi.CutMarker) {
		t.Errorf("cut page output carries no marker: %q", out)
	}
	// The text before the cut is still readable.
	if out := m["output"].(string); !strings.Contains(out, "boot:") {
		t.Errorf("cut page lost its readable text: %q", out)
	}

	// The same sequence read whole is neither truncated nor marked.
	whole := readPage(t, s, sessID, 0, len(raw), true)
	if whole["ansi_truncated"].(bool) {
		t.Errorf("a page holding the complete sequence reported ansi_truncated=true; output=%q", whole["output"])
	}
	if out := whole["output"].(string); strings.Contains(out, ansi.CutMarker) {
		t.Errorf("a complete page carries the cut marker: %q", out)
	}
	if out := whole["output"].(string); !strings.Contains(out, "grub>") {
		t.Errorf("complete sequence should strip to the real text, got %q", out)
	}
}

// TestShellOutput_AnsiTruncatedFalseWhenStrippingOff pins the other half of the
// contract: with strip_ansi off the caller receives the raw bytes and can see
// the escape itself, so reporting a cut would be noise.
func TestShellOutput_AnsiTruncatedFalseWhenStrippingOff(t *testing.T) {
	const raw = "boot: \x1b[006;126Hgrub> \r\n"
	cut := strings.Index(raw, "\x1b[006;126H") + len("\x1b[006;12")

	s, sessID := archivedShellWithBytes(t, raw)
	m := readPage(t, s, sessID, 0, cut, false)

	if m["ansi_truncated"].(bool) {
		t.Error("ansi_truncated=true with strip_ansi=false; raw bytes are returned untouched")
	}
	if out := m["output"].(string); !strings.Contains(out, "\x1b[006;12") {
		t.Errorf("strip_ansi=false must return the raw fragment, got %q", out)
	}
}

// TestShellOutput_BytesReturnedCountsRawBytes pins the unit of bytes_returned.
//
// It used to be len(output), which is not comparable with the offsets printed
// beside it: strip_ansi deletes escape sequences and compact rewrites line
// endings, so 13 raw bytes came back as bytes_returned=4. Checking an echo
// against what was sent is the only available way to notice a bootloader that
// truncated a command, and that check is impossible while the two numbers count
// different things.
func TestShellOutput_BytesReturnedCountsRawBytes(t *testing.T) {
	const raw = "\x1b[32mGREEN\x1b[0m \x1b[1mBOLD\x1b[0m \x1b[2K\x1b[1;1Hplain text here\r\n"

	s, sessID := archivedShellWithBytes(t, raw)
	m := readPage(t, s, sessID, 0, len(raw), true)

	start := int64(m["start_offset"].(float64))
	end := int64(m["end_offset"].(float64))
	bytesRet := int64(m["bytes_returned"].(float64))

	if bytesRet != end-start {
		t.Errorf("bytes_returned=%d but end_offset-start_offset=%d; they must count the same bytes",
			bytesRet, end-start)
	}
	if bytesRet != int64(len(raw)) {
		t.Errorf("bytes_returned=%d, want the raw page length %d", bytesRet, len(raw))
	}

	out := m["output"].(string)
	if int64(len(out)) == bytesRet {
		t.Logf("page stripped to the same length (%d); the offset identity above is the real assertion", bytesRet)
	}
	if outBytes := int64(m["output_bytes"].(float64)); outBytes != int64(len(out)) {
		t.Errorf("output_bytes=%d, want %d", outBytes, len(out))
	}
}

// TestShellOutput_OffsetPagingAgreesWithBytesReturned is the property the
// change exists to make available: summing a paged read reproduces the stream
// length, so a caller can measure what a device echoed against what it sent.
func TestShellOutput_OffsetPagingAgreesWithBytesReturned(t *testing.T) {
	const raw = "\x1b[32mA\x1b[0m\x1b[32mB\x1b[0m\x1b[32mC\x1b[0m plain\r\nsecond line\r\n"

	s, sessID := archivedShellWithBytes(t, raw)

	var seen int64
	for offset := 0; offset < len(raw); {
		page := readPage(t, s, sessID, offset, 8, true)
		n := int64(page["bytes_returned"].(float64))
		if n <= 0 {
			break
		}
		seen += n
		offset += int(n)
	}
	if seen != int64(len(raw)) {
		t.Errorf("paged over %d bytes, want the full %d", seen, len(raw))
	}
}
