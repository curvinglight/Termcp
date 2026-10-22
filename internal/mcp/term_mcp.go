package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/open-mcp-ai/termcp/internal/directive"
	"github.com/open-mcp-ai/termcp/internal/envelope"
)

// The mcp directive's stdio form: an MCP server running ON a shell channel.
//
// This is the half of `mcp` that is not a transport. A handle that names a shell
// is a process the server already started (`pipe mac --argv npx -y …`), and
// driving it is typing a frame at it and reading the answer back — no socket, no
// HTTP, and nothing to open. The URL forms live in term_jrpc.go, where the three
// frame-channel doors share one implementation.
//
// Keeping this here is what lets `mcp` stay one directive after gaining a
// registry: the handle decides which half runs, and a caller does not have to
// know which kind it holds.

// mcpStdio speaks to the process running on a shell channel.
//
// A stdio peer is just a line-oriented pipe: `run` writes the frame, `read` gets the
// reply. That is the whole implementation, and it is why an MCP server needs no
// registration ceremony — `pipe --argv` starts it, and this types at it.
func (s *Server) mcpStdio(ctx context.Context, stmt directive.Statement, sub string, handle string, rest []string) envelope.Slot {
	headers := map[string]string{"Path": stmt.Path}
	switch sub {
	case "tools", "send", "call":
		var frame string
		switch sub {
		case "tools":
			frame = `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
		default:
			payload := unquoteWord(strings.TrimSpace(strings.Join(rest, " ")))
			if payload == "" {
				return badRequest(stmt, "mcp "+sub+" needs a JSON-RPC frame")
			}
			// VALIDATE BEFORE WRITING, the same way the URL forms already do.
			//
			// A stdio peer reads one frame per line, so anything unparseable is either
			// ignored or answered with a parse error — and the caller saw 202 "sent"
			// either way, which says the frame landed when in fact it was never a frame.
			// `cdp send 'not json'` already answered 400; the two used to disagree about
			// what "sent" means.
			if !json.Valid([]byte(payload)) {
				return badRequest(stmt, "the frame is not JSON")
			}
			frame = payload
		}
		// One frame, one line: the peer reads line-delimited JSON-RPC.
		if _, fail := s.callTool(ctx, "shell_input", argsOf("shell_id", handle, "text", frame)); fail != nil {
			return s.slotFromFailure(ctx, stmt, fail)
		}
		if _, fail := s.callTool(ctx, "shell_key", argsOf("shell_id", handle, "key", "enter")); fail != nil {
			return s.slotFromFailure(ctx, stmt, fail)
		}
		// THE HINT NAMES THE VERB THAT EXISTS. `mcp recv` was a directive of its own until
		// the surface became verbs over a tree; the reply kept telling callers to run it,
		// so the one instruction this message gives would have answered "unknown
		// directive". Frames are read with `--frame`, because a plain `read` of a channel
		// is bytes — which is what a command's output usually is.
		return envelope.Slot{Code: 202, Headers: headers, Body: "sent; read the reply with `read " + handle + " --frame`"}
	case "recv":
		return s.mcpRecvFrame(ctx, stmt, handle)
	case "close":
		// The cursor has to go with the handle: handles are minted monotonically
		// (jrpcHandleSeq), so a reopened channel never reuses this key and the map
		// would grow by one entry per channel the process ever opened.
		s.dropRecvCursor(handle)
		return envelope.Slot{Code: 204, Headers: headers}
	}
	return badRequest(stmt, "unknown mcp subcommand: "+sub)
}

// mcpRecvFrame reads exactly one JSON-RPC frame and advances this handle's cursor
// past it.
//
// The "exactly one" part is the contract: a frame is complete when the bytes parsed
// as JSON, and a partial one must be held rather than acted on. The read is
// therefore retried while the buffer does not yet parse — never a blocking read
// that may return half a message.
//
// The cursor is the whole point, and it is kept HERE rather than left to
// shell_output. shell_output has two modes and neither is a queue consumer: with
// an offset it is a stateless positional read, and without one it mints a NEW
// reader at the current end of the stream. A recv that passed neither therefore
// read from "now" on every call, so a first call that ended mid-frame reported
// 204 — and the next call started at the new end, skipping the frames that had
// been waiting behind it. Measured on three queued replies: recv delivered one and
// silently dropped the other two.
//
// With an explicit offset read the position is unambiguous and stateless on the
// server side, so the map below is the only state and it advances by exactly the
// frame that was returned. Bytes between frames (the newlines a line-delimited
// peer writes) are skipped by the trim in firstJSONValue, and the offset advances
// over them too, so a resumed read starts at the next '{'.
func (s *Server) mcpRecvFrame(ctx context.Context, stmt directive.Statement, handle string) envelope.Slot {
	from := s.recvCursor(handle)
	// A whole frame has to fit the window, or a single large reply could never be
	// delivered: the window is a cap on what one recv will consider, not a
	// statement about how long a frame may be.
	window := defaultBytesLimit
	if window < minRecvWindow {
		window = minRecvWindow
	}
	// POLL, BECAUSE THE READ ITSELF DOES NOT WAIT FOR AN OFFSETTED WINDOW.
	//
	// shell_output's timeout is honoured only by its live-cursor mode; the
	// offset-addressed mode returns what is buffered right now, and nothing else. A
	// recv that asked once therefore answered "no complete frame yet" while the peer's
	// bytes were still in flight — and a caller had to guess how long to keep asking.
	// The retry loop is here, with a bound, so one recv means one frame rather than
	// one sample.
	//
	// The bound is the same window the directive exists inside: a caller that gets
	// 204 after it has been told the frame is not coming yet, and the next recv
	// resumes from the cursor this one left untouched.
	deadline := time.Now().Add(recvPollBudget)
	var doc map[string]any
	var body string
	for {
		res, fail := s.callTool(ctx, "shell_output", argsOf(
			"shell_id", handle, "offset", float64(from),
			"max_bytes", float64(window), "timeout", float64(0), "strip_ansi", false))
		if fail != nil {
			return s.slotFromFailure(ctx, stmt, fail)
		}
		var ok bool
		doc, ok = toolResultMap(res)
		if !ok {
			return s.slotFromFailure(ctx, stmt, &toolFailure{code: CodeOperationFailed, msg: toolResultText(res)})
		}
		body = getString(doc, "output", "")
		if _, complete := firstJSONValue(body); complete {
			break
		}
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return s.slotFromFailure(ctx, stmt, &toolFailure{code: CodeOperationFailed, msg: ctx.Err().Error()})
		case <-time.After(recvPollInterval):
		}
	}
	// rawLen, not len(body): the window's byte span is what the next read's offset
	// is measured in. len(body) counts the RENDERED string, which differs whenever
	// the reply was ANSI-stripped or escaped, so advancing by it lands mid-frame.
	// windowResult names this quantity raw_bytes for exactly this reason.
	rawLen := int64(getFloatAny(doc, "raw_bytes", 0))
	frame, complete := firstJSONValue(body)
	total := int64(getFloatAny(doc, "total_bytes", 0))
	if !complete {
		// `has_more` says the channel holds bytes this window did not return, which
		// means a frame is longer than the window rather than absent. Reporting 204
		// there would be a lie the caller cannot act on; a 413 names the problem.
		if getBool(doc, "has_more", false) && total > from {
			return envelope.Slot{Code: 413, Headers: map[string]string{
				"Path": stmt.Path, "Error-Code": "frame_too_large",
				"Content-Range": envelope.UnsatisfiedRange(total),
			}, Body: fmt.Sprintf("a frame at offset %d is longer than the %d-byte window", from, window)}
		}
		return envelope.Slot{Code: 204, Headers: map[string]string{
			"Path":          stmt.Path,
			"Content-Range": envelope.ContentRange(from, from+rawLen-1, total),
		}, Body: "no complete frame yet"}
	}
	start := from + int64(strings.Index(body, frame))
	end := start + int64(len(frame))
	// Advance past the frame, and past the whitespace that separated it from the
	// next one, so a resumed read starts on a value rather than on a newline.
	next := end
	for next < from+rawLen && int(next-from) < len(body) && strings.IndexByte(" \t\r\n", body[next-from]) >= 0 {
		next++
	}
	s.setRecvCursor(handle, next)

	// A SUB-MCP'S SAMPLING REQUEST IS ANSWERED HERE, NOT RETURNED TO THE CALLER.
	//
	// `sampling/createMessage` is the one request that flows the other way: the
	// server being driven asks the client's model to generate something. A sub-MCP
	// started on a shell channel has no model, so it asks its host — and this is the
	// host. Returning that frame to the caller instead would leave the sub-MCP
	// blocked on an answer nobody was going to write, and the caller reading a
	// request it did not ask for.
	//
	// Forwarding is transparent on purpose: the caller asked for one frame and gets
	// one frame, after the round trip to the Agent has happened underneath. The
	// answer is written back to the sub-MCP's stdin and the cursor moves past it.
	if handled, slot := s.forwardSubMCPSampling(ctx, stmt, handle, frame); handled {
		if slot != nil {
			return *slot
		}
		// Answered and written back: read again, so this recv still delivers a frame.
		return s.mcpRecvFrame(ctx, stmt, handle)
	}

	return envelope.Slot{
		Code:    200,
		Headers: map[string]string{"Path": stmt.Path, "Content-Range": envelope.ContentRange(start, end-1, end)},
		Body:    frame,
	}
}

// forwardSubMCPSampling answers an inbound `sampling/createMessage` from a sub-MCP
// by asking the Agent that is driving this server, and writes the result back.
//
// handled=false means the frame is not a sampling request and the caller should
// treat it normally. handled=true with a non-nil slot means the round trip failed
// and that slot is the reply; handled=true with a nil slot means the answer was
// written back and the caller should keep reading.
//
// The Agent session is taken from the CONTEXT, not from a stored handle: this runs
// inside the tool call the Agent made, so ctx carries exactly the client that is
// waiting for the reply. A stored session would be wrong as soon as two Agents
// drive the same daemon.
func (s *Server) forwardSubMCPSampling(ctx context.Context, stmt directive.Statement, handle, frame string) (bool, *envelope.Slot) {
	var req struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal([]byte(frame), &req); err != nil {
		return false, nil // not JSON we understand; let the caller see it
	}
	if req.Method != "sampling/createMessage" {
		return false, nil
	}
	// No id: a notification, not a request. Nothing to answer.
	if len(req.ID) == 0 || string(req.ID) == "null" {
		return false, nil
	}

	var params mcpgo.CreateMessageParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		// Malformed params: answer the sub-MCP with a JSON-RPC error rather than
		// leaving it blocked on a request that will never be answered.
		return true, s.replyToSubMCP(ctx, stmt, handle, req.ID, nil, "invalid sampling params: "+err.Error())
	}

	target := mcpserver.ClientSessionFromContext(ctx)
	if target == nil {
		return true, s.replyToSubMCP(ctx, stmt, handle, req.ID, nil,
			"no Agent session on this call; a sub-MCP's sampling request can only be forwarded while an Agent is driving the tool call")
	}

	// Method is a PROMOTED field, so it cannot be set in the literal under the
	// module's Go version; assign it after construction.
	createReq := mcpgo.CreateMessageRequest{CreateMessageParams: params}
	createReq.Method = "sampling/createMessage"
	res, err := s.ForwardSampling(ctx, target, createReq)
	if err != nil {
		return true, s.replyToSubMCP(ctx, stmt, handle, req.ID, nil, err.Error())
	}
	return true, s.replyToSubMCP(ctx, stmt, handle, req.ID, res, "")
}

// replyToSubMCP writes a JSON-RPC answer back to the sub-MCP's stdin.
//
// A nil result produces an error object, which is what the sub-MCP is waiting for
// either way: silence would leave its request outstanding forever.
func (s *Server) replyToSubMCP(ctx context.Context, stmt directive.Statement, handle string, id json.RawMessage, res *mcpgo.CreateMessageResult, errMsg string) *envelope.Slot {
	answer := map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id)}
	if errMsg != "" {
		answer["error"] = map[string]any{"code": -32603, "message": errMsg}
	} else {
		answer["result"] = res
	}
	payload, err := json.Marshal(answer)
	if err != nil {
		slot := envelope.Slot{Code: 500, Headers: map[string]string{
			"Path": stmt.Path, "Error-Code": "sampling_answer_unencodable",
		}, Body: "could not encode the sampling answer: " + err.Error()}
		return &slot
	}
	if _, fail := s.callTool(ctx, "shell_input", argsOf("shell_id", handle, "text", string(payload))); fail != nil {
		slot := s.slotFromFailure(ctx, stmt, fail)
		return &slot
	}
	if _, fail := s.callTool(ctx, "shell_key", argsOf("shell_id", handle, "key", "enter")); fail != nil {
		slot := s.slotFromFailure(ctx, stmt, fail)
		return &slot
	}
	return nil
}

// How long one recv keeps asking, and how often.
//
// The pair is a compromise between the two callers that exist: an agent reading a
// stdio MCP server's answer wants `mcp recv` to return the frame, not a 204 it has to
// loop on, while the same call must still come back promptly when the answer really
// has not been written. Two seconds is below the 6-9s an assistant turn costs and long enough
// for a peer that has already been sent the request.
const (
	recvPollBudget   = 2 * time.Second
	recvPollInterval = 25 * time.Millisecond
)

// minRecvWindow is the smallest window a recv will read with.
//
// defaultBytesLimit is the tool's default for a READ, where a caller usually wants
// a screenful. A JSON-RPC frame is a different unit: an initialize result with
// instructions is routinely larger than that, and a window smaller than one frame
// makes it undeliverable. 1 MiB is far past any legitimate frame and still a bound.
const minRecvWindow = 1 << 20

// recvCursor and setRecvCursor keep one read position per channel.
//
// Keyed by handle without the server's own offset bookkeeping: two callers reading
// the same channel is a race the protocol does not describe, and serialising here
// means neither silently loses a frame to the other's window.
func (s *Server) recvCursor(handle string) int64 {
	s.recvCursorMu.Lock()
	defer s.recvCursorMu.Unlock()
	return s.recvCursors[handle]
}

// dropRecvCursor forgets a closed handle's read position.
func (s *Server) dropRecvCursor(handle string) {
	s.recvCursorMu.Lock()
	defer s.recvCursorMu.Unlock()
	delete(s.recvCursors, handle)
}

func (s *Server) setRecvCursor(handle string, at int64) {
	s.recvCursorMu.Lock()
	defer s.recvCursorMu.Unlock()
	if s.recvCursors == nil {
		s.recvCursors = make(map[string]int64)
	}
	s.recvCursors[handle] = at
}

// firstJSONValue returns the first complete JSON value in raw.
//
// It scans for a complete object or array rather than trusting a newline: a frame
// may be pretty-printed, and a line-delimited peer may still emit a value with
// embedded newlines inside a string.
func firstJSONValue(raw string) (string, bool) {
	// SKIP THE NOISE BEFORE THE FRAME, and there is always some: a stdio peer's own
	// startup output shares this stream. Measured on @modelcontextprotocol/
	// server-everything it was 149 bytes of npm warning and a "Starting default
	// (STDIO) server..." banner, and every recv on that channel reported "no complete
	// frame yet" while a complete frame sat in the buffer — the decoder was being
	// handed the warning, not the frame.
	//
	// A frame starts at a '{' or a '['. Nothing before the first one can be part of
	// it: a JSON object's first byte must be '{' (a leading BOM or whitespace is not
	// this protocol's problem, and whitespace is handled by the scan below). Scanning
	// for those two bytes is what the line-delimited reality of a pipe channel needs.
	start := strings.IndexAny(raw, "{[")
	if start < 0 {
		return "", false
	}
	trimmed := raw[start:]
	dec := json.NewDecoder(strings.NewReader(trimmed))
	var value json.RawMessage
	if err := dec.Decode(&value); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) || isSyntaxEOF(err) {
			return "", false
		}
		return "", false
	}
	return strings.TrimSpace(string(value)), true
}

// isSyntaxEOF reports whether a JSON decode failed only because the input ended.
func isSyntaxEOF(err error) bool {
	var syn *json.SyntaxError
	if errors.As(err, &syn) {
		return false
	}
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}
