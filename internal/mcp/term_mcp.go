package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"strings"
	"sync"
	"sync/atomic"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/open-mcp-ai/termcp/internal/directive"
	"github.com/open-mcp-ai/termcp/internal/envelope"
)

// The mcp directive: a JSON-RPC conversation.
//
// Four transports live behind it, and the interesting one is `raw`: `mcp send` and
// `mcp recv` assume nothing about the methods, so any line-oriented JSON-RPC peer
// works — an MCP server, an LSP, a private RPC. The protocol decides what the
// frames mean; this only moves them.
//
// The channel forms use handles:
//
//	pipe mac --argv npx -y some-server     → a stdio peer on a pipe channel
//	mcp open https://host/mcp              → streamable HTTP, NO shell channel spent
//	mcp tools|send|recv|call|close <handle>
//
// `mcp open` rides HTTP directly. That matters: a caller with a limited shell
// budget can hold a dozen HTTP endpoints without spending any of it, which is the
// difference between relaying an endpoint and starting a process.

// mcpChannels is the registry of opened HTTP endpoints, keyed by handle.
//
// ponytail: an in-memory map with no eviction. An endpoint is opened by an agent
// inside one conversation; a leak would be one small session struct per opened
// URL, and the alternative (a reaper) is machinery nobody asked for. Add eviction
// when a long-lived instance actually accumulates them.
var (
	mcpChannelsMu sync.Mutex
	mcpChannels   = map[string]*httpMCPChannel{}
)

// mcpHandleSeq mints handles monotonically: len(mcpChannels)+1 can reuse a
// handle after a close and silently point two names at one channel.
var mcpHandleSeq atomic.Int64

// httpMCPChannel is one opened HTTP endpoint.
type httpMCPChannel struct {
	url         string
	sse         bool
	session     string // Mcp-Session-Id, once initialize answered
	client      *http.Client
	endpoint    string // SSE: the message URL discovered from the event stream
	nextID      int
	initialized bool
}

// mcpOpen connects an HTTP MCP endpoint and hands back a handle.
//
// It does not spend a shell channel: the transport is HTTP, so `ps` grows nothing.
// The handle is an opaque short name; it is what the other subcommands take.
func (s *Server) mcpOpen(ctx context.Context, stmt directive.Statement, url string) envelope.Slot {
	u, err := neturl.Parse(url)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return envelope.Slot{Code: 400, Headers: map[string]string{"Path": stmt.Path, "Error-Code": "invalid_argument"},
			Body: "mcp open needs an http(s) URL, e.g. https://host/mcp"}
	}
	if strings.HasSuffix(url, "/sse") {
		return envelope.Slot{Code: 400, Headers: map[string]string{"Path": stmt.Path, "Error-Code": "invalid_argument"},
			Body: "the mcp directive speaks streamable HTTP; use the endpoint that serves POST /stream (the legacy /sse transport is not implemented)"}
	}
	ch := &httpMCPChannel{url: url, client: http.DefaultClient, nextID: 1}
	handle := fmt.Sprintf("mcp%d", mcpHandleSeq.Add(1))
	mcpChannelsMu.Lock()
	mcpChannels[handle] = ch
	mcpChannelsMu.Unlock()
	return envelope.Slot{
		Code:    200,
		Headers: map[string]string{"Path": stmt.Path, "Handle": handle},
		Body:    fmt.Sprintf("%s %s (no shell channel consumed)\n", handle, url),
	}
}

// mcpChannel dispatches the subcommands that address an existing channel.
func (s *Server) mcpChannel(ctx context.Context, stmt directive.Statement, sub string, rest []string) envelope.Slot {
	if len(rest) == 0 {
		return envelope.Slot{Code: 400, Headers: map[string]string{"Path": stmt.Path, "Error-Code": "invalid_argument"},
			Body: "mcp " + sub + " needs a handle (a shell handle for stdio, or the one mcp open returned)"}
	}
	handle := rest[0]
	mcpChannelsMu.Lock()
	ch, ok := mcpChannels[handle]
	if ok && sub == "close" {
		delete(mcpChannels, handle)
	}
	mcpChannelsMu.Unlock()
	if ok {
		if sub == "close" {
			return envelope.Slot{Code: 204, Headers: map[string]string{"Path": stmt.Path}}
		}
		return s.mcpHTTP(ctx, stmt, sub, ch, rest[1:])
	}
	// Not an opened endpoint: it is a shell handle, and the peer is the process on
	// that channel.
	return s.mcpStdio(ctx, stmt, sub, handle, rest[1:])
}

// mcpHTTP speaks to an opened endpoint.
func (s *Server) mcpHTTP(ctx context.Context, stmt directive.Statement, sub string, ch *httpMCPChannel, rest []string) envelope.Slot {
	headers := map[string]string{"Path": stmt.Path}
	switch sub {
	case "recv":
		return envelope.Slot{Code: 204, Headers: headers,
			Body: "an HTTP endpoint answers each send; there is nothing to receive. Use `mcp call`."}
	case "send", "call", "tools":
		var frame map[string]any
		switch sub {
		case "tools":
			frame = map[string]any{"jsonrpc": "2.0", "id": ch.nextID, "method": "tools/list"}
		default:
			// rest is already everything AFTER the handle (mcpChannel sliced it),
			// so the frame starts at rest[0]. Skipping one more dropped the frame's
			// first word: `mcp call h {"jsonrpc":…}` reported "needs a frame" and
			// only worked with a junk word in front of it.
			payload := unquoteWord(strings.TrimSpace(strings.Join(rest, " ")))
			if payload == "" {
				return envelope.Slot{Code: 400, Headers: map[string]string{"Path": stmt.Path, "Error-Code": "invalid_argument"},
					Body: "mcp " + sub + " needs a JSON-RPC frame"}
			}
			if err := json.Unmarshal([]byte(payload), &frame); err != nil {
				return envelope.Slot{Code: 400, Headers: map[string]string{"Path": stmt.Path, "Error-Code": "invalid_argument"},
					Body: "the frame is not JSON: " + err.Error()}
			}
		}
		method, _ := frame["method"].(string)
		if method != "initialize" {
			if err := ch.ensureInitialized(ctx); err != nil {
				return envelope.Slot{Code: 500, Headers: map[string]string{"Path": stmt.Path, "Error-Code": CodeConnectionFailed}, Body: err.Error()}
			}
		}
		ch.nextID++
		reply, err := ch.post(ctx, frame)
		if err != nil {
			return envelope.Slot{Code: 500, Headers: map[string]string{"Path": stmt.Path, "Error-Code": CodeConnectionFailed}, Body: err.Error()}
		}
		if method == "initialize" {
			ch.initialized = true
		}
		return envelope.Slot{Code: 200, Headers: headers, Body: reply}
	}
	return envelope.Slot{Code: 400, Headers: map[string]string{"Path": stmt.Path, "Error-Code": "invalid_argument"},
		Body: "unknown mcp subcommand: " + sub}
}

// ensureInitialized performs the MCP initialize handshake for streamable HTTP.
// It runs once per channel; the session id it captures is what every later
// frame must carry, and its absence was the "Invalid session ID" failure.
func (c *httpMCPChannel) ensureInitialized(ctx context.Context) error {
	if c.initialized || c.sse {
		return nil
	}
	id := c.nextID
	init := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": mcpgo.LATEST_PROTOCOL_VERSION,
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "termcp", "version": "mcp-directive"},
		},
	}
	c.nextID++
	reply, err := c.post(ctx, init)
	if err != nil {
		return err
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(reply), &doc); err != nil {
		return fmt.Errorf("initialize: %s", reply)
	}
	if e, ok := doc["error"]; ok {
		return fmt.Errorf("initialize: %v", e)
	}
	note := map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}
	if _, err := c.post(ctx, note); err != nil {
		return err
	}
	c.initialized = true
	return nil
}

// sseData extracts the JSON-RPC payload from a text/event-stream body: the
// event whose data id matches wantID, or the first data event for a
// notification. Server-sent responses are normal for streamable HTTP.
func sseData(raw string, wantID any) string {
	var data strings.Builder
	var best string
	flush := func() {
		payload := strings.TrimSpace(data.String())
		data.Reset()
		if payload == "" {
			return
		}
		var probe map[string]any
		if json.Unmarshal([]byte(payload), &probe) != nil {
			return
		}
		if wantID == nil {
			if best == "" {
				best = payload
			}
			return
		}
		if got, ok := probe["id"]; ok && fmt.Sprint(got) == fmt.Sprint(wantID) {
			best = payload
		}
	}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			flush()
			continue
		}
		if payload, ok := strings.CutPrefix(line, "data:"); ok {
			data.WriteString(strings.TrimSpace(payload))
			data.WriteByte('\n')
		}
	}
	flush()
	if best != "" {
		return best
	}
	return strings.TrimSpace(raw)
}

// post sends one frame and returns the reply body.
func (c *httpMCPChannel) post(ctx context.Context, frame map[string]any) (string, error) {
	body, err := json.Marshal(frame)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.session != "" {
		req.Header.Set("Mcp-Session-Id", c.session)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		c.session = sid
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 400 {
		return string(raw), fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	text := strings.TrimSpace(string(raw))
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		text = sseData(text, frame["id"])
	}
	return text, nil
}

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
				return envelope.Slot{Code: 400, Headers: map[string]string{"Path": stmt.Path, "Error-Code": "invalid_argument"},
					Body: "mcp " + sub + " needs a JSON-RPC frame"}
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
		return envelope.Slot{Code: 202, Headers: headers, Body: "sent; read the reply with `mcp recv " + handle + "`"}
	case "recv":
		return s.mcpRecvFrame(ctx, stmt, handle)
	case "close":
		return envelope.Slot{Code: 204, Headers: headers}
	}
	return envelope.Slot{Code: 400, Headers: map[string]string{"Path": stmt.Path, "Error-Code": "invalid_argument"},
		Body: "unknown mcp subcommand: " + sub}
}

// mcpRecvFrame reads exactly one JSON-RPC frame.
//
// The "exactly one" part is the contract: a frame is complete when the bytes parsed
// as JSON, and a partial one must be held rather than acted on. The read is
// therefore retried while the buffer does not yet parse — never a blocking read
// that may return half a message.
//
// No offset is passed, deliberately. The read tool's offset defaults to -1, which
// means "carry on from where the last read stopped"; passing the stream SIZE
// instead (which is what the old code did) asks for bytes after the end, so recv
// answered "no complete frame yet" for a frame that was sitting right there in the
// channel. A recv is a consumer, not a snapshot: it must advance the same cursor
// every other read advances, or the second call re-delivers the first frame.
func (s *Server) mcpRecvFrame(ctx context.Context, stmt directive.Statement, handle string) envelope.Slot {
	res, fail := s.callTool(ctx, "shell_output", argsOf(
		"shell_id", handle, "max_bytes", float64(defaultBytesLimit), "timeout", float64(0)))
	if fail != nil {
		return s.slotFromFailure(ctx, stmt, fail)
	}
	doc, ok := toolResultMap(res)
	if !ok {
		return s.slotFromFailure(ctx, stmt, &toolFailure{code: CodeOperationFailed, msg: toolResultText(res)})
	}
	body := getString(doc, "output", "")
	frame, complete := firstJSONValue(body)
	if !complete {
		return envelope.Slot{Code: 204, Headers: map[string]string{
			"Path":          stmt.Path,
			"Content-Range": envelope.UnsatisfiedRange(int64(getFloatAny(doc, "total_bytes", 0))),
		}, Body: "no complete frame yet"}
	}
	start := int64(getFloatAny(doc, "start_offset", 0))
	end := start + int64(len(frame))
	return envelope.Slot{
		Code:    200,
		Headers: map[string]string{"Path": stmt.Path, "Content-Range": envelope.ContentRange(start, end-1, end)},
		Body:    frame,
	}
}

// firstJSONValue returns the first complete JSON value in raw.
//
// It scans for a complete object or array rather than trusting a newline: a frame
// may be pretty-printed, and a line-delimited peer may still emit a value with
// embedded newlines inside a string.
func firstJSONValue(raw string) (string, bool) {
	trimmed := strings.TrimLeft(raw, " \t\r\n")
	if trimmed == "" {
		return "", false
	}
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
