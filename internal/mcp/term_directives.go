package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/open-mcp-ai/termcp/internal/directive"
	"github.com/open-mcp-ai/termcp/internal/envelope"
	"github.com/open-mcp-ai/termcp/internal/locator"
	"github.com/open-mcp-ai/termcp/internal/ns"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// jsonMarshalIndent renders a document body for a generated reply.
func jsonMarshalIndent(m map[string]any) (string, error) {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// runDirective dispatches one parsed statement onto the existing handlers.
//
// A directive that names a batch (a comma-separated target) produces one slot per
// entry; everything else produces exactly one. That is the "slots are response
// units" rule: N questions, N answers, each with its own status line.
//
// Namespace reachability is checked HERE, once, before dispatch. The outer tool
// call cannot do it: `term` carries its target inside `cmd`, so the guard that
// reads `shell_id`/`session_id` arguments sees nothing to resolve and admits
// everything. Directives that reach a handler still get the full admission (the
// handler path runs checkAdmission again); the ones that read a manager directly
// — info, ps, span — would otherwise be reachable across namespaces, which is
// exactly the isolation this feature promises.
func (s *Server) runDirective(ctx context.Context, stmt directive.Statement, win termArgs) []envelope.Slot {
	if bad := s.refuseForeignTarget(ctx, stmt); bad != nil {
		return []envelope.Slot{*bad}
	}
	switch stmt.Op {
	case "run":
		return s.directiveRun(ctx, stmt, win)
	case "type":
		return s.directiveType(ctx, stmt, win)
	case "key":
		return s.directiveKey(ctx, stmt, win)
	case "read":
		return s.directiveRead(ctx, stmt, win)
	case "pipe":
		return s.directiveOpen(ctx, stmt, win, false)
	case "pty":
		return s.directiveOpen(ctx, stmt, win, true)
	case "ps":
		return []envelope.Slot{s.directivePS(ctx, stmt, win)}
	case "info":
		return []envelope.Slot{s.directiveInfo(ctx, stmt, win)}
	case "resize":
		return []envelope.Slot{s.directiveResize(ctx, stmt, win)}
	case "close":
		return []envelope.Slot{s.directiveClose(ctx, stmt, win)}
	case "rm":
		return []envelope.Slot{s.directiveRemove(ctx, stmt, win)}
	case "span":
		return []envelope.Slot{s.directiveSpan(ctx, stmt, win)}
	case "reader":
		return []envelope.Slot{s.directiveReader(ctx, stmt, win)}
	case "watch":
		return []envelope.Slot{s.directiveWatch(ctx, stmt, win)}
	case "notify":
		return []envelope.Slot{s.directiveNotify(ctx, stmt, win)}
	case "detect":
		return []envelope.Slot{s.directiveDetect(ctx, stmt, win)}
	case "fwd":
		return []envelope.Slot{s.directiveForward(ctx, stmt, win)}
	case "file":
		return []envelope.Slot{s.directiveFile(ctx, stmt, win)}
	case "profile":
		return []envelope.Slot{s.directiveProfile(ctx, stmt, win)}
	case "ext":
		return []envelope.Slot{s.directiveExtension(ctx, stmt, win)}
	case "mcp":
		return []envelope.Slot{s.directiveMCP(ctx, stmt, win)}
	case "alias":
		return []envelope.Slot{s.directiveAlias(ctx, stmt, win)}
	case "help":
		return []envelope.Slot{s.directiveHelp(stmt, win)}
	}
	return []envelope.Slot{{
		Code:    400,
		Headers: map[string]string{"Error-Code": "unknown_directive"},
		Body:    "unknown directive: " + stmt.Op,
	}}
}

// refuseForeignTarget applies namespace isolation to every directive, including
// the ones that never reach a handler.
//
// A batch is checked entry by entry and refused as a whole: running part of it
// against somebody else's session would make the refusal partial and the outcome
// unknowable.
func (s *Server) refuseForeignTarget(ctx context.Context, stmt directive.Statement) *envelope.Slot {
	callerNS := callerNamespace(ctx)
	if callerNS == "" || stmt.Target == "" {
		return nil // the operator, or a directive that names nothing
	}
	ids, _ := splitBatchIDs(stmt.Target)
	for _, id := range ids {
		if id == "" {
			continue
		}
		single := stmt
		single.Target = id
		sess := s.sessionForStatement(single)
		if sess == nil || ns.Reachable(sess.Namespace, callerNS) {
			continue
		}
		slot := failureSlot(&toolFailure{code: CodeForbidden, msg: ns.ForbiddenForeign(callerNS)})
		slot.Headers["Path"] = stmt.Path
		return &slot
	}
	return nil
}

// sessionForStatement resolves the session a directive names, for the admission
// check. A handle may be a session id, a shell id, or a locator — the same
// spellings the tool surface accepts, because a second spelling rule would be a
// second place to get it wrong.
func (s *Server) sessionForStatement(stmt directive.Statement) *session.Session {
	if stmt.Target == "" || s.sessMgr == nil {
		return nil
	}
	guard := toolGuard{server: s}
	if sess := guard.sessionForSessionID(stmt.Target); sess != nil {
		return sess
	}
	return guard.sessionForShellID(stmt.Target)
}

// The directives themselves.
//
// Each one translates to the handler that already implements it and re-renders
// the reply. Nothing below knows how to write a file, open a port or read a
// buffer; that is what keeps the single entry a re-rendering of the surface
// instead of a parallel implementation of it.

// directiveRun is one command line in one call: type the payload, press enter,
// read the window — and hand back the slot.
//
// The completion rule differs by mode, and that difference is the whole reason
// `run` exists: on a pipe channel the process exiting IS completion, so it
// returns the moment the channel closes; on a pty there is no such signal, so it
// waits out the caller's time window and returns what the terminal produced.
func (s *Server) directiveRun(ctx context.Context, stmt directive.Statement, win termArgs) []envelope.Slot {
	if ids, isBatch := splitBatchIDs(stmt.Target); isBatch {
		slots := make([]envelope.Slot, 0, len(ids))
		for _, id := range ids {
			single := stmt
			single.Target = id
			slots = append(slots, s.runOne(ctx, single, win))
		}
		return slots
	}
	return []envelope.Slot{s.runOne(ctx, stmt, win)}
}

// runOne executes one `run` against one handle.
func (s *Server) runOne(ctx context.Context, stmt directive.Statement, win termArgs) envelope.Slot {
	// The cursor is captured BEFORE the bytes go out: the window must start where
	// this command's output begins, or a previous command's unread tail would be
	// delivered as if it belonged to this one.
	start, ok := s.outputSizeFor(stmt.Target)
	if !ok {
		return s.slotFromFailure(ctx, stmt, s.probeShell(stmt.Target))
	}

	if _, fail := s.callTool(ctx, "shell_input", argsOf("shell_id", stmt.Target, "text", stmt.Payload)); fail != nil {
		return s.slotFromFailure(ctx, stmt, fail)
	}
	keyRes, fail := s.callTool(ctx, "shell_key", argsOf("shell_id", stmt.Target, "key", "enter"))
	if fail != nil {
		return s.slotFromFailure(ctx, stmt, fail)
	}
	// A held command line: review mode staged the text and queued it on the enter.
	// The reply is the review shape, not a read — there is nothing to read yet.
	if held := reviewPendingFrom(keyRes); held != nil {
		return envelope.Slot{
			Code:    202,
			Headers: map[string]string{"Path": stmt.Path},
			Body:    *held,
		}
	}

	return s.readSlotAfter(ctx, stmt, win, start)
}

// probeShell resolves a handle and reports why it cannot be used, so a `run`
// against a missing handle fails before it types anything.
func (s *Server) probeShell(handle string) *toolFailure {
	if _, bad := s.requireShell(handle); bad != nil {
		return failureFromResult(bad)
	}
	return nil
}

// slotFromFailure renders a handler failure for a directive.
func (s *Server) slotFromFailure(ctx context.Context, stmt directive.Statement, f *toolFailure) envelope.Slot {
	slot := failureSlot(f)
	if slot.Headers == nil {
		slot.Headers = map[string]string{}
	}
	slot.Headers["Path"] = stmt.Path
	return slot
}

// directiveType writes bytes without pressing enter.
func (s *Server) directiveType(ctx context.Context, stmt directive.Statement, win termArgs) []envelope.Slot {
	res, fail := s.callTool(ctx, "shell_input", argsOf("shell_id", stmt.Target, "text", stmt.Payload))
	if fail != nil {
		return []envelope.Slot{s.slotFromFailure(ctx, stmt, fail)}
	}
	if held := reviewPendingFrom(res); held != nil {
		return []envelope.Slot{{Code: 202, Headers: map[string]string{"Path": stmt.Path}, Body: *held}}
	}
	headers := map[string]string{"Path": stmt.Path}
	s.addUsage(&termReply{win: win}, stmt, headers)
	return []envelope.Slot{{Code: 204, Headers: headers}}
}

// directiveKey presses a named key.
func (s *Server) directiveKey(ctx context.Context, stmt directive.Statement, win termArgs) []envelope.Slot {
	if len(stmt.Args) == 0 {
		return []envelope.Slot{{Code: 400, Headers: map[string]string{"Error-Code": "invalid_argument", "Path": stmt.Path},
			Body: "key needs a key name, e.g. `key a1f3:1 enter`"}}
	}
	args := argsOf("shell_id", stmt.Target, "key", stmt.Args[0])
	if repeat := atoiFlag(stmt, "repeat", 1); repeat > 1 {
		args["repeat"] = float64(repeat)
	}
	res, fail := s.callTool(ctx, "shell_key", args)
	if fail != nil {
		return []envelope.Slot{s.slotFromFailure(ctx, stmt, fail)}
	}
	if held := reviewPendingFrom(res); held != nil {
		return []envelope.Slot{{Code: 202, Headers: map[string]string{"Path": stmt.Path}, Body: *held}}
	}
	return []envelope.Slot{{Code: 204, Headers: map[string]string{"Path": stmt.Path}}}
}

// directiveRead reads a window of a stream, honouring the three axes.
//
// A comma-separated target reads each channel independently and returns one slot
// per entry, which is the same batch shape input already had — a fleet is one
// question with N answers, not N round trips.
func (s *Server) directiveRead(ctx context.Context, stmt directive.Statement, win termArgs) []envelope.Slot {
	if ids, isBatch := splitBatchIDs(stmt.Target); isBatch {
		slots := make([]envelope.Slot, 0, len(ids))
		for _, id := range ids {
			single := stmt
			single.Target = id
			slots = append(slots, s.directiveReadOne(ctx, single, win))
		}
		return slots
	}
	return []envelope.Slot{s.directiveReadOne(ctx, stmt, win)}
}

// directiveReadOne reads one channel.
func (s *Server) directiveReadOne(ctx context.Context, stmt directive.Statement, win termArgs) envelope.Slot {
	// bytes_limit 0 means "hand back the handle, consume nothing". readSlotAfter
	// honours it; this path has to as well, or the one directive whose whole point
	// is reading would be the one place the caller cannot decline to read.
	// The offset is the stream's current end, so the reply still says where the
	// next read starts.
	if win.BytesLimit == 0 {
		start, _ := s.outputSizeFor(stmt.Target)
		return envelope.Slot{
			Code:    204,
			Headers: map[string]string{"Path": stmt.Path, "Content-Range": envelope.UnsatisfiedRange(start)},
		}
	}
	args := argsOf("shell_id", stmt.Target)
	if v, ok := stmt.Flags["cursor"]; ok {
		args["offset"] = flagNumber(v)
	}
	if v, ok := stmt.Flags["from-line"]; ok {
		args["from_line"] = flagNumber(v)
	}
	if v, ok := stmt.Flags["tail"]; ok {
		args["tail_lines"] = flagNumber(v)
	}
	if v, ok := stmt.Flags["stream"]; ok {
		args["stream"] = v
	}
	if boolFlag(stmt, "raw") {
		args["strip_ansi"] = false
	}
	return s.readSlot(ctx, stmt, win, args)
}

// readSlotAfter reads the window a command produced, starting where it was
// submitted.
//
// Completion differs by mode:
//
//   - A PIPE channel's completion signal is its process exiting. When the program
//     the channel runs is what the caller just ran, waiting IS the answer: the
//     channel closes and the exit code arrives with the bytes.
//   - A PTY channel has no such signal — a shell does not report that a command
//     finished. There the caller's time window is the window: wait it out, then
//     read what the terminal produced.
//
// `timeout` bounds both and defaults to two seconds.
func (s *Server) readSlotAfter(ctx context.Context, stmt directive.Statement, win termArgs, start int64) envelope.Slot {
	args := argsOf("shell_id", stmt.Target, "offset", float64(start))
	// A byte limit of 0 means "hand back the handle, consume nothing": the
	// directive must not even read, or a JSON-RPC frame destined for `mcp recv`
	// would be eaten by the envelope.
	if win.BytesLimit == 0 {
		return envelope.Slot{
			Code:    204,
			Headers: map[string]string{"Path": stmt.Path, "Content-Range": envelope.UnsatisfiedRange(start)},
		}
	}
	if wait := s.completionWait(stmt.Target); wait {
		s.awaitExit(ctx, stmt.Target, win)
	} else if s.ptyRunning(stmt.Target) {
		s.awaitWindow(ctx, win)
	}
	return s.readSlot(ctx, stmt, win, args)
}

// ptyRunning reports whether the handle is a live pty channel. The handle may
// be a shell id OR a session id naming its primary channel, so it resolves
// through the same door every other directive uses.
func (s *Server) ptyRunning(handle string) bool {
	cs, bad := s.requireShell(handle)
	if bad != nil {
		return false
	}
	info := cs.Info()
	return info.Mode == api.ModePTY && info.Status == api.SessionRunning
}

// awaitWindow waits out the caller's time window. A pty has no completion
// signal, so the window is sampled rather than ended early; timeout=0 means a
// snapshot, not a wait.
func (s *Server) awaitWindow(ctx context.Context, win termArgs) {
	limit := time.Duration(win.TimeoutSeconds * float64(time.Second))
	if limit <= 0 {
		return
	}
	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// completionWait reports whether this channel's completion is observable, which
// is true exactly for a pipe channel that has not exited yet.
func (s *Server) completionWait(handle string) bool {
	cs, bad := s.requireShell(handle)
	if bad != nil {
		return false
	}
	info := cs.Info()
	return info.Mode == api.SessionMode("pipe") && info.Status == api.SessionRunning
}

// awaitExit waits for a pipe channel's process to exit, bounded by the caller's
// timeout. It returns as soon as the channel closes — that is the whole point of
// the pipe half of the completion rule.
func (s *Server) awaitExit(ctx context.Context, handle string, win termArgs) {
	limit := time.Duration(win.TimeoutSeconds * float64(time.Second))
	if limit <= 0 {
		return
	}
	deadline := time.After(limit)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-deadline:
			return
		case <-ticker.C:
			cs, bad := s.requireShell(handle)
			if bad != nil || cs.Info().Status != api.SessionRunning {
				return
			}
		}
	}
}

// readSlot performs one read and renders it. This is where the three axes are
// applied for every directive that returns bytes.
func (s *Server) readSlot(ctx context.Context, stmt directive.Statement, win termArgs, args map[string]any) envelope.Slot {
	args["max_bytes"] = float64(win.BytesLimit)
	args["max_lines"] = float64(win.LinesLimit)
	// A pipe channel that has not exited yet must not block for the whole timeout
	// on a read that has nothing new; a pty read waits for the quiet window
	// instead. Both are upper bounds, so the smaller of the two wins.
	args["timeout"] = readTimeout(win)

	res, fail := s.callTool(ctx, "shell_output", args)
	if fail != nil {
		return s.slotFromFailure(ctx, stmt, fail)
	}
	doc, ok := toolResultMap(res)
	if !ok {
		return s.slotFromFailure(ctx, stmt, &toolFailure{code: CodeOperationFailed, msg: toolResultText(res)})
	}
	return s.windowSlot(stmt, win, doc)
}

// windowSlot renders a shell_output document as an envelope slot.
//
// Every byte fact comes from the handler's own reply: the offsets, the total, the
// session status and the channel's exit code. The envelope adds the HTTP shape and
// nothing else — no second read, no second source of truth.
func (s *Server) windowSlot(stmt directive.Statement, win termArgs, doc map[string]any) envelope.Slot {
	start := int64(getFloatAny(doc, "start_offset", 0))
	end := int64(getFloatAny(doc, "end_offset", 0))
	total := int64(getFloatAny(doc, "total_bytes", 0))
	body := getString(doc, "output", "")
	headers := map[string]string{"Path": stmt.Path}

	// The three axes, applied to what came back. The handler already truncated to
	// bytes_limit/lines_limit, so a short body means the window ended; a body that
	// reaches a limit while more exists is a partial read.
	truncated := false
	if win.BytesLimit > 0 && len(body) > win.BytesLimit {
		body = foldOrTruncate(body, win.BytesLimit, win.OnBytesOverflow)
		truncated = true
	}
	if win.LinesLimit > 0 {
		if lines := strings.Count(body, "\n"); lines > win.LinesLimit {
			body = foldOrTruncateLines(body, win.LinesLimit, win.OnLinesOverflow)
			truncated = true
		}
	}

	code := 200
	hasBytes := len(body) > 0 || end > start
	switch {
	case hasBytes && (truncated || boolAny(doc, "has_more")):
		code = 206
	case !hasBytes:
		code = 204
	}
	if total >= 0 && (code == 206 || code == 204) {
		if code == 204 {
			headers["Content-Range"] = envelope.UnsatisfiedRange(total)
		} else {
			headers["Content-Range"] = envelope.ContentRange(start, end-1, total)
		}
	} else if code == 200 && total > 0 {
		headers["Content-Range"] = envelope.ContentRange(start, end-1, total)
	}

	// Status and Exit-Code only when they are NEWS: a channel that has closed shows
	// its exit state once and stays that way, so every later read of it repeats the
	// same two lines consistently. "still running" is what an ordinary reply would
	// say, which is exactly what makes it noise — and a live channel has no exit
	// code to report, because a shell never tells the pipe outside it that a
	// command inside it finished.
	if status, exit, closed := s.channelExitState(stmt.Target); closed {
		headers["Status"] = status
		if exit != nil {
			headers["Exit-Code"] = fmt.Sprint(*exit)
			// A channel terminal state that is a failure makes every reply about it a
			// failure: the status code is a function of the channel, not of who
			// looked. That is what makes isError a global signal rather than a
			// per-call accident.
			if code < 400 && *exit != 0 {
				code = 500
				headers["Error-Code"] = "command_failed"
			}
		}
	}
	s.addUsage(s.currentReply, stmt, headers)
	return envelope.Slot{Code: code, Headers: headers, Body: body}
}

// channelExitState reports a handle's terminal state.
//
// closed is false while any part of the channel is still running, which is the
// common case and the one that must stay silent.
func (s *Server) channelExitState(handle string) (status string, exit *int, closed bool) {
	if s.sessMgr == nil {
		return "", nil, false
	}
	// A handle is a raw shell id, a session id, a locator naming either, or the
	// short `sid:N` that ps prints — every spelling locator.SplitHandle knows,
	// because a handle the caller had to copy out of a reply must not be silent
	// here: returning "still running" for a channel this simply failed to find is
	// how a dead channel keeps reporting itself alive.
	h := locator.SplitHandle(handle)
	if h.Index > 0 {
		// A channel was named explicitly, so that channel IS the question — the
		// session-level fallbacks below would answer about a different one.
		if sess := s.sessMgr.Get(h.SessionID); sess != nil {
			if cs, err := s.shellFromIndex(sess, h.Index); err == nil && cs != nil {
				info := cs.Info()
				if info.Status == "exited" || info.Status == "error" {
					return string(info.Status), info.ExitCode, true
				}
			}
		}
		return "", nil, false
	}
	if cs := s.sessMgr.GetChildShell(handle); cs != nil {
		info := cs.Info()
		if info.Status == "exited" || info.Status == "error" {
			return string(info.Status), info.ExitCode, true
		}
		return "", nil, false
	}
	if sess := s.sessMgr.Get(handle); sess != nil {
		info := sess.Info()
		if info.Status == "exited" || info.Status == "error" {
			return string(info.Status), info.ExitCode, true
		}
		// The handle named the session, so the primary channel is the object the
		// caller means: a one-channel session is the ordinary case, and its exit is
		// the session's.
		if cs := sess.PrimaryShell(); cs != nil {
			sinfo := cs.Info()
			if sinfo.Status == "exited" || sinfo.Status == "error" {
				return string(sinfo.Status), sinfo.ExitCode, true
			}
		}
	}
	return "", nil, false
}

// currentReply is the reply currently being assembled, so a slot can consult the
// learn_mode de-duplication. It is set per statement by runDirective's caller.
func (s *Server) addUsage(t *termReply, stmt directive.Statement, headers map[string]string) {
	// The de-duplication lives on the reply; a nil receiver means "no learn mode".
	if t == nil || !t.win.LearnMode {
		return
	}
	t.withUsage(stmt, headers)
}

// foldOrTruncate applies an overflow policy to a byte-truncated body.
func foldOrTruncate(body string, limit int, policy string) string {
	if policy == "fold" {
		return ""
	}
	return body[:limit]
}

// foldOrTruncateLines applies an overflow policy to a line-truncated body.
func foldOrTruncateLines(body string, limit int, policy string) string {
	if policy == "fold" {
		return ""
	}
	var b strings.Builder
	count := 0
	for _, line := range strings.SplitAfter(body, "\n") {
		if count == limit {
			break
		}
		b.WriteString(line)
		if strings.HasSuffix(line, "\n") {
			count++
		}
	}
	return b.String()
}

// readTimeout is the blocking wait a read gets: the caller's timeout, capped so a
// non-blocking read (timeout 0) really is non-blocking.
func readTimeout(win termArgs) float64 {
	if win.TimeoutSeconds <= 0 {
		return 0
	}
	if win.TimeoutSeconds > 60 {
		return 60
	}
	return win.TimeoutSeconds
}

// outputSizeFor reads a handle's current stream length, which is where a command's
// own output begins.
func (s *Server) outputSizeFor(handle string) (int64, bool) {
	sess, bad := s.requireShell(handle)
	if bad != nil {
		return 0, false
	}
	size, err := s.sessMgr.OutputSize(sess.ParentSessionID(), sess.ID)
	if err != nil {
		return 0, false
	}
	return size, true
}

// reviewPendingFrom extracts the review-pending message from a handler reply, or
// nil when the call ran.
func reviewPendingFrom(res *mcpgo.CallToolResult) *string {
	doc, ok := toolResultMap(res)
	if !ok {
		return nil
	}
	if pending, _ := doc["review_pending"].(bool); !pending {
		return nil
	}
	msg := getString(doc, "message", "Submitted for review.")
	return &msg
}

// toolResultMap parses a handler reply's JSON body.
func toolResultMap(res *mcpgo.CallToolResult) (map[string]any, bool) {
	if res == nil || len(res.Content) == 0 {
		return nil, false
	}
	text := toolResultText(res)
	var m map[string]any
	if err := json.Unmarshal([]byte(text), &m); err != nil {
		return nil, false
	}
	return m, true
}

// failureFromResult turns a handler's error result into a toolFailure.
// failureFromResult translates a failed tool result into envelope terms.
//
// It goes through extractErrorInfo rather than decoding Content directly, because
// the error payload is a JSON envelope whose `error` field is what a caller
// reads: decoding the raw content would return the envelope as if it were text,
// and — worse — the dedicated error_code would be lost, turning every refusal
// into a generic operation_failed (a 500) instead of the 403 it is.
func failureFromResult(res *mcpgo.CallToolResult) *toolFailure {
	if res == nil {
		return &toolFailure{code: CodeOperationFailed, msg: "the operation failed without a reason"}
	}
	code, msg := extractErrorInfo(res)
	if code == "" {
		code = CodeOperationFailed
	}
	if msg == "" {
		msg = toolResultText(res)
	}
	return &toolFailure{code: code, msg: msg}
}

// getFloatAny reads a number from a JSON document.
func getFloatAny(m map[string]any, key string, def float64) float64 {
	if v, ok := m[key]; ok {
		if f, ok := v.(float64); ok {
			return f
		}
	}
	return def
}

// boolAny reads a boolean from a JSON document.
func boolAny(m map[string]any, key string) bool {
	v, _ := m[key].(bool)
	return v
}

// flagNumber renders a numeric flag for the JSON arguments of a handler.
func flagNumber(raw string) float64 {
	var f float64
	if _, err := fmt.Sscanf(raw, "%g", &f); err != nil {
		return 0
	}
	return f
}
