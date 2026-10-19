package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/capability"
	"github.com/open-mcp-ai/termcp/internal/session"
)

// POST /api/term is the same protocol over HTTP.
//
// These tests pin the four things that make that claim true rather than merely
// intended, because each of them could silently rot into "a second, smaller API
// that happens to look similar":
//
//  1. the reply is the SAME envelope the tool returns, slot for slot;
//  2. the command space is not a subset — a directive the tool can run, HTTP runs;
//  3. a failed command is a failing SLOT, not a failed request (a batch keeps its
//     successful slots), which is the one thing a REST client has to be able to
//     depend on;
//  4. the entrance is recorded, so a held command line can say which surface
//     submitted it instead of always claiming to be an agent.

// postTerm is one request to the handler under test.
func postTerm(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/term", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.TermHandler().ServeHTTP(rec, req)
	return rec
}

// termSlot decodes the reply. A single slot arrives bare (the tool does the same
// with structuredContent), a batch arrives wrapped in {"slots": [...]}, so the
// helper normalises both into a list.
func termSlots(t *testing.T, rec *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("reply is not JSON: %v, body: %s", err, rec.Body.String())
	}
	if raw, ok := body["slots"]; ok {
		list, ok := raw.([]any)
		if !ok {
			t.Fatalf("slots is %T, want an array", raw)
		}
		out := make([]map[string]any, 0, len(list))
		for _, item := range list {
			out = append(out, item.(map[string]any))
		}
		return out
	}
	return []map[string]any{body}
}

// TestHTTPTermReturnsTheSameEnvelope: the HTTP reply carries the same fields the
// tool's structured projection does, for the same command.
//
// The point is not the JSON shape (which a test can assert trivially) but that
// there is ONE projection: if these ever disagree, an agent and a script are
// reading two descriptions of one operation and only one of them is true.
func TestHTTPTermReturnsTheSameEnvelope(t *testing.T) {
	s := newTestServer(t)

	rec := postTerm(t, s, `{"cmd":"help"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a successful command is a 200)", rec.Code)
	}
	slots := termSlots(t, rec)
	if len(slots) != 1 {
		t.Fatalf("got %d slots, want 1", len(slots))
	}
	if got := slots[0]["protocol"]; got != "TERM/1" {
		t.Errorf("protocol = %v, want TERM/1", got)
	}
	if got := slots[0]["code"]; got != float64(200) {
		t.Errorf("code = %v, want 200", got)
	}
	if body, _ := slots[0]["body"].(string); !strings.Contains(body, "help [directive]") {
		t.Errorf("body does not look like the help listing: %q", body)
	}

	// The same command through the tool surface must produce the same text.
	tool := termText(t, s, map[string]any{"cmd": "help"})
	if viaHTTP := rec.Header().Get("X-Termcp-Text"); viaHTTP != escapeHeader(tool) {
		t.Errorf("HTTP text projection differs from the tool's:\nhttp: %q\ntool: %q", viaHTTP, tool)
	}
}

// TestHTTPTermIsTheWholeCommandSpace: a window parameter the schema promises must
// work over HTTP too, or the HTTP entrance is a different protocol wearing the
// same name.
func TestHTTPTermIsTheWholeCommandSpace(t *testing.T) {
	s := newTestServer(t)

	// bytes_limit=0 is the "consume nothing" window: a generated reply comes back
	// empty with 204, which proves the argument reached parseTermArgs rather than
	// being ignored as an unknown body field.
	slots := termSlots(t, postTerm(t, s, `{"cmd":"help","bytes_limit":0}`))
	if got := slots[0]["code"]; got != float64(204) {
		t.Fatalf("code = %v, want 204 for a zero-byte window", got)
	}
	if body, _ := slots[0]["body"].(string); body != "" {
		t.Errorf("body = %q, want empty under bytes_limit=0", body)
	}

	// An out-of-range window is the argument error the tool gives, over HTTP.
	rec := postTerm(t, s, `{"cmd":"help","total_bytes_limit":0}`)
	slots = termSlots(t, rec)
	if got := slots[0]["code"]; got != float64(400) {
		t.Errorf("code = %v, want 400 for an invalid window", got)
	}
	if got := slots[0]["Error-Code"]; got != "invalid_argument" {
		t.Errorf("Error-Code = %v, want invalid_argument", got)
	}
}

// TestHTTPTermBatchIsOneReplyPerStatement pins the slot rule over HTTP: `;` runs
// several statements and gives one slot each, so a batch reply is a list, not a
// concatenated body.
//
// It also pins the narrower truth a caller has to know: a directive this build
// does not have fails at PARSE time, before any statement runs, so it is one
// failing slot for the whole command rather than a good slot next to a bad one.
// That is the tool's behaviour exactly — the HTTP entrance parses with the same
// parser — and a test that expected two slots would be asserting a second,
// friendlier protocol that does not exist.
func TestHTTPTermBatchIsOneReplyPerStatement(t *testing.T) {
	s := newTestServer(t)

	// Two statements that both succeed: one reply, two slots, each labelled with
	// the command it ran.
	slots := termSlots(t, postTerm(t, s, `{"cmd":"ps; help"}`))
	if len(slots) != 2 {
		t.Fatalf("got %d slots, want 2 (one per statement)", len(slots))
	}
	if got := slots[0]["Path"]; got != "ps" {
		t.Errorf("first slot Path = %v, want ps", got)
	}
	if got := slots[1]["Path"]; got != "help" {
		t.Errorf("second slot Path = %v, want help", got)
	}

	// An unknown directive is a parse error: one slot, its own code, and the whole
	// directive table so the caller learns the shape without a second call.
	rec := postTerm(t, s, `{"cmd":"ps; nosuchdirective x"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: nothing in the command parsed", rec.Code)
	}
	slots = termSlots(t, rec)
	if len(slots) != 1 {
		t.Fatalf("got %d slots, want 1 for a parse failure", len(slots))
	}
	if got := slots[0]["Error-Code"]; got != "unknown_directive" {
		t.Errorf("Error-Code = %v, want unknown_directive", got)
	}
	if usage, _ := slots[0]["Usage"].(string); !strings.Contains(usage, "|") {
		t.Errorf("Usage = %q, want the directive table", usage)
	}
}

// TestHTTPTermAllFailedIsAFailingStatus: a reply where nothing succeeded has no
// success to report, so the HTTP status follows the slot instead of pretending.
// Without this, `curl -f` cannot tell a typo from a working call.
func TestHTTPTermAllFailedIsAFailingStatus(t *testing.T) {
	s := newTestServer(t)

	rec := postTerm(t, s, `{"cmd":"nosuchdirective x"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 when every slot failed", rec.Code)
	}
}

// TestHTTPTermEntranceIsRecorded pins that the entrance reaches the record.
//
// This is the defect the REST entrance would have shipped with: the source was
// hardcoded "mcp" at its two call sites, so a command line submitted over HTTP
// would have been queued, and later logged, as an agent's. A reviewer reading
// "source: mcp" on a request a script sent is being told something false about
// who is asking.
func TestHTTPTermEntranceIsRecorded(t *testing.T) {
	s := newTestServer(t)
	if got := callerSource(withCallerSource(t.Context(), "rest")); got != "rest" {
		t.Errorf("callerSource = %q, want rest", got)
	}
	// The tool surface sets nothing, so it must keep reading as an agent.
	if got := callerSource(t.Context()); got != "mcp" {
		t.Errorf("callerSource with no claim = %q, want mcp", got)
	}
	// And the transcript attribution follows it: a REST write is the operator's
	// own input, not an agent's.
	if got := callerInputSource(withCallerSource(t.Context(), "rest")); got != session.InputFromAPI {
		t.Errorf("input source for rest = %v, want the API source", got)
	}
	if got := callerInputSource(t.Context()); got != session.InputFromAI {
		t.Errorf("input source with no claim = %v, want the AI source", got)
	}

	// The handler installs the claim itself, so a caller inherits it without
	// doing anything. This is the assertion that FAILS if the handler forgets:
	// testing withCallerSource alone would pass no matter what the handler does,
	// which is the difference between testing a helper and testing the wire.
	if got := sourceSeenByHandler(t, s); got != "rest" {
		t.Errorf("POST /api/term reached the command with source %q, want rest", got)
	}
}

// sourceSeenByHandler drives the real handler and reports the source a held
// command line was recorded with.
//
// The observable is the review queue, not the helper: a session is put in review
// mode, the request is made over HTTP, and the queue stamps Source on whatever it
// holds. Reading callerSource() directly would pass no matter what the handler
// does, which is the difference between testing a helper and testing the wire.
func sourceSeenByHandler(t *testing.T, s *Server) string {
	t.Helper()
	sessionID, shellID := startGatedSession(t, s, 1, time.Minute)
	rec := postTerm(t, s, `{"cmd":"run `+shellID+` echo probe"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a held command line is a slot, not a request failure)", rec.Code)
	}
	slots := termSlots(t, rec)
	if got := slots[0]["code"]; got != float64(202) {
		t.Fatalf("code = %v, want 202 review_pending: %s", got, rec.Body.String())
	}
	queue := s.sessMgr.Get(sessionID).ApprovalQueue()
	if queue == nil {
		t.Fatal("a reviewed session has no approval queue")
	}
	pending := queue.List()
	if len(pending) == 0 {
		t.Fatal("nothing queued: the held command line was not submitted for review")
	}
	return pending[0].Source
}

// TestHTTPTermMalformedBodyIsAnEnvelope: a caller that mistyped its JSON reads the
// same format as one that mistyped a directive, instead of falling out of the
// protocol into a plain-text parse error.
func TestHTTPTermMalformedBodyIsAnEnvelope(t *testing.T) {
	s := newTestServer(t)

	rec := postTerm(t, s, `{"cmd": `)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	slots := termSlots(t, rec)
	if got := slots[0]["protocol"]; got != "TERM/1" {
		t.Errorf("protocol = %v, want TERM/1 even for a malformed body", got)
	}
	if got := slots[0]["Error-Code"]; got != "invalid_argument" {
		t.Errorf("Error-Code = %v, want invalid_argument", got)
	}
}

// TestHTTPTermEmptyBodyIsTheGuide: an empty body means the same thing an empty
// cmd does. The guidance call must not be reachable only through the tool.
func TestHTTPTermEmptyBodyIsTheGuide(t *testing.T) {
	s := newTestServer(t)

	slots := termSlots(t, postTerm(t, s, ""))
	if got := slots[0]["code"]; got != float64(200) {
		t.Fatalf("code = %v, want 200", got)
	}
	body, _ := slots[0]["body"].(string)
	if !strings.Contains(body, "termcp terminal") {
		t.Errorf("empty body did not return the guide: %q", body[:min(120, len(body))])
	}
}

// TestHTTPTermAdmitsThroughTheCapabilityTable is the security half of the claim
// that this entrance reuses admission rather than re-deriving it.
//
// A session carrying a template's whitelist must refuse over HTTP exactly as it
// refuses over MCP — same capability, same refusal text. If this ever passes
// while the tool surface refuses (or the reverse), the entrance became a way
// around the whitelist, which is the failure the whole capability table exists
// to prevent.
func TestHTTPTermAdmitsThroughTheCapabilityTable(t *testing.T) {
	s := newTestServer(t)

	// A restricted session that may not write to its shell.
	sid := guardTestSession(t, s, []string{capability.ShellList}, nil)

	// `ps` only lists, so it is admitted.
	slots := termSlots(t, postTerm(t, s, `{"cmd":"ps"}`))
	if got := slots[0]["code"]; got != float64(200) {
		t.Errorf("ps over HTTP = %v, want 200 for a listing-only template", got)
	}

	// `run` writes to the shell, which the template does not grant.
	rec := postTerm(t, s, `{"cmd":"run `+sid+`:1 echo hi"}`)
	slots = termSlots(t, rec)
	code := slots[0]["code"]
	if code == float64(200) {
		t.Fatalf("run over HTTP succeeded against a template that does not grant shell.input: %s", rec.Body.String())
	}
	if got := slots[0]["Error-Code"]; got != "forbidden" {
		t.Errorf("Error-Code = %v, want forbidden", got)
	}

	// The tool surface must answer the same way for the same command: one
	// decision, two entrances.
	toolText := termText(t, s, map[string]any{"cmd": "run " + sid + ":1 echo hi"})
	if !strings.Contains(toolText, "403") {
		t.Errorf("the tool surface did not refuse the same command, so the two entrances disagree:\n%s", toolText)
	}
}

// TestStructuredOnlyHalvesTheReply pins the switch that declines the second copy
// of a body.
//
// The defect it exists for: a reply sends its bytes twice — once as the rendered
// text, once in structuredContent — so a caller reading a large log pays for it
// twice. Measured on `help` that is +25%, on a 200-line read +57%. The window
// parameters exist to stop a caller paying for output it did not ask for, and a
// duplicate of output it DID ask for is the same waste through another door.
//
// Two things have to be true, and the test pins both: the structured projection
// still carries everything (so a JSON-only client loses nothing), and the text
// projection is actually gone (so the bytes are actually saved).
func TestStructuredOnlyHalvesTheReply(t *testing.T) {
	s := newTestServer(t)

	both := s.Term(t.Context(), "help", map[string]any{})
	if both.Text() == "" {
		t.Fatal("the default reply must keep the text projection")
	}
	if len(both.Structured()) != 1 || both.Structured()[0]["body"] == "" {
		t.Fatal("the default reply must keep the structured projection")
	}

	only := s.Term(t.Context(), "help", map[string]any{"structured_only": true})
	if only.Text() != "" {
		t.Errorf("structured_only left a text projection of %d bytes", len(only.Text()))
	}
	// The bytes are not lost, they are simply carried once.
	if got := only.Structured()[0]["body"]; got != both.Structured()[0]["body"] {
		t.Errorf("structured_only changed the body:\n%v\n%v", got, both.Structured()[0]["body"])
	}
	// Status and headers survive too: the projection is the same value. The code
	// is an int here, not the float64 a JSON round-trip would give, because this
	// reads the value the server built rather than a decoded reply.
	if got := only.Structured()[0]["code"]; got != 200 {
		t.Errorf("code = %v (%T), want 200", got, got)
	}
	if got := only.Structured()[0]["Path"]; got != "help" {
		t.Errorf("Path = %v, want help", got)
	}
}

// TestStructuredOnlyOmitsTextContent pins the wire consequence on the tool
// result: the mode must produce no text content block, not an empty one. A client
// that renders text should be told "nothing here" by absence, and an empty string
// would still cost its framing.
func TestStructuredOnlyOmitsTextContent(t *testing.T) {
	s := newTestServer(t)

	full := termCall(t, s, map[string]any{"cmd": "help"})
	if len(full.Content) == 0 {
		t.Fatal("the default result must carry a text content block")
	}

	lean := termCall(t, s, map[string]any{"cmd": "help", "structured_only": true})
	if len(lean.Content) != 0 {
		t.Errorf("structured_only result carries %d content block(s), want 0", len(lean.Content))
	}
	if lean.StructuredContent == nil {
		t.Error("structured_only result has no structuredContent: the caller would get nothing at all")
	}
	if lean.IsError {
		t.Error("a successful reply must not be flagged as an error")
	}
}

// TestStructuredOnlyOverHTTP pins that the REST entrance takes the same switch.
// The saving is largest exactly where REST is most likely to be used (a big read),
// and a flag that exists on one entrance and not the other is two protocols.
func TestStructuredOnlyOverHTTP(t *testing.T) {
	s := newTestServer(t)

	rec := postTerm(t, s, `{"cmd":"help","structured_only":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	// REST already sends the rendered form only as an optional header, so the
	// switch's job here is to stop paying for that header too.
	if got := rec.Header().Get("X-Termcp-Text"); got != "" {
		t.Errorf("X-Termcp-Text is %d bytes, want none under structured_only", len(got))
	}
	slots := termSlots(t, rec)
	if got := slots[0]["body"]; got == nil || got == "" {
		t.Error("the JSON body must still carry the bytes")
	}
}

// TestTermDocsAreGenerated pins that docs/api.md's argument table is the table
// this package renders.
//
// It replaces a name-matching test that was one bug away from useless: it first
// accepted any backticked mention of an argument anywhere in §16, so deleting the
// table row still passed because a prose paragraph happened to name it. Matching
// names was always a proxy — the thing that must be true is "the doc contains
// exactly what the generator produces", and that is a comparison, not a guess.
//
// What this catches, concretely: an argument added to termArgDecls (so it appears
// in the schema and is parsed) without regenerating the doc; a doc row edited by
// hand; a marker moved. The fix in every case is `go run ./cmd/termdoc`.
func TestTermDocsAreGenerated(t *testing.T) {
	doc, err := os.ReadFile("../../docs/api.md")
	if err != nil {
		t.Fatalf("read docs/api.md: %v", err)
	}
	body := string(doc)

	// The markers must exist: RenderTermDoc reports "unchanged" when they are
	// missing, so without this check a doc that lost them would pass silently —
	// which is the same class of blind spot the previous version had.
	if !strings.Contains(body, termDocBegin) || !strings.Contains(body, termDocEnd) {
		t.Fatalf("docs/api.md is missing the termdoc markers; the argument table cannot be generated")
	}

	if _, changed := RenderTermDoc(body); changed {
		t.Errorf("docs/api.md is stale: run `go run ./cmd/termdoc`")
	}
}

// TestTermDocTableCoversTheSchema pins the other direction: every argument the
// schema declares has a generated row, and no row names an argument the schema
// does not have.
//
// The generated-table test above would catch both, but only by reporting "stale" —
// which is a poorer diagnosis than "the table is missing X". This one names it.
func TestTermDocTableCoversTheSchema(t *testing.T) {
	s := newTestServer(t)
	var list struct {
		Tools []struct {
			Name        string         `json:"name"`
			InputSchema map[string]any `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(callMCP(t, s, "tools/list", nil), &list); err != nil {
		t.Fatal(err)
	}
	var props map[string]any
	for i := range list.Tools {
		if list.Tools[i].Name == termToolName {
			props, _ = list.Tools[i].InputSchema["properties"].(map[string]any)
		}
	}
	if len(props) == 0 {
		t.Fatalf("tools/list has no %q schema", termToolName)
	}

	rows := docTableRows()
	for name := range props {
		if !strings.Contains(rows, "| `"+name+"` |") {
			t.Errorf("the rendered table has no row for the %q argument the schema declares", name)
		}
	}
	// And nothing extra: a row for an argument that does not exist is a doc
	// promising behaviour the build cannot deliver.
	for _, d := range termArgDecls {
		if _, ok := props[d.Name]; !ok {
			t.Errorf("the table declares %q but the schema does not", d.Name)
		}
	}
}
