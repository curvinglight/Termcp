package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
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
	// A CHANNEL DIRECTIVE TAKES A CHANNEL, and the check happens here as well as at
	// parse time. Parse-time covers the ordinary path (the target word is visible in
	// the statement); this one covers the paths the parser cannot see — an alias
	// whose expansion supplies the target, and a target that reached dispatch by
	// some route other than parseStatement. Two enforcement points, one rule, so a
	// future entry cannot reintroduce the silent primaryShell fallback.
	if op, ok := directive.Lookup(stmt.Op); ok && op.TargetKind == directive.TargetChannel {
		if stmt.Target != "" && !directive.NamesAChannel(stmt.Target) {
			return []envelope.Slot{badRequest(stmt,
				directive.ChannelTargetProblem(op, stmt.Target)+"\n"+op.Usage)}
		}
	}
	switch stmt.Op {
	// --- verbs whose target is already the node they act on ---
	//
	// TargetChain normalised `a1f3 1` to `a1f3:1` at parse time, so these four see
	// one spelling and need no kind handling at all.
	case "run":
		return s.directiveRun(ctx, stmt, win)
	case "input":
		return s.directiveType(ctx, stmt, win)
	case "key":
		return s.directiveKey(ctx, stmt, win)
	case "resize":
		return []envelope.Slot{s.directiveResize(ctx, stmt, win)}
	case "span":
		return []envelope.Slot{s.directiveSpan(ctx, stmt, win)}
	case "reader":
		return []envelope.Slot{s.directiveReader(ctx, stmt, win)}
	case "get":
		if stmt.Kind == "host" {
			// `get host <name>` reads one entry back. Without it a caller could create,
			// edit and delete a host but never see what it holds, so a partial edit had
			// to be done blind.
			return []envelope.Slot{s.directiveHost(ctx, withFlags(stmt, map[string]string{
				"action": "get", "name": stmt.Key,
			}), win)}
		}
		// A PEER'S OWN STATE IS READ HERE, not from the SSH side. `get a1f3 cdp`
		// describes the browser (attached pages, frame cursor); `get a1f3 cdp:1` names
		// one page; `get a1f3 cdp protocol Page` reads the live signature the browser
		// publishes about itself. All three are answered from the peer, so they are
		// routed before the session/file branches that would otherwise refuse them.
		if isPeerKind(stmt.Kind) {
			h, index, bad := s.peerOf(stmt)
			if bad != nil {
				return []envelope.Slot{*bad}
			}
			// `protocol` IS A CHILD OF THE CONNECTION, so it arrives in one of two
			// positions depending on whether the address was qualified:
			//   get internal cdp protocol      -> Key="protocol"
			//   get internal cdp:1 protocol    -> Key="1", Args[0]="protocol"
			// Both mean the same thing; the second is what a caller copies out of a reply.
			if stmt.Key == "protocol" || (len(stmt.Args) > 0 && stmt.Args[0] == "protocol") {
				domain := ""
				if stmt.Key == "protocol" {
					if len(stmt.Args) > 0 {
						domain = stmt.Args[0]
					}
				} else if len(stmt.Args) > 1 {
					domain = stmt.Args[1]
				}
				return []envelope.Slot{s.cdpProtocol(ctx, stmt, h, domain)}
			}
			return []envelope.Slot{s.cdpDescribe(stmt, h, index)}
		}
		if stmt.Kind == "file" {
			// --op names a SUB-ACTION; the sfTP layer groups those under an action
			// (perm, link, fs) and dispatches again inside the group. The group is
			// derivable from the op, so the caller never has to know it — and asking for
			// `--op stat` must not reach the fs group, where the answer is "action must
			// be truncate, realpath, or statvfs".
			op, _ := stmt.Flag("op")
			if op == "" {
				return []envelope.Slot{s.directiveFile(ctx, withFlags(stmt, map[string]string{
					"action": "stat", "remote-path": stmt.Key,
				}), win)}
			}
			action, ok := fileGroupForOp(op)
			if !ok {
				return []envelope.Slot{badRequest(stmt, "get --op does not know "+quoteOrDash(op)+
					". It takes stat, realpath, readlink, getwd, urls, or one of: chmod, chown, chtimes, truncate, statvfs.")}
			}
			return []envelope.Slot{s.directiveFile(ctx, withFlags(stmt, map[string]string{
				"action": action, "op": op, "remote-path": stmt.Key,
			}), win)}
		}
		return []envelope.Slot{s.directiveInfo(ctx, stmt, win)}
	case "notify":
		return []envelope.Slot{s.directiveNotify(ctx, stmt, win)}
	case "help":
		return []envelope.Slot{s.directiveHelp(stmt, win)}

	// --- connect: a new SSH link ---
	case "connect":
		return s.directiveOpen(ctx, stmt, win, wantPTY(stmt))

	// --- open: a child of an existing session, or a peer through it ---
	case "open":
		switch stmt.Kind {
		case "forward":
			// THE DIRECTION IS A FLAG, and local is the default because it is the one a
			// caller reaches for when they say "forward this port" without qualifying it.
			// Without this, every forward was local: `--dynamic` and `--remote` were not
			// flags of `open` at all, so SOCKS5 and remote forwards became unreachable in
			// the rewrite while the detail page still described all three.
			action := "local"
			if boolFlag(stmt, "dynamic") {
				action = "dynamic"
			} else if boolFlag(stmt, "remote") {
				action = "remote"
			}
			return []envelope.Slot{s.directiveForward(ctx, withFlags(stmt, map[string]string{
				"action": action,
			}), win)}
		case "mcp", "cdp", "jrpc":
			// ATTACH RIDES ON OPEN, because attaching a page IS opening a child of the
			// browser — the same verb that opened the browser. The target id is a flag
			// because the key position already names the URL.
			if target, ok := stmt.Flag("target"); ok {
				h, _, bad := s.peerOf(stmt)
				if bad != nil {
					return []envelope.Slot{*bad}
				}
				return []envelope.Slot{s.cdpAttach(ctx, stmt, h, target)}
			}
			return s.openPeer(ctx, stmt, win, stmt.Kind)
		default:
			return s.directiveOpen(ctx, stmt, win, wantPTY(stmt))
		}

	// --- ls: every collection is one verb ---
	case "ls":
		return s.directiveLS(ctx, stmt, win)

	// --- rm: a live node, or saved configuration ---
	case "rm":
		switch {
		case stmt.Kind == "file":
			return []envelope.Slot{s.directiveFile(ctx, withFlags(stmt, map[string]string{
				"action":      "delete",
				"remote-path": stmt.Key,
			}), win)}
		case stmt.Kind == "host":
			return []envelope.Slot{s.directiveHost(ctx, withFlags(stmt, map[string]string{
				"action": "delete", "name": stmt.Key,
			}), win)}
		case stmt.Kind == "ext":
			return []envelope.Slot{s.directiveExtension(ctx, withFlags(stmt, map[string]string{
				"action": "delete", "name": stmt.Key,
			}), win)}
		case stmt.Kind == "alias":
			return []envelope.Slot{s.directiveAlias(ctx, aliasStatement(stmt, true), win)}
		default:
			return []envelope.Slot{s.directiveRemove(ctx, stmt, win)}
		}

	// --- close: a channel, a session, a tunnel or a peer ---
	//
	// A NAMED KIND NEVER FALLS THROUGH. `close <session> forward <id>` is a tunnel;
	// `close <session>` is the container. If the kind is one this verb does not
	// handle, the answer is a 400 — not "close whatever the target happened to be".
	// It fell through once: `close <session> mcp 1` parsed the kind away, found no
	// branch, and took the session down, so a caller closing a peer destroyed every
	// channel on the link. That is the same shape as #132 (`close <session> --force`
	// closing the primary channel): a qualifier read as decoration.
	case "close":
		switch stmt.Kind {
		case "forward":
			// THE ID IS THE NODE, so it arrives as the key rather than as a flag: the tree
			// says `close <session> forward <id>`, and asking for `--forward-id` here made
			// the same object addressable two ways — a positional key when opening it,
			// a flag when closing it. An explicit flag still wins, so a caller who wrote
			// it out is not punished.
			flags := map[string]string{"action": "close"}
			if stmt.Key != "" {
				flags["forward-id"] = stmt.Key
			}
			return []envelope.Slot{s.directiveForward(ctx, withFlags(stmt, flags), win)}
		case "cdp", "mcp", "jrpc":
			// `close a1f3 cdp:1` detaches one page; `close a1f3 cdp` closes the peer.
			h, index, bad := s.peerOf(stmt)
			if bad != nil {
				return []envelope.Slot{*bad}
			}
			if index != "" {
				return []envelope.Slot{s.cdpDetach(ctx, stmt, h, index)}
			}
			return []envelope.Slot{s.closePeer(stmt, h)}
		case "":
			if isPeerHandle(stmt.Target) {
				return s.peerChannel(ctx, stmt, win, "close", nil)
			}
			return []envelope.Slot{s.directiveClose(ctx, stmt, win)}
		default:
			return []envelope.Slot{badRequest(stmt, "close does not take a "+stmt.Kind+
				". A peer is closed by its address: `close <session> cdp`.")}
		}

	// --- read: a channel's new bytes, a file's content, or a peer's frames ---
	case "read":
		if stmt.Kind == "file" {
			return []envelope.Slot{s.directiveFile(ctx, withFlags(stmt, map[string]string{
				"action":      "read",
				"remote-path": stmt.Key,
			}), win)}
		}
		// A REGISTRY HANDLE IS ALWAYS A FRAME SOURCE; A SHELL IS BYTES UNLESS ASKED.
		//
		// A network peer has no byte stream to speak of, so `read` on one can only mean
		// "the next frame". A pipe channel is the opposite: it is usually a command's
		// output, and only sometimes a JSON-RPC server — so frames need the flag, or
		// every ordinary `read` of a command's output would be parsed as protocol.
		if isPeerKind(stmt.Kind) {
			h, index, bad := s.peerOf(stmt)
			if bad != nil {
				return []envelope.Slot{*bad}
			}
			// THE FRAME LOG HAS TWO AXES, and the flags pick one of them.
			//   --since  pages forward in TIME: what arrived after n, again and again.
			//   --at     pages inside ONE frame: that frame's bytes from b onward. A
			//            frame too big to inline is answered as a receipt naming its
			//            number, and this is the read that receipt tells you to make.
			// A plain read without either is the other question — the next frame off
			// the queue, once — which is what `send` pairs with.
			if at, ok := stmt.Flag("at"); ok {
				n, err := strconv.ParseInt(strings.TrimSpace(at), 10, 64)
				if err != nil || n < 1 {
					return []envelope.Slot{badRequest(stmt, "--at takes a frame number (the number a receipt reported as Frame), got "+strconv.Quote(at))}
				}
				offset := 0
				if raw, ok := stmt.Flag("offset"); ok {
					v, err := strconv.Atoi(strings.TrimSpace(raw))
					if err != nil || v < 0 {
						return []envelope.Slot{badRequest(stmt, "--offset takes a byte offset, got "+strconv.Quote(raw))}
					}
					offset = v
				}
				limit := 0
				if raw, ok := stmt.Flag("bytes"); ok {
					v, err := strconv.Atoi(strings.TrimSpace(raw))
					if err != nil || v <= 0 {
						return []envelope.Slot{badRequest(stmt, "--bytes takes a positive page size, got "+strconv.Quote(raw))}
					}
					limit = v
				}
				// One page per frame at a time, so the answer is a slice of a known
				// frame rather than an interleaving whose offsets mean nothing.
				return []envelope.Slot{s.pageFrame(stmt, h, n, offset, limit)}
			}
			if since, ok := stmt.Flag("since"); ok {
				n, err := parseSince(since)
				if err != nil {
					return []envelope.Slot{badRequest(stmt, err.Error())}
				}
				grep, _ := stmt.Flag("grep")
				// THE PAGE IS A FILTER, NOT A DIFFERENT STREAM. One socket carries every
				// attached page's frames interleaved, each tagged with its sessionId —
				// so `read <session> cdp:1 --since N` means "the ones that belong to this
				// page". Without the filter the index would be accepted and ignored,
				// which reads like a scoped answer and is not.
				return []envelope.Slot{s.cdpEvents(ctx, stmt, h, index, n, grep, win)}
			}
			return s.peerChannel(ctx, stmt, win, "read", nil)
		}
		if boolFlag(stmt, "frame") {
			return s.peerChannel(ctx, stmt, win, "read", nil)
		}
		return s.directiveRead(ctx, stmt, win)

	// --- write: a file's content ---
	//
	// THE FRAME FORM LIVES ON call. Both are "put bytes somewhere", but a frame is a
	// payload (verbatim, quotes and all) and a file write is a set of flags
	// (--data/--mode/--offset), and one verb cannot take both: the parser has one
	// payload rule per directive, so whichever form it picked would be wrong for the
	// other. `call` already carries a verbatim payload, which is exactly what a frame
	// needs.
	case "write":
		if stmt.Kind == "file" {
			// --old-text makes it a replace: one write verb, and whether it edits in
			// place is a property of what was supplied, not a second word to learn.
			action := "write"
			extra := map[string]string{}
			if _, ok := stmt.Flag("old-text"); ok {
				action = "replace"
			} else if op, ok := stmt.Flag("op"); ok {
				// --op NAMES THE SUB-ACTION and the group comes from the same table `get`
				// uses, so the two verbs cannot drift into disagreeing about what a chmod
				// is. --mode alone still means chmod: a mode with no content can only be
				// a permission change.
				group, known := fileGroupForOp(op)
				if !known {
					return []envelope.Slot{badRequest(stmt, "write --op does not know "+quoteOrDash(op))}
				}
				action, extra["op"] = group, op
			} else if _, ok := stmt.Flag("mode"); ok {
				if _, hasData := stmt.Flag("data"); !hasData {
					if _, hasFile := stmt.Flag("local-path"); !hasFile {
						action, extra["op"] = "perm", "chmod"
					}
				}
			}
			flags := map[string]string{"action": action, "remote-path": stmt.Key}
			for k, v := range extra {
				flags[k] = v
			}
			return []envelope.Slot{s.directiveFile(ctx, withFlags(stmt, flags), win)}
		}
		return []envelope.Slot{badRequest(stmt, "write needs a kind: write <session> file <path>")}

	// --- call: one request, one reply ---
	//
	// THE HANDLE MAY BE A PEER OR A SHELL. A network peer is opened with
	// `open <session> mcp <url>` and gets a registry handle; a stdio peer is a pipe
	// channel already running the server, addressed by its channel handle. Both are
	// "something that speaks JSON-RPC", and the caller should not have to know which
	// kind of pipe it is talking through — so one verb covers both.
	case "call":
		return s.peerChannel(ctx, stmt, win, "call", nil)

	// --- ls on a peer lists what it holds, not what the SSH side holds ---
	//
	// It is the same question `ls <session>` asks (`what hangs off this node`), so
	// it is the same verb. A browser's children are tabs; a jrpc peer has none.
	// (Handled in directiveLS, which every `ls` reaches.)

	// --- configuration, which outlives every session ---
	case "create", "edit":
		act := "create"
		if stmt.Op == "edit" {
			act = "edit"
		}
		switch stmt.Kind {
		case "file":
			// Only create reaches here: a file is made with mkdir, or as a symlink when
			// --target names what it points at.
			//
			// THE LINK GROUP NEEDS AN --op AS WELL AS THE ACTION, and the caller should not
			// have to supply one: --target already says this is a symlink, so requiring
			// `--op symlink` too would be a second way to say the same thing. Note the
			// argument order flips going in: the node's key is the LINK path and --target
			// is what it points at, while the handler calls those link_path and target.
			action := "mkdir"
			flags := map[string]string{"action": action, "remote-path": stmt.Key}
			if target, ok := stmt.Flag("target"); ok {
				// A HARD LINK IS THE SAME SENTENCE with --op link: the key is the new
				// name, --target is what it already exists as. Without this branch a hard
				// link had no spelling at all — `write --op link` wanted existing_path and
				// new_path, and neither is a flag of write.
				if op, _ := stmt.Flag("op"); op == "link" {
					flags["action"] = "link"
					flags["op"] = "link"
					flags["new-path"] = stmt.Key
					flags["existing-path"] = target
					return []envelope.Slot{s.directiveFile(ctx, withFlags(stmt, flags), win)}
				}
				flags["action"] = "link"
				flags["op"] = "symlink"
				flags["link-path"] = stmt.Key
				flags["target"] = target
			}
			return []envelope.Slot{s.directiveFile(ctx, withFlags(stmt, flags), win)}
		case "host":
			return []envelope.Slot{s.directiveHost(ctx, withFlags(stmt, map[string]string{
				"action": act, "name": stmt.Key,
			}), win)}
		case "ext":
			// `update` was the old name for the same write; ext kept it because the
			// config file calls the field `update`. One verb here, one word there.
			if act == "edit" {
				act = "update"
			}
			return []envelope.Slot{s.directiveExtension(ctx, withFlags(stmt, map[string]string{
				"action": act, "name": stmt.Key,
			}), win)}
		case "alias":
			return []envelope.Slot{s.directiveAlias(ctx, aliasStatement(stmt, false), win)}
		}
		usage := ""
		if op, ok := directive.Lookup(stmt.Op); ok {
			usage = "\n" + op.Usage
		}
		return []envelope.Slot{badRequest(stmt, stmt.Op+" needs a kind: "+stmt.Op+" host|ext|alias <name>"+usage)}
	}
	return []envelope.Slot{{
		Code:    400,
		Headers: map[string]string{"Error-Code": "unknown_directive"},
		Body:    "unknown directive: " + stmt.Op,
	}}
}

// withFlags returns a copy of stmt whose flag map carries the given values.
//
// THE ADAPTER IS THE POINT OF THIS LAYER. The handlers below were written against
// the old spelling (`file <session> --action read --remote-path x`) and their
// internals are not what changed: an SFTP read is an SFTP read. Rewriting them to
// read a different field would have been six hundred lines of shuffling with a
// fresh bug in each. So the surface is translated here, once, and the machinery
// keeps working.
//
// The copy is shallow except for the flag map: a statement is shared between the
// statements of one cmd, and mutating the caller's map would make `a; b` where a is
// translated leak its flags into b.
func withFlags(stmt directive.Statement, set map[string]string) directive.Statement {
	flags := make(map[string]*string, len(stmt.Flags)+len(set))
	for k, v := range stmt.Flags {
		flags[k] = v
	}
	for k, v := range set {
		if v == "" {
			continue
		}
		val := v
		flags[k] = &val
	}
	out := stmt
	out.Flags = flags
	return out
}

// noContent returns a 204 slot carrying the statement's Path header.
func noContent(stmt directive.Statement) envelope.Slot {
	return envelope.Slot{Code: 204, Headers: map[string]string{"Path": stmt.Path}}
}

// badRequest returns a 400 slot for an invalid argument or missing parameter.
func badRequest(stmt directive.Statement, body string) envelope.Slot {
	return envelope.Slot{
		Code:    400,
		Headers: map[string]string{"Path": stmt.Path, "Error-Code": CodeInvalidArgument},
		Body:    body,
	}
}

// badRequestNoPath is badRequest for the replies that predate a statement — the
// argument screen at the entrance, and a directive error raised before a Path
// exists. There is no Path header to attach, and inventing one would echo a
// position the caller never wrote.
func badRequestNoPath(body string) envelope.Slot {
	return envelope.Slot{
		Code:    400,
		Headers: map[string]string{"Error-Code": CodeInvalidArgument},
		Body:    body,
	}
}

// ptrSlot is the address of a slot, for the helpers whose caller must return a
// *envelope.Slot rather than a value.
func ptrSlot(s envelope.Slot) *envelope.Slot { return &s }

// notFoundSlot returns a 404 slot with the given error code.
func notFoundSlot(stmt directive.Statement, code, body string) envelope.Slot {
	return envelope.Slot{
		Code:    404,
		Headers: map[string]string{"Path": stmt.Path, "Error-Code": code},
		Body:    body,
	}
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
		// THE SAME ADVICE ON BOTH ROADS. A finished pipe can be refused here, before
		// anything is typed, or by the write below; only the second used to carry the
		// explanation, so a caller that arrived on the first got "process has exited"
		// with no way forward.
		return s.slotFromFailure(ctx, stmt, pipeClosedHint(stmt.Target, s.probeShell(stmt.Target), s))
	}

	if _, fail := s.callTool(ctx, "shell_input", argsOf("shell_id", stmt.Target, "text", stmt.Payload)); fail != nil {
		return s.slotFromFailure(ctx, stmt, pipeClosedHint(stmt.Target, fail, s))
	}
	keyRes, fail := s.callTool(ctx, "shell_key", argsOf("shell_id", stmt.Target, "key", "enter"))
	if fail != nil {
		return s.slotFromFailure(ctx, stmt, pipeClosedHint(stmt.Target, fail, s))
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

// pipeClosedHint turns a bare write failure into something a caller can act on.
//
// A pipe channel's process can exit while the session and the channel still report
// `running` — the status is updated when the exit is reaped, and the write can lose
// that race. What reaches the caller then is the OS error on a closed stdin, `EOF`
// or `process has exited, cannot send input`, which says nothing about the two facts
// that matter: this channel was a one-shot program, and its output is still there.
//
// The hint is only added for a pipe channel: on a pty the same error means something
// else (the shell died), and pointing at `read` would be wrong.
func pipeClosedHint(handle string, f *toolFailure, s *Server) *toolFailure {
	if f == nil {
		return f
	}
	msg := strings.TrimSpace(f.msg)
	closed := msg == "EOF" || strings.Contains(msg, "cannot send input") ||
		strings.Contains(msg, "file already closed") || strings.Contains(msg, "broken pipe")
	if !closed {
		return f
	}
	// The channel may already be refused by requireShell (its process exited and the
	// status caught up), which is the OTHER way this error arrives. Both roads lead to
	// the same advice, so a bad resolution is checked for the mode instead of
	// returning early: a pty whose shell died must NOT be told to `read` its exit
	// code, because a pty has no exit code to read.
	cs, bad := s.requireShell(handle)
	if cs == nil {
		return f
	}
	if bad != nil {
		if cs.Info().Mode != api.ModePipe {
			return f
		}
	} else if cs.Info().Mode != api.ModePipe {
		return f
	}
	return &toolFailure{
		code: CodeOperationFailed,
		msg: msg + "\nThis is a PIPE channel: its process has finished, so there is no stdin left to write to " +
			"and `run`/`input` cannot drive it. Its output is still readable — `read " + handle + "` returns it, " +
			"including the exit code. For an interactive session use `open <session>` instead (a pty).",
	}
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
		return []envelope.Slot{s.slotFromFailure(ctx, stmt, pipeClosedHint(stmt.Target, fail, s))}
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
		return []envelope.Slot{badRequest(stmt, "key needs a key name, e.g. `key a1f3:1 enter`")}
	}
	args := argsOf("shell_id", stmt.Target, "key", stmt.Args[0])
	repeat, err := atoiFlag(stmt, "repeat", 1)
	if err != nil {
		return []envelope.Slot{numericFlagProblem(stmt, "repeat", err)}
	}
	if repeat > 1 {
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
	// --grep takes its own path, because it changes what the window counts.
	// `lines_limit` normally bounds raw lines; with a pattern it bounds MATCHES,
	// and the filter therefore has to run before the window rather than after it.
	if _, ok := stmt.Flag("grep"); ok {
		return s.readGrep(ctx, stmt, win)
	}
	args := argsOf("shell_id", stmt.Target)
	if slot, bad := numericFlags(args, stmt,
		[2]string{"cursor", "offset"},
		[2]string{"from-line", "from_line"},
		[2]string{"tail", "tail_lines"},
	); bad {
		return slot
	}
	if v, ok := stmt.Flag("stream"); ok {
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

// pipeRunning reports whether the handle is a live PIPE channel — the only kind
// whose command has an observable end, and therefore the only kind that can be
// "still running" in a way worth reporting.
func (s *Server) pipeRunning(handle string) bool {
	cs, bad := s.requireShell(handle)
	if bad != nil {
		return false
	}
	info := cs.Info()
	return info.Mode == api.SessionMode("pipe") && info.Status == api.SessionRunning
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
	started := time.Now()
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
	return s.windowSlot(stmt, win, doc, time.Since(started))
}

// windowSlot renders a shell_output document as an envelope slot.
//
// Every byte fact comes from the handler's own reply: the offsets, the total, the
// session status and the channel's exit code. The envelope adds the HTTP shape and
// nothing else — no second read, no second source of truth.
func (s *Server) windowSlot(stmt directive.Statement, win termArgs, doc map[string]any, waited time.Duration) envelope.Slot {
	start := int64(getFloatAny(doc, "start_offset", 0))
	end := int64(getFloatAny(doc, "end_offset", 0))
	total := int64(getFloatAny(doc, "total_bytes", 0))
	body := getString(doc, "output", "")
	headers := map[string]string{"Path": stmt.Path}

	// THE HANDLER ALREADY APPLIED THE LIMITS, so this pass must decide from
	// has_more, not from the body's size.
	//
	// It used to test `len(body) > win.BytesLimit` and `lines > win.LinesLimit`,
	// which the handler had just made false: it returns at most bytes_limit bytes and
	// lines_limit lines, so the comparison never held and the fold policy never ran.
	// The observable result was that `on_lines_overflow: fold` and
	// `on_bytes_overflow: fold` returned the truncated window anyway — the parameter
	// was accepted, validated, and then ignored.
	//
	// has_more is the fact the handler DOES report: "the window ended before the
	// stream did". That is exactly the condition the fold policy is about.
	more := boolAny(doc, "has_more")
	truncated := false
	if more && win.BytesLimit > 0 && win.OnBytesOverflow == "fold" {
		body = ""
		truncated = true
	}
	if more && win.LinesLimit > 0 && win.OnLinesOverflow == "fold" {
		body = ""
		truncated = true
	}
	// A FOLD THAT DOES NOT SAY WHAT IT FOLDED IS INDISTINGUISHABLE FROM NOTHING THERE.
	//
	// `on_*_overflow: fold` is the right default for a caller that would rather have
	// a handle than a wall of bytes, but it used to answer 206 with an empty body and
	// a range — and an empty body is also what "no new output" looks like. The caller
	// had to trust that bytes existed somewhere it could not name.
	//
	// The Folded header names the gap: which byte span this reply withheld, and the
	// exact flag that fetches it. That is the difference between "folded on purpose"
	// and "nothing to show", and it costs one header line.
	if truncated && total >= start {
		headers["Folded"] = fmt.Sprintf("bytes %d-%d of %d; read --cursor %d", start, total-1, total, start)
	}

	code := 200
	hasBytes := len(body) > 0 || end > start
	switch {
	case hasBytes && (truncated || boolAny(doc, "has_more")):
		code = 206
	case !hasBytes:
		code = 204
	}

	// WAITED ANSWERS "DID YOU ACTUALLY WAIT OR DID YOU SNAP-FAIL?" (桩 5).
	//
	// A 204 that arrives in 0.03ms is a failure (the socket died, the reader broke,
	// the window never ran); a 204 that arrives in 2000ms is legitimate silence.
	// Both used to look identical. The header states the actual time elapsed so a
	// caller can distinguish "waited and nothing arrived" from "dropped out
	// instantly", which is the one difference that decides whether to retry.
	//
	// SAMPLED NAMES THE PTY CONTRACT (误读 3).
	//
	// An empty read of a pty is a sample of a live shell that outlives every command;
	// it does not mean the command exited and produced nothing. Only on 204: on a 200
	// the output speaks for itself, and on pipe the exit state is observable.
	if waited > 0 {
		headers["Waited"] = fmt.Sprintf("%dms", waited.Milliseconds())
	}
	if code == 204 && s.ptyRunning(stmt.Target) {
		headers["Sampled"] = "true"
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
	// "DID IT FINISH?" IS THE QUESTION A LONG CALL HAS, AND ABSENCE WAS THE ANSWER.
	//
	// A window that expires while the program is still running used to produce a
	// reply identical to one where the program finished immediately and printed
	// nothing: 204, no status, no hint. A caller polling for a 180-second nvme
	// self-test had no way to tell "still working" from "done, silent" and kept
	// re-reading a channel that was already finished.
	//
	// still_running is that fact, stated once. It is not an error — the command was
	// accepted and is running — so the code stays whatever the window said.
	if status, exit, closed := s.channelExitState(stmt.Target); closed {
		headers["Status"] = status
		if exit != nil {
			headers["Exit-Code"] = fmt.Sprint(*exit)
			// WHO ASKED DECIDES WHETHER A NON-ZERO EXIT IS THIS REPLY'S FAILURE.
			//
			// `run` reports the command it just sent, so a non-zero exit is news
			// about THIS call and 500 is right. `read` reports a transcript, and the
			// transcript is readable whether or not the program in it succeeded —
			// turning that into 500 made a successful read look broken, and dragged
			// every other statement in the same cmd down with it (isError is a
			// property of the whole call).
			//
			// The exit code stays in its header either way, and Error-Code still names
			// it, so a caller that wants to treat it as a failure has the fact. What
			// it no longer gets is a status that says the READ failed.
			if code < 400 && *exit != 0 {
				headers["Error-Code"] = "child_exit"
				if stmt.Op != "read" {
					code = 500
					headers["Error-Code"] = "command_failed"
				}
			}
		}
	} else if (stmt.Op == "run" || stmt.Op == "type") && s.pipeRunning(stmt.Target) {
		// PIPE ONLY, because only a pipe has a completion signal to miss.
		//
		// A pipe channel's run waits for its process to exit, so returning while it
		// is still running means the window expired mid-command: the caller's 180
		// second job is still going and the bytes so far are all there is. A pty
		// channel's run always waits its window and the shell outlives every command,
		// so the same flag would be permanently true and say nothing.
		//
		// NOT an Error-Code: nothing failed. The command was accepted and is running.
		headers["Still-Running"] = "true"
	}
	return envelope.Slot{Code: code, Headers: headers, Body: body}
}

// addUsage adds the directive's usage line to one slot when learn_mode asked for
// it.
//
// The de-duplication lives on the REPLY, not on the server: "the same directive
// once" is a fact about one answer, so two calls in flight must not be able to see
// each other's bookkeeping. That is why this takes the reply as an argument instead
// of reading a field off the Server — a shared field made every Term call race with
// every other, which is a bug this signature removes rather than locks around.
func (s *Server) addUsage(t *termReply, stmt directive.Statement, headers map[string]string) {
	if t == nil {
		return
	}
	t.withUsage(stmt, headers)
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
		// A session's exit is the session's; we do not look into its primary
		// shell. Every channel that needs its exit reported passes a channel handle
		// above (`sid:N`), which resolves on its own.
		return "", nil, false
	}
	return "", nil, false
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

// numericFlagProblem is the 400 for a numeric flag whose value does not parse.
//
// The flag name and the offending text are both in the message, because the caller
// wrote a command line and needs to see WHICH word was wrong — "invalid argument"
// on a fifty-flag directive is not actionable.
func numericFlagProblem(stmt directive.Statement, flag string, err error) envelope.Slot {
	return badRequest(stmt, "--"+flag+" "+err.Error())
}

// boolAny reports whether a document field is the literal true.
func boolAny(m map[string]any, key string) bool {
	v, _ := m[key].(bool)
	return v
}

// flagNumber parses a numeric flag for a handler's JSON arguments.//
// IT DOES NOT FALL BACK. The previous version returned 0 for anything it could not
// parse, and 0 is a legal value nearly everywhere it lands: `resize abc xyz` became
// rows=0/cols=0, `--tail abc` became "no tail", `--grace abc` became "no grace".
// Each of those then flowed into a handler whose own default (24x80, the default
// window, the default grace) turned the caller's typo into a DIFFERENT successful
// operation, reported as 2xx. The fallback was not protecting anything — it was
// deciding, silently, what the caller must have meant.
//
// So the failure is reported here, where the flag is still a flag and the message
// can still name it. The caller of this function is expected to turn the error into
// a 400 for the statement; see numericFlags.
func flagNumber(raw string) (float64, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0, fmt.Errorf("expected a number, got an empty value")
	}
	f, err := strconv.ParseFloat(trimmed, 64)
	if err != nil {
		return 0, fmt.Errorf("expected a number, got %q", trimmed)
	}
	// NaN/Inf parse successfully but are never a meaningful port, size or offset,
	// and passing one on would make every downstream comparison quietly false.
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, fmt.Errorf("expected a finite number, got %q", trimmed)
	}
	return f, nil
}

// isPeerKind reports whether a kind names a frame peer rather than a file or a
// forward. Peers are the three doors that move JSON-RPC frames.
func isPeerKind(kind string) bool {
	switch kind {
	case "cdp", "mcp", "jrpc":
		return true
	}
	return false
}

// isPeerHandle reports whether target is the old bare-handle spelling of a peer.
//
// KEPT BECAUSE THE CHANNEL FORM STILL USES IT: a stdio MCP server runs ON a shell, so
// `read a1f3:1 --frame` and `call a1f3:1 '{…}'` address a channel, not a session plus
// a kind. A registry key is `<session> <kind>`, which a channel handle can never be,
// so the check stays exact.
func isPeerHandle(target string) bool {
	jrpcChannelsMu.Lock()
	_, found := jrpcChannels[target]
	jrpcChannelsMu.Unlock()
	return found
}

// peerOf resolves the peer a statement addresses: `<session> <kind>:<index>[@<target>]`.
//
// THE SESSION HALF IS RESOLVED, NOT MATCHED. A caller names a session by its id
// (what `ls` prints) or by its host (`internal`), and both mean the same link; the
// registry is keyed by the id, so the name is resolved here exactly once.
//
// THE INDEX IS THE CONNECTION, and `@<target>` is the tab inside it. The bare
// spelling (`internal cdp`) is accepted only while exactly one connection exists —
// with two, an unqualified address would have to pick one, and picking is how a
// caller ends up driving the wrong browser while the reply says it worked.
func (s *Server) peerOf(stmt directive.Statement) (*jrpcHandle, string, *envelope.Slot) {
	if !isPeerKind(stmt.Kind) {
		slot := badRequest(stmt, "not a peer: "+stmt.Kind)
		return nil, "", &slot
	}
	sess, bad := s.resolveTunnelSession(stmt.Target)
	if bad != nil {
		slot := envelope.Slot{Code: 404, Headers: map[string]string{
			"Path": stmt.Path, "Error-Code": CodeSessionNotRunning,
		}, Body: "no running session named " + stmt.Target + ", so there is no " + stmt.Kind +
			" peer on it. `ls` lists the live sessions; `connect " + stmt.Target + "` makes one."}
		return nil, "", &slot
	}

	// `<index>@<target>`: the index is the connection, everything after the `@` is the
	// browser's own target id. Splitting on the LAST `@` keeps a target id containing
	// one (none do today, but the rule should not depend on that).
	connIndex, target := "", ""
	if stmt.Key != "" {
		if at := strings.IndexByte(stmt.Key, '@'); at >= 0 {
			connIndex, target = stmt.Key[:at], stmt.Key[at+1:]
			if target == "" {
				slot := badRequest(stmt, "`"+stmt.Target+" "+stmt.Kind+"…@` names no target. "+
					"A page address is `<session> "+stmt.Kind+":<n>@<targetId>`; `ls "+stmt.Target+" "+stmt.Kind+
					":<n>` prints the ids.")
				return nil, "", &slot
			}
		} else if idx, isIndex := directive.ChannelIndex(stmt.Key); isIndex {
			connIndex = idx
		} else if stmt.Key != "protocol" {
			slot := badRequest(stmt, "`"+stmt.Target+" "+stmt.Kind+" "+stmt.Key+"` names nothing. "+
				"An address is `<session> "+stmt.Kind+":<n>` (a connection) or `<session> "+stmt.Kind+
				":<n>@<targetId>` (a page in it).")
			return nil, "", &slot
		}
	}

	h, refusal := s.peerConn(stmt, sess.ID, stmt.Kind, connIndex)
	if refusal != nil {
		return nil, "", refusal
	}
	// ONLY A BROWSER HAS PAGES.
	if target != "" && stmt.Kind != "cdp" {
		slot := badRequest(stmt, "`"+stmt.Target+" "+stmt.Kind+":"+h.index+"@"+target+"` names nothing: only a cdp peer has "+
			"targets, and `"+stmt.Kind+"` is one connection with no children.")
		return nil, "", &slot
	}
	return h, target, nil
}

// peerConn finds one connection by index, or the only one when no index is given.
func (s *Server) peerConn(stmt directive.Statement, sessionID, kind, index string) (*jrpcHandle, *envelope.Slot) {
	jrpcChannelsMu.Lock()
	defer jrpcChannelsMu.Unlock()
	if index != "" {
		if h, ok := jrpcChannels[peerKey(sessionID, kind, index)]; ok {
			return h, nil
		}
		live := liveAddrsLocked(sessionID, kind)
		body := "this session has no " + kind + ":" + index + ", so that address names nothing."
		if len(live) > 0 {
			body += " Live connections: " + strings.Join(live, ", ") + "."
		}
		slot := envelope.Slot{Code: 404, Headers: map[string]string{
			"Path": stmt.Path, "Error-Code": "not_found",
		}, Body: body}
		return nil, &slot
	}
	// No index: allowed only while the answer is unique.
	var found *jrpcHandle
	n := 0
	for _, h := range jrpcChannels {
		if h.sessionID == sessionID && h.kind == kind {
			found, n = h, n+1
		}
	}
	switch n {
	case 1:
		return found, nil
	case 0:
		slot := envelope.Slot{Code: 404, Headers: map[string]string{
			"Path": stmt.Path, "Error-Code": "not_found",
		}, Body: "this session has no " + kind + " connection, so `" + kind + "` names nothing. " +
			"Open one first: `open <session> " + kind + " <url>` (a browser's devtools endpoint is http://host:port)."}
		return nil, &slot
	default:
		live := liveAddrsLocked(sessionID, kind)
		slot := envelope.Slot{Code: 400, Headers: map[string]string{
			"Path": stmt.Path, "Error-Code": "ambiguous",
		}, Body: "this session has " + fmt.Sprint(n) + " " + kind + " connections, so `" + kind +
			"` alone is ambiguous. Name one: " + strings.Join(live, ", ") + "  (`ls` prints them too)."}
		return nil, &slot
	}
}

// liveAddrsLocked renders a session's connections of one kind, sorted.
func liveAddrsLocked(sessionID, kind string) []string {
	out := make([]string, 0, 2)
	for key := range jrpcChannels {
		if strings.HasPrefix(key, sessionID+" "+kind+":") {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}

// peerAddress renders a peer's address for a reply: `<session> <kind>[:<index>]`.
func peerAddress(sessionID, kind, index string) string {
	return sessionID + " " + kind + ":" + index
}

// pageAddress renders one attached tab: the peer, then the browser's own target id.
//
// THE PAGE IS NOT A NUMBERED NODE. A cdp peer manages a whole browser instance, and
// the browser already names its tabs — `Target.getTargets` returns that id, the tab
// keeps it for its lifetime, and no other tab will ever reuse it. Numbering them here
// would be a second naming scheme for something the peer already names, and it would
// make `:N` mean both "which browser" and "which tab".
func pageAddress(h *jrpcHandle, target string) string {
	return peerAddress(h.sessionID, h.kind, h.index) + "@" + target
}

// closePeersBySession releases every peer that tunnels through sessionID.
//
// A PEER'S TRANSPORT IS THE SESSION'S SSH CHANNEL, so when the session goes dead the
// peer is a socket to nowhere: no frame can ever arrive again, but the handle stays
// addressable and keeps its frame ring — up to 2048 frames or 8 MiB each — for the
// life of the process. Forwards already cascade this way (forwardMgr.CloseBySession);
// peers did not, and the two views disagreed: `ls cdp` hid the dead peer while
// `read <handle>` still answered from it.
//
// Closing outside the lock matters: ch.Close() talks to the transport, and a dead
// transport is exactly the case that can block.
func closePeersBySession(sessionID string) {
	if sessionID == "" {
		return
	}
	jrpcChannelsMu.Lock()
	var doomed []*jrpcHandle
	for key, h := range jrpcChannels {
		if h.sessionID == sessionID {
			doomed = append(doomed, h)
			delete(jrpcChannels, key)
		}
	}
	jrpcChannelsMu.Unlock()
	for _, h := range doomed {
		// A handle without a channel is one that failed before it was usable. This runs
		// on the session-death path, where a nil dereference would take the process down
		// with the session it was cleaning up after.
		if h.ch != nil {
			_ = h.ch.Close()
		}
	}
}

// closePeer ends a peer and forgets it.
func (s *Server) closePeer(stmt directive.Statement, h *jrpcHandle) envelope.Slot {
	jrpcChannelsMu.Lock()
	delete(jrpcChannels, peerKey(h.sessionID, h.kind, h.index))
	jrpcChannelsMu.Unlock()
	_ = h.ch.Close()
	return noContent(stmt)
}

// parseSince reads the --since cursor.
func parseSince(raw string) (int64, error) {
	n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("--since takes a frame number (the number a previous reply reported as Latest), got %q", raw)
	}
	return n, nil
}

// peerChannel routes read/call/close at a frame peer onto the jrpc layer.
//
// TWO SHAPES ARRIVE HERE. A peer opened as `<session> <kind>` (a browser, a network
// MCP server) is resolved from the statement's own target and kind; a stdio MCP
// server running ON a shell is addressed by its channel handle (`a1f3:1`) and never
// reaches the registry at all. Both speak JSON-RPC, and the caller should not have
// to know which kind of pipe it is talking through — so the mapping happens here,
// once, and the one-time MCP handshake stays where it is (per handle).
func (s *Server) peerChannel(ctx context.Context, stmt directive.Statement, win termArgs, sub string, rest []string) []envelope.Slot {
	// `read`/`write` are this tool's general words for taking and putting bytes; on a
	// peer they are its recv and send.
	switch sub {
	case "read":
		sub = "recv"
	case "write":
		sub = "send"
	}
	frame := strings.TrimSpace(stmt.Payload)

	var h *jrpcHandle
	index := ""
	if isPeerKind(stmt.Kind) {
		var bad *envelope.Slot
		h, index, bad = s.peerOf(stmt)
		if bad != nil {
			return []envelope.Slot{*bad}
		}
	} else {
		// A CHANNEL HANDLE. It is usually a command's output, only sometimes a
		// JSON-RPC server — and when it is not a peer, typing at the process is the
		// right thing to do: no handshake, because a handshake would consume the very
		// frames the caller is about to read.
		jrpcChannelsMu.Lock()
		found, ok := jrpcChannels[stmt.Target]
		jrpcChannelsMu.Unlock()
		if !ok {
			return []envelope.Slot{s.mcpStdio(ctx, stmt, sub, stmt.Target, []string{frame})}
		}
		h = found
	}

	// A BROWSER SESSION IS NOT A CHANNEL: the frame is written by the caller, and the
	// one thing this layer adds is the sessionId. Everything else (the method table,
	// the parameters) stays the caller's, which is why there is no CDP command enum
	// anywhere in this file.
	if h.kind == "cdp" && (sub == "call" || sub == "send") {
		f, bad := parseJRPCFrame(stmt, []string{frame})
		if bad != nil {
			return []envelope.Slot{*bad}
		}
		if sub == "send" || boolFlag(stmt, "send") {
			return []envelope.Slot{s.cdpSend(ctx, stmt, h, index, f)}
		}
		return []envelope.Slot{s.cdpCall(ctx, stmt, h, index, f)}
	}
	addr := peerKey(h.sessionID, h.kind, h.index)
	out := stmt
	out.Op = h.kind
	return []envelope.Slot{s.jrpcChannel(ctx, out, win, h.kind, sub, []string{addr, frame})}
}

// openPeer opens a peer through a session and returns the handle.
func (s *Server) openPeer(ctx context.Context, stmt directive.Statement, win termArgs, kind string) []envelope.Slot {
	out := stmt
	out.Op = kind
	return []envelope.Slot{s.jrpcOpen(ctx, out, kind, stmt.Target, stmt.Key)}
}

// directiveLS is the one listing verb, for every collection in the tree.
//
// IT SHEDS THE KIND BEFORE LISTING. `ls host` and `ls ext` name a collection, not a
// node: the parser put the word in Kind because that is where the resource tree says
// it goes, and this translates it into the action the collection's handler expects.
func (s *Server) directiveLS(ctx context.Context, stmt directive.Statement, win termArgs) []envelope.Slot {
	switch stmt.Kind {
	case "host":
		// A NAME HERE WOULD BE SILENTLY DROPPED: the handler lists every entry, so
		// `ls host prod` answered the whole table and the caller believed it had been
		// filtered. One entry is what `get host` is for.
		if stmt.Key != "" {
			return []envelope.Slot{badRequest(stmt, "`ls` lists every host; for one entry use `get host "+
				stmt.Key+"`.")}
		}
		return []envelope.Slot{s.directiveHost(ctx, withFlags(stmt, map[string]string{"action": "list"}), win)}
	case "ext":
		return []envelope.Slot{s.directiveExtension(ctx, withFlags(stmt, map[string]string{"action": "list"}), win)}
	case "alias":
		return []envelope.Slot{s.directiveAlias(ctx, stmt, win)}
	case "forward":
		// A KEY HERE WOULD BE SILENTLY DROPPED. The handler lists every forward on the
		// session, so `ls <session> forward fw-1` would answer a full listing and the
		// caller would believe the key had scoped it. Saying so is cheaper than letting
		// them read a list that looks filtered and is not.
		if stmt.Key != "" {
			return []envelope.Slot{badRequest(stmt, "`ls` lists the whole collection; it does not take a "+
				"forward id. Use `ls "+stmt.Target+" forward` for all of them.")}
		}
		return []envelope.Slot{s.directiveForward(ctx, withFlags(stmt, map[string]string{"action": "list"}), win)}
	case "file":
		// THERE IS NO DIRECTORY LISTING, and this says so instead of guessing.
		//
		// `file --action find` was added and then reverted; the word `ls <session>
		// file <path>` survived in the usage line and here, wrapped around the fs group,
		// whose only members are truncate/realpath/statvfs. A caller asking to list a
		// directory got "file --action fs needs the sub-action", which names a layer
		// they never mentioned and says nothing about the capability being absent.
		//
		// The kind stays DECLARED so it can be refused by name: dropping it would make
		// `file` a stray positional argument and the command would silently list the
		// session instead — the same fall-through that closed a session from `close`.
		return []envelope.Slot{badRequest(stmt, "there is no directory listing here. `ls` enumerates "+
			"nodes termcp knows (sessions, channels, forwards, hosts, extensions), not files: a directory "+
			"is the target's own business, so ask the shell — `run "+stmt.Target+":1 $1` with args [\"ls -la "+
			stmt.Key+"\"]. For one path's metadata use `get "+stmt.Target+" file "+stmt.Key+"`.")}
	}
	if stmt.Target == "" {
		// Bare `ls`: every live link and every channel on it.
		return []envelope.Slot{s.directivePS(ctx, withFlags(stmt, map[string]string{"shells": "true"}), win)}
	}
	// `ls <peer>` ASKS THE PEER what hangs off it. For a browser those are tabs; the
	// question is the same one `ls <session>` asks about channels, so it is the same
	// verb — and it is answered by the peer rather than by the SSH side, which knows
	// nothing about tabs.
	if isPeerKind(stmt.Kind) {
		// `ls <session> cdp` NAMES THE CONNECTIONS; `ls <session> cdp:1` names the tabs
		// inside one. That is the tree rule — a listing shows a node's children — and it
		// is also the only way to discover that a second browser exists.
		if stmt.Key == "" {
			return []envelope.Slot{s.listPeers(stmt)}
		}
		h, _, bad := s.peerOf(stmt)
		if bad != nil {
			return []envelope.Slot{*bad}
		}
		return []envelope.Slot{s.cdpList(ctx, stmt, h)}
	}
	// `ls <session>`: that link's channels. directivePS lists everything, so the
	// statement is narrowed by pointing the handler at one session through --all and
	// filtering in psOne; see directivePS for the filter.
	return []envelope.Slot{s.directivePS(ctx, withFlags(stmt, map[string]string{
		"shells": "true", "only-session": stmt.Target, "all": "true",
	}), win)}
}

// wantPTY decides whether a new channel gets a terminal.
//
// THE RULE IS SSH'S OWN, which is why it needs no explaining: `ssh host` gives you a
// terminal, `ssh host cmd` runs cmd with plain pipes, and `ssh -t host cmd` forces a
// terminal back on. So an --argv means pipe unless --pty asks otherwise, and no
// --argv means pty unless --pipe asks otherwise.
//
// It matters beyond bookkeeping. A pty has no "the command finished" signal, so an
// exit code is unreadable on one; a caller that asked for a single command and got a
// terminal would read an empty reply as "no output" instead of "it is still running".
func wantPTY(stmt directive.Statement) bool {
	if boolFlag(stmt, "pipe") {
		return false
	}
	if boolFlag(stmt, "pty") {
		return true
	}
	argv, _ := stmt.Flag("argv")
	return argv == ""
}

// aliasStatement rebuilds the payload the alias handler reads.
//
// The handler takes "<name> <expansion>" verbatim, so the expansion is never
// re-quoted: what the caller wrote is what the alias runs. The parser splits that
// into a key and positional words — `create alias ll ls a1f3` — and this puts them
// back together, with --off for a removal, so the handler keeps its one contract.
func aliasStatement(stmt directive.Statement, off bool) directive.Statement {
	out := stmt
	parts := append([]string{stmt.Key}, stmt.Args...)
	body := strings.Join(parts, " ")
	if off {
		body = stmt.Key + " --off"
	}
	out.Payload = body
	return out
}

// fileGroupForOp maps a sub-action to the action group that dispatches it.
//
// The SFTP surface has two levels — an action that names a group (perm, link, fs)
// and an op inside it — because the handlers are grouped by what they touch. A verb
// should not expose that: `--op chmod` says everything, and which group carries it
// is an implementation detail. This is the one place that detail lives.
func fileGroupForOp(op string) (string, bool) {
	switch op {
	case "stat", "getwd", "urls":
		// Their own actions, not members of a group. `urls` belongs here: it hands back
		// the download/upload addresses for a path, which is the only way to deliver a
		// file to something outside termcp, and leaving it out of the table made
		// `get <session> file <path> --op urls` answer "does not know".
		return op, true
	case "chmod", "chown", "chtimes":
		return "perm", true
	case "readlink":
		// ONLY readlink. `symlink` and `link` used to be listed here, which meant
		// `get --op symlink` (a read verb, pretending) and `write --op symlink` (refused
		// for want of --target, a flag write does not have). Both are CREATE operations
		// and both live on `create … file`, where the node's key is the link and --target
		// is what it points at.
		return "link", true
	case "truncate", "realpath", "statvfs":
		return "fs", true
	}
	return "", false
}

// listPeers renders every connection of one kind under a session.
//
// IT IS A LISTING, NOT A RESOLUTION: with two browsers open, `ls internal cdp` must
// still answer — that is exactly the moment a caller needs the addresses. The verbs
// that act on one peer refuse an unqualified address in that case; this one hands
// them the names to qualify with.
func (s *Server) listPeers(stmt directive.Statement) envelope.Slot {
	sess, bad := s.resolveTunnelSession(stmt.Target)
	if bad != nil {
		return envelope.Slot{Code: 404, Headers: map[string]string{
			"Path": stmt.Path, "Error-Code": CodeSessionNotRunning,
		}, Body: "no running session named " + stmt.Target + ", so it holds no " + stmt.Kind + " connections."}
	}
	jrpcChannelsMu.Lock()
	addrs := liveAddrsLocked(sess.ID, stmt.Kind)
	handles := make([]*jrpcHandle, 0, len(addrs))
	for _, addr := range addrs {
		if h := jrpcChannels[addr]; h != nil {
			handles = append(handles, h)
		}
	}
	jrpcChannelsMu.Unlock()

	var b strings.Builder
	if len(handles) == 0 {
		hint := map[string]string{
			"cdp":  "a browser's devtools endpoint, http://host:port.",
			"mcp":  "an MCP endpoint, http(s):// or ws(s)://.",
			"jrpc": "any JSON-RPC endpoint over http(s):// or ws(s)://.",
		}[stmt.Kind]
		b.WriteString("no " + stmt.Kind + " connections on this session.\n\n")
		b.WriteString("Next: `open " + stmt.Target + " " + stmt.Kind + " <url>` — " + hint + "\n")
		return envelope.Slot{Code: 200, Headers: map[string]string{"Path": stmt.Path}, Body: b.String()}
	}
	b.WriteString(fmt.Sprint(len(handles)) + " " + stmt.Kind + " connection(s):\n")
	for _, h := range handles {
		b.WriteString("  " + peerAddress(h.sessionID, h.kind, h.index) + "\t" +
			h.ch.Transport().String() + "\t" + h.ch.URL())
		if h.cdp != nil {
			b.WriteString(fmt.Sprintf("\t%d target(s) attached", len(h.cdp.snapshot())))
		}
		b.WriteString("\n")
	}
	return envelope.Slot{Code: 200, Headers: map[string]string{"Path": stmt.Path}, Body: b.String()}
}
