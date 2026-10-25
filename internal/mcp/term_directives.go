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
	"github.com/open-mcp-ai/termcp/internal/task"
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
		if stmt.Kind == "task" {
			// A RECEIPT IS READ WITHOUT A SESSION behind it: the transfer it describes
			// outlived the call that started it, and the caller may have no session
			// handle at all by now. `get task <id>` therefore takes no target.
			return []envelope.Slot{s.directiveGetTask(ctx, stmt, win)}
		}
		if stmt.Kind == "host" {
			// `get host <name>` reads one entry back. Without it a caller could create,
			// edit and delete a host but never see what it holds, so a partial edit had
			// to be done blind.
			return []envelope.Slot{s.directiveHost(ctx, withFlags(stmt, map[string]string{
				"action": "get", "name": stmt.Key,
			}), win)}
		}
		if stmt.Kind == "ext" {
			// `get ext <name>` reads one template back, which is what makes `ls ext
			// <name>` refusable by name rather than by a shrug. Without it the only way
			// to see one extension was the full listing, so a caller that mistyped a
			// name got fourteen rows and believed it had filtered (issue #143).
			return []envelope.Slot{s.directiveExtension(ctx, withFlags(stmt, map[string]string{
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
			// `tools` IS THE MCP SIBLING OF `protocol`: the peer's own table, read live,
			// in the same two positions (`mcp tools` and `mcp:1 tools`). An argument names
			// one tool and prints its full input schema.
			if stmt.Kind == "mcp" && (stmt.Key == "tools" || (len(stmt.Args) > 0 && stmt.Args[0] == "tools")) {
				tool := ""
				if stmt.Key == "tools" {
					if len(stmt.Args) > 0 {
						tool = stmt.Args[0]
					}
				} else if len(stmt.Args) > 1 {
					tool = stmt.Args[1]
				}
				return []envelope.Slot{s.mcpToolList(ctx, stmt, h, win, tool)}
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
		case "task":
			return []envelope.Slot{s.directiveCloseTask(ctx, stmt)}
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
			// AN EXPLICIT --forward-id NAMES A FORWARD, and it must not be swallowed by
			// the container close (dogfood finding on #159). The parser accepts the flag
			// — it is declared on this verb — and the dispatcher then ignored it, so
			// `close <session> --forward-id fw-…` answered 204 and TERMINATED THE WHOLE
			// SESSION: the caller asked to stop one tunnel and silently lost the
			// connection. `--grace` is the mirror image: a session deadline that must not
			// be ignored either.
			//
			// This is not reading intent out of a flag (the mistake `close <channel>
			// --force` was fixed for): no other object a close can name has a forward
			// id, so the flag picks the node rather than modifying it.
			if fid, ok := stmt.Flag("forward-id"); ok && fid != "" {
				if stmt.FlagState("grace") != directive.FlagAbsent {
					return []envelope.Slot{badRequest(stmt, "`--grace` is a session deadline; "+fid+
						" is a forward, and closing a tunnel has nothing to wait for.\n"+
						"Close the tunnel and the connection separately: `close "+stmt.Target+" --forward-id "+fid+
						"`, then `close "+stmt.Target+" --grace N` if that is also wanted.")}
				}
				return []envelope.Slot{s.directiveForward(ctx, withFlags(stmt, map[string]string{
					"action": "close", "forward-id": fid,
				}), win)}
			}
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
		// A PIPELINE IS ONE STATEMENT WITH TWO ENDS, and it is answered by the copy
		// before any of the single-ended read paths below can claim it.
		if stmt.Pipe != nil {
			return s.pipeCopy(ctx, stmt, win)
		}
		// The right half of a pipeline is only reachable through its left half, so
		// reaching it here means something routed it alone. Refuse rather than run a
		// write against a target nobody named.
		if stmt.Piped {
			return []envelope.Slot{badRequest(stmt, "this is the right half of a `|`; it runs as part of the pipeline")}
		}
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
			// --restart REBUILDS THE SESSION, and it is the one thing on this node that is
			// not a configuration change. An edit already rebuilds when it changes the
			// session template — the reconciler compares a fingerprint — but not when the
			// template reads the same and only the FILE it names has changed, which is
			// exactly a git pull. Then the process is still running the old script, and the
			// DSL is the only entrance an agent gets, so without this it had no way to say
			// "rebuild it": the reconciler's action was reachable from the MCP tool and the
			// admin API, but not from the language the agent actually speaks.
			if boolFlag(stmt, "restart") {
				if other := firstFlagOtherThan(stmt, "restart"); other != "" {
					return []envelope.Slot{badRequest(stmt,
						"--restart rebuilds the session and changes no configuration, so it cannot be combined with --"+
							other+"; send the edit and the rebuild as two calls")}
				}
				act = "retry"
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
// firstFlagOtherThan names a flag the caller set besides the one given.
//
// It exists so a call that asks for two things can be refused BY NAME. The
// alternative - doing one of them and reporting success - is the failure mode this
// codebase has been bitten by more than once: the caller believes it did both, and
// the half that was dropped is the one it cannot see.
func firstFlagOtherThan(stmt directive.Statement, except string) string {
	names := make([]string, 0, len(stmt.Flags))
	for name := range stmt.Flags {
		if name != except {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return ""
	}
	// Map order is random and the message is read by a person: sort it.
	sort.Strings(names)
	return names[0]
}

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

// closedSlot answers a close with the object it closed, in a header.
//
// A CLOSE IS THE LAST THING A CALLER DOES WITH AN OBJECT, AND IT WAS THE ONLY REPLY
// THAT SAID NOTHING. `close <session>` answered 204 with an empty body, so the caller
// could not tell "closed the session I named" from "closed something, somewhere" — the
// reported reaction was to run `ls` and see what was missing, a call spent confirming
// instead of advancing, on the step where a wrong answer is most expensive.
//
// The code stays 204: there is no content to return, and the REST entrance drops a body
// on 204, so a sentence there would reach MCP only. A header is carried by both.
//
// The NAME rides along when the object has one, because that is how the caller thinks
// of it: they wrote `--name explore` and the reply should say "explore", not only the
// uuid they copied out of a receipt three calls ago.
func closedSlot(stmt directive.Statement, address, name string) envelope.Slot {
	closed := address
	if strings.TrimSpace(name) != "" {
		closed += " (" + name + ")"
	}
	return envelope.Slot{Code: 204, Headers: map[string]string{
		"Path":   stmt.Path,
		"Closed": closed,
	}}
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

	// THE WINDOW IS CHECKED BEFORE ANYTHING IS TYPED, because a 400 must not be a lie.
	//
	// The window is validated where it is read — inside shell_output — and that read
	// happens after this function has typed the line and pressed Enter. So a refusal
	// about the window arrives after the command has already run (issue #179), and a 4xx
	// reads as "nothing happened": the caller retries, and the command runs twice. On a
	// mutating command the retry is the damage.
	//
	// The rule that made this reachable was the upper bound on `timeout`, which only
	// the read validator had (issue #168): every other window number is checked at the
	// entrance, before dispatch, so the tool could not show the lie. Removing that bound
	// removes the trigger; this check removes the CLASS, because the next rule added to
	// the read validator would otherwise arrive after the effect again. The same
	// validator is called with the same numbers, just earlier — one rule, two callers.
	if f := s.checkWindow(stmt, win); f != nil {
		return s.slotFromFailure(ctx, stmt, f)
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

	// --grep CHANGES WHAT THE WINDOW COUNTS, so it takes the same path `read` does
	// (matches rather than raw lines) — scoped to this command, which is the region the
	// caller just created (issue #187).
	//
	// IT WAITS FIRST, THE SAME WAY THE PLAIN PATH DOES. A search over bytes that have not
	// arrived is not a search: it scanned nothing and answered "no line matched … in the 0
	// lines scanned", which is true and useless — found by driving it, not by a test. The
	// wait is the same one the read half uses, so the two halves of a `run` cannot
	// disagree about when the command's output is there.
	if _, ok := stmt.Flag("grep"); ok {
		waited := s.awaitRunOutput(ctx, stmt, win, start)
		slot := s.readGrepFrom(ctx, stmt, win, start, readTimeout(remaining(win, waited)))
		if slot.Code < 400 {
			if slot.Headers == nil {
				slot.Headers = map[string]string{}
			}
			// A searched run is still a run: the caller needs the offset its input landed
			// at as much here as anywhere (issue #194).
			slot.Headers["Input-At"] = strconv.FormatInt(start, 10)
		}
		return slot
	}
	slot := s.readSlotAfter(ctx, stmt, win, start)
	return tailSlot(slot, stmt)
}

// tailSlot keeps the last N lines of a reply, and says that it did.
//
// NOT THE TRANSCRIPT'S OWN TAIL SELECTOR. That one reads from the end of the whole
// buffer and IGNORES an offset when both are given (window.go switches on TailLines
// before Offset), so `run --tail 20` would hand back the previous command's last lines
// as part of this command's output — the exact confusion issue #194 is about. The
// window is read from this command's input first, and the tail is taken from it.
//
// The trim is a FACT, not a silent loss: the reply names how many lines it kept and how
// many the window held, and Content-Range still describes the stream span the window
// came from (so `read --cursor` continues from where this reply's bytes ended).
func tailSlot(slot envelope.Slot, stmt directive.Statement) envelope.Slot {
	v, ok := stmt.Flag("tail")
	if !ok || v == "" || slot.Code >= 400 || slot.Body == "" {
		return slot
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return slot
	}
	lines := strings.Split(strings.TrimRight(slot.Body, "\n"), "\n")
	if len(lines) <= n {
		return slot
	}
	if slot.Headers == nil {
		slot.Headers = map[string]string{}
	}
	slot.Headers["Trimmed"] = fmt.Sprintf("the last %d of %d lines of this command's output; read the rest with `read`", n, len(lines))
	slot.Body = strings.Join(lines[len(lines)-n:], "\n")
	return slot
}

// checkWindow validates the read window a command will be read with, before the
// command runs.
//
// It builds the argument map readSlot would build and hands it to the same parser,
// so the two cannot drift. `timeout` here is the caller's whole window rather than
// what is left of it: the remainder is always smaller, so a window that passes here
// cannot be refused later, and one refused here would have been refused there.
func (s *Server) checkWindow(stmt directive.Statement, win termArgs) *toolFailure {
	args := argsOf("shell_id", stmt.Target)
	args["max_bytes"] = float64(win.BytesLimit)
	args["max_lines"] = float64(win.LinesLimit)
	args["timeout"] = win.TimeoutSeconds
	if _, bad := parseReadParams(args); bad != nil {
		return failureFromResult(bad)
	}
	return nil
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
	// 409, NOT 500. The channel's process is over; nothing about the server broke and
	// the caller's own line was fine. A 5xx here invites a retry of a write that can
	// never land, and hides a finished channel behind "the server failed".
	return &toolFailure{
		code: CodeSessionNotRunning,
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
	// bytes_limit 0 means "hand back the channel, consume nothing". readSlotAfter
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
	// --offset IS THE SAME BYTE POSITION AS --cursor, and on a channel read it used to
	// be accepted and IGNORED (dogfood finding on #159): the flag is declared for the
	// peer `--at` path, so the parser took it and this function never looked. A caller
	// who wrote `read <channel> --offset 0` therefore got the default cursor read — on
	// a closed channel, an empty one — and nothing said the flag had not been used.
	//
	// Asking twice is refused rather than silently resolved: both name where the read
	// starts, and a caller who wrote both is not describing a position.
	if stmt.FlagState("cursor") != directive.FlagAbsent && stmt.FlagState("offset") != directive.FlagAbsent {
		return badRequest(stmt, "`--cursor` and `--offset` name the same byte position; pass one of them.")
	}
	if slot, bad := numericFlags(args, stmt,
		[2]string{"cursor", "offset"},
		[2]string{"offset", "offset"},
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
	// THE SESSION'S ENCODING, when the caller knows it. A terminal is not a UTF-8
	// device: a zh-CN Windows console writes GBK, so without this the reply was a
	// refusal the caller could not act on (issue #149).
	if v, ok := stmt.Flag("encoding"); ok {
		args["charset"] = v
	}
	return s.readSlot(ctx, stmt, win, args, 0)
}

// awaitRunOutput waits for the command a `run` just typed, using the same signals the
// read half uses, and returns how long it waited.
//
// ONE PLACE DECIDES WHEN A COMMAND'S OUTPUT IS THERE, because two places deciding it
// separately is how the plain half and the searched half of one verb end up disagreeing:
// `run --grep` read with a zero window while `run` read with the caller's, so a search
// over bytes that had not arrived answered "no line matched … in the 0 lines scanned".
//
// A LIVE PIPE HAS NO SIGNAL AT ALL — no exit to wait for and no pty to go quiet — so this
// returns immediately and the caller's window is spent by the read that follows, which
// blocks for it. That is the division the plain path makes, and it is why the return is a
// duration rather than a bool: what is LEFT of the window is what the read may spend.
func (s *Server) awaitRunOutput(ctx context.Context, stmt directive.Statement, win termArgs, start int64) time.Duration {
	began := time.Now()
	if s.completionWait(stmt.Target) {
		s.awaitExit(ctx, stmt.Target, win)
	} else if s.ptyRunning(stmt.Target) {
		s.awaitWindow(ctx, stmt.Target, win, start)
	}
	return time.Since(began)
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
	// WHERE THIS COMMAND'S OUTPUT BEGINS (issue #194).
	//
	// The reply is a SAMPLE of the channel's stream, and the one number that turns the
	// sample into something a caller can act on is the offset the input landed at: it is
	// what `read --offset <n>` takes to get the rest, and it is what tells an empty reply
	// apart from a caller whose cursor is already past the output. The number was already
	// computed — it is the offset this window was scoped to — it just was not said.
	//
	// It is the offset BEFORE the input, not the marker's: the window starts where the
	// command's bytes begin, so re-reading from Input-At gives back this same window
	// (marker included) rather than skipping the echo.
	inputAt := strconv.FormatInt(start, 10)
	// A byte limit of 0 means "hand back the channel, consume nothing": the
	// directive must not even read, or a JSON-RPC frame destined for `mcp recv`
	// would be eaten by the envelope.
	if win.BytesLimit == 0 {
		return envelope.Slot{
			Code: 204,
			Headers: map[string]string{
				"Path":          stmt.Path,
				"Content-Range": envelope.UnsatisfiedRange(start),
				"Input-At":      inputAt,
			},
		}
	}
	waitStarted := time.Now()
	// windowExpired is a FACT the wait already computed and then threw away: a pty has
	// no completion signal, so "the window ran out while bytes were still arriving" is
	// the closest thing to "this command is still running" the server can honestly say —
	// and without it the caller cannot tell a finished command from a window that ended
	// (issue #161, P1-3).
	windowExpired := false
	waitedOnAPty := false
	if wait := s.completionWait(stmt.Target); wait {
		s.awaitExit(ctx, stmt.Target, win)
	} else if s.ptyRunning(stmt.Target) {
		waitedOnAPty = true
		windowExpired = s.awaitWindow(ctx, stmt.Target, win, start)
	}
	slot := s.readSlot(ctx, stmt, win, args, time.Since(waitStarted))
	if slot.Code < 400 {
		if slot.Headers == nil {
			slot.Headers = map[string]string{}
		}
		slot.Headers["Input-At"] = inputAt
	}
	if waitedOnAPty && !windowExpired && slot.Code < 400 {
		// HOW THE WAIT ENDED IS A FACT, AND IT WAS THE ONE THING NOT SAID.
		//
		// A pty has no completion signal, so this wait ends one of two ways: the window
		// runs out, or the stream goes quiet (ptyQuiet with no new byte). The second is
		// the ordinary case — it is how a finished command looks — and it said NOTHING,
		// so a command that merely went silent while it worked was indistinguishable from
		// one that had finished. Three callers reading the same reply hit it the same way:
		// "the output looks truncated, and nothing tells me whether it is".
		//
		// `Still-Running` is deliberately NOT reused: it means "the window expired while
		// bytes were still arriving", which is evidence. A quiet stream is not evidence of
		// anything — the command may have finished, or be sleeping — and a header that
		// claimed either would be the kind of guess this server refuses to make about a
		// pty. So it names the evidence instead, and `read` is how the caller finds out
		// more.
		if slot.Headers == nil {
			slot.Headers = map[string]string{}
		}
		slot.Headers["Completion"] = "quiet"
	}
	if windowExpired && slot.Code < 400 && s.ptyRunning(stmt.Target) && slot.Headers["Still-Running"] == "" {
		// Same header the pipe path uses, same meaning: the command was accepted and has
		// not finished. It is set only when the window expired, so it does not become the
		// permanently-true flag that kept it off the pty path in the first place.
		if slot.Headers == nil {
			slot.Headers = map[string]string{}
		}
		slot.Headers["Still-Running"] = "true"
	}
	// A WINDOW I SHOWED IS A WINDOW I READ. The read above is positional (it names an
	// offset), and a positional read moves no cursor — so `read <channel>`'s documented
	// default, "the new bytes since the last read", was false for the flow callers
	// actually use: after three `run`s the first plain read replayed the channel from
	// byte zero (issue #162, S2). The caller had seen those bytes; the cursor had not.
	//
	// Only when bytes were actually handed over: `fold` and `bytes_limit: 0` reply
	// without showing anything, and advancing past bytes the caller never saw would
	// turn a withheld window into a skipped one.
	if slot.Code < 400 && slot.Body != "" {
		if end, ok := envelope.RangeEnd(slot.Headers["Content-Range"]); ok {
			if cs, bad := s.requireShell(stmt.Target); bad == nil {
				cs.AdvanceDefaultReader(end)
			}
		}
	}
	return slot
}

// humanTyped reports whether bytes entered this channel through the non-MCP entrance
// since start.
//
// THIS IS THE ONE FACT ABOUT "WHO IS ON THE OTHER END" THAT TERMCP ACTUALLY HAS.
// The tempting question — "is something in the foreground waiting for input?" — cannot
// be answered from here (the process group lives on the far side of the SSH connection,
// and a Windows terminal has no such notion), and guessing it from the byte stream is
// impossible: a program blocked at a prompt and a finished command are byte-identical.
//
// But the transcript already records who typed. Every input span carries a status —
// `i` the HTTP/WebSocket surface, `a` an agent through MCP — because the log has to be
// replayable and auditable. So "someone else is writing here" is a recorded fact, not an
// inference, and a caller about to type into a shared channel deserves to know first.
//
// The value reported is therefore the entrance (`api`), not a guess about who is behind
// it: a browser and a script driving /api/term are the same status in the log, and
// claiming "human" for both would be wrong for one of them.
//
// The question asked is "at or after the start of this window", and the answer comes from
// the store's sparse seek table (MarksWindow) rather than the whole index: a reader that
// needs one screen of a long log should not pay for the log.
func (s *Server) humanTyped(handle string, start, end int64) bool {
	cs, bad := s.requireShell(handle)
	if bad != nil {
		return false
	}
	if end < start {
		end = start
	}
	marks, _, err := s.sessMgr.MarksWindow(cs.ParentSessionID(), cs.ID, start, end)
	if err != nil {
		return false
	}
	for _, m := range marks {
		if m.Offset < start {
			continue
		}
		if m.Status == api.LogAPIInput {
			return true
		}
	}
	return false
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

// ptyQuiet is how long a pty's stream must stand still before a wait calls the
// command done.
//
// MEASURED, NOT GUESSED. One `echo` on a PowerShell with an oh-my-posh profile
// arrives as five chunks spread over 1.1s: the echo of the typed line at 120ms,
// a first repaint at 510ms, the command's own output at 940ms. The gaps between
// chunks reach 430ms, so a shorter grace ends the wait on the echo alone and the
// reply looks like a command that printed nothing. 500ms covers the widest gap
// this shell showed and still ends a fast command in about a quarter of the 2s
// window a caller typically passes.
const ptyQuiet = 500 * time.Millisecond

// ptyPoll is how often the wait asks the stream how long it is. The wait is a
// poll rather than a subscribe because the length is already tracked per channel
// and asking for it is cheaper than registering a second reader that would then
// swallow bytes meant for `read`.
const ptyPoll = 20 * time.Millisecond

// awaitWindow waits out a pty command, bounded by the caller's time window.
//
// A PTY HAS NO COMPLETION SIGNAL, so the wait used to be a blind sleep of the
// whole window: `run` on a 20s window took 20s on a command that finished in one,
// and the reply said nothing about how long the command itself took. The window
// is the caller's budget, not the command's duration, so the wait now watches the
// stream: it ends when the stream has stood still for ptyQuiet, and only falls
// back to the window when there is nothing to watch (a command that has printed
// nothing yet has no other signal). timeout=0 stays a snapshot, not a wait.
//
// The cost of the trade: a command that thinks silently for longer than ptyQuiet
// after printing comes back with what it printed so far, and the caller reads on.
// That is the same contract a terminal has — you cannot tell "done" from "quiet"
// either — and it is the one direction the old blind sleep got wrong.
//
// IT RETURNS WHICH OF THE TWO HAPPENED, because the caller can act on the difference:
// the window running out means the command has not finished, which is a fact worth a
// header. Ending on quiet means the command stopped talking, which is as close to "done"
// as a terminal ever gets.
func (s *Server) awaitWindow(ctx context.Context, handle string, win termArgs, start int64) (windowExpired bool) {
	limit := time.Duration(win.TimeoutSeconds * float64(time.Second))
	if limit <= 0 {
		return false
	}
	deadline := time.Now().Add(limit)
	last := start
	var still time.Time
	ticker := time.NewTicker(ptyPoll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
		now, ok := s.outputSizeFor(handle)
		if !ok {
			// The channel went away under the wait; the read that follows reports it
			// in the caller's own vocabulary instead of "nothing arrived".
			return false
		}
		switch {
		case now != last:
			last, still = now, time.Time{}
			// THE REPLY CANNOT GROW, SO WAITING BUYS NOTHING.
			//
			// The space window is a limit on how much enters the caller's context, so once
			// the byte window is full every further byte is discarded: the wait would sit
			// there for the rest of the window to collect output the reply cannot carry.
			// A noisy command (a scan, a brute force) never goes quiet, so this was the
			// only thing that could end the wait early — the reporting caller's 320s
			// window over a command that filled the reply in its first second is exactly
			// this case (issue #161, P1-3).
			if win.BytesLimit > 0 && now-start >= int64(win.BytesLimit) {
				return true
			}
		case now <= start:
			// Nothing has been said yet. Silence before the first byte is not
			// completion, it is a command that has not started talking; only the
			// window can end this wait.
		default:
			if still.IsZero() {
				still = time.Now()
			} else if time.Since(still) >= ptyQuiet {
				return false
			}
		}
		if !time.Now().Before(deadline) {
			return true
		}
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
//
// `waited` IS HOW MUCH OF THE CALLER'S WINDOW IS ALREADY SPENT. The wait before this
// read (awaitWindow / awaitExit) burns the caller's budget, and the read that follows
// then passed the FULL timeout again — so a `run` with timeout=60 could block for 120
// seconds, and the second half of that was invisible in the reply. The window is one
// budget, not two: the read gets what is left.
//
// That is also what makes `timeout` mean one thing. `readTimeout` capped a plain read at
// 60 and left `run` uncapped, so the same parameter bounded two directives differently
// and nothing in the reply said which had happened (issue #161, P1-3).
func (s *Server) readSlot(ctx context.Context, stmt directive.Statement, win termArgs, args map[string]any, waited time.Duration) envelope.Slot {
	started := time.Now()
	args["max_bytes"] = float64(win.BytesLimit)
	args["max_lines"] = float64(win.LinesLimit)
	// A pipe channel that has not exited yet must not block for the whole timeout
	// on a read that has nothing new; a pty read waits for the quiet window
	// instead. Both are upper bounds, so the smaller of the two wins.
	args["timeout"] = readTimeout(remaining(win, waited))

	res, fail := s.callTool(ctx, "shell_output", args)
	if fail != nil {
		return s.slotFromFailure(ctx, stmt, fail)
	}
	doc, ok := toolResultMap(res)
	if !ok {
		return s.slotFromFailure(ctx, stmt, &toolFailure{code: CodeOperationFailed, msg: toolResultText(res)})
	}
	// WAITED IS THE WHOLE CALL, not just this read.
	//
	// The header exists to tell "I waited and heard nothing" from "I did not wait" —
	// the distinction that keeps an empty reply from looking like a broken channel. It
	// was measured from the READ, so the wait before it (where the window is actually
	// spent) was missing: a call that sat on a 1s window answered `Waited: 0ms`, and the
	// reporting caller read that as "the window never happened" (issue #161, P1-3).
	return s.windowSlot(stmt, win, doc, waited+time.Since(started))
}

// remaining is the part of the caller's window that the wait did not spend.
//
// A negative or zero remainder is zero: the wait already used the budget, so the read
// that follows must not block at all. That is the difference between "the window ended"
// and "the window ended, and then we waited again".
func remaining(win termArgs, waited time.Duration) termArgs {
	left := win.TimeoutSeconds - waited.Seconds()
	if left < 0 {
		left = 0
	}
	win.TimeoutSeconds = left
	return win
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

	// A WINDOW THAT ROLLED BACK SAYS SO.
	//
	// The rule is that a window never ends inside a character, so the bytes that would
	// have been cut are withheld and the next read delivers them whole. That is lossless
	// and the Content-Range already says where the window stopped — but "I got fewer
	// bytes than bytes_limit" and "a character straddled the end" are the same bytes on
	// the wire, and a caller paging by its own arithmetic (offset += bytes_limit) would
	// skip the withheld bytes and then land INSIDE a character on the next read, where
	// the reply is a 400 naming a charset the session does not write. One header turns
	// that into a fact the caller can act on.
	if n := int64(getFloatAny(doc, "trimmed_bytes", 0)); n > 0 {
		// TWO CASES, because the remedy differs and only one of them has one.
		//
		// Mid-stream, the withheld bytes are simply early: the next read starts at them
		// and gets them whole. At the END of the stream there is no next read that can
		// complete them — they are a fragment of a character that the writer never
		// finished — so promising "read --cursor A continues there" would send the caller
		// into a loop that returns nothing forever. Say which one it is.
		//
		// Both are said in terms of the CHARSET, because that is what decided the cut:
		// the boundary came from the encoding, not from the size limit the caller passed.
		if end+n >= total {
			headers["Trimmed"] = fmt.Sprintf("bytes %d-%d withheld: they are the end of the stream and do not form a character in this encoding, so no read returns them as text",
				end, end+n-1)
		} else {
			headers["Trimmed"] = fmt.Sprintf("bytes %d-%d withheld: the window end fell inside a character in this encoding; read --cursor %d continues there",
				end, end+n-1, end)
		}
	}

	// A FILTERED READ SAYS WHAT IT PASSED OVER.
	//
	// `--stream o` answers "stdout only" by selecting a subset of the span the window
	// covers, and the span still runs to End — the cursor has to move by the range, or a
	// poll loop would re-read the same bytes forever. So the bytes of the other stream
	// leave the caller's cursor without ever being shown, and nothing used to say so.
	// The worst case was not a partial body: `--stream e` on a stdout-only channel
	// answered 200 with an EMPTY body and `Content-Range: bytes 0-7/8` — "eight bytes
	// here" and zero delivered — and the plain read that followed returned 204, so eight
	// bytes of output were consumed by a reply that displayed nothing at all.
	//
	// The count and the flag that shows them turn that into a fact the caller can act on.
	// It is deliberately NOT a refusal and NOT a rewind: the caller asked for a view, and
	// the bytes are still in the channel for whoever asks for the other view.
	if n := int64(getFloatAny(doc, "stream_omitted_bytes", 0)); n > 0 {
		// The stream is a flag on the statement, not a window parameter: the filter runs
		// inside the read, and only the statement knows which side was asked for.
		want, _ := stmt.Flag("stream")
		other := "e"
		if want == "e" {
			other = "o"
		}
		headers["Omitted"] = fmt.Sprintf("%d bytes in this range are not in stream %s and went past unseen; read --stream %s (or --stream all) shows them, and --offset reaches back",
			n, want, other)
	}

	// WHO ELSE IS TYPING. The reply can say that a person wrote into this channel,
	// because the transcript records the source of every input span (`i` human through
	// the HTTP/WebSocket surface, `a` an agent through MCP) — the log has to be
	// replayable, so the attribution is a recorded fact rather than an inference. It is
	// the one part of "this channel's state is not mine alone" that termcp can prove: a
	// program blocked at a prompt and a finished command are byte-identical, so the
	// foreground question is unanswerable here, but "a human typed" is in the index.
	//
	// Only when it is at or after this window's start: a person who typed before the
	// bytes being shown is history the caller has already accounted for.
	if s.humanTyped(stmt.Target, start, total) {
		// "api", NOT "human". The transcript records which ENTRANCE the bytes came
		// through (`i` the HTTP/WebSocket surface, `a` MCP), and the HTTP surface is used
		// by a person in a browser AND by any script that drives /api/term directly. An
		// agent calling the REST entrance was being told "a human typed" about its own
		// command — the header has to name the fact termcp holds, not the one it is
		// guessing at.
		headers["Typed-By"] = "api"
	}

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
	// A 204 WITH NO WAIT IS THE ONE REPLY THAT LOOKS LIKE A BUG.
	//
	// An empty read of a live shell is legitimate — the command may simply not have
	// printed yet — but `Waited: 0ms` says that in a technical voice, in the header
	// block a caller skims last. The reported reaction was to suspect the input had
	// been dropped and spend a call proving it had not. Naming the flag is what turns
	// that into an obvious next move.
	//
	// A HEADER, not a body sentence: this reply is a 204, and 204 must not carry a
	// body — the REST entrance would drop it and only MCP would show the hint, which
	// is the silent divergence between the two entrances this repo keeps paying for.
	// It appears only on the empty-and-unwaited case, so a read with output never pays.
	if code == 204 && waited == 0 && s.ptyRunning(stmt.Target) {
		headers["Hint"] = "nothing yet — a pty is still running. Pass timeout=<ms> to wait for output"
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
			// 126 AND 127 ARE TWO ANSWERS, AND THE SHELL'S OWN WORDS DO NOT TELL THEM
			// APART (issue #191). "permission denied" covers both "this file has no
			// execute bit" and "this filesystem is mounted noexec", and the caller who
			// reads it starts chmod-ing a file that is already 755. "command not
			// found" is a PATH answer. Neither is a fact about the terminal, so it is
			// stated rather than guessed at: the two causes are named, and the mode is
			// one command away (`--op stat` reports it).
			if *exit == 126 {
				headers["Hint"] = "exit 126: the program exists but could not be executed. Check its mode " +
					"(`get <session> file <path> --op stat` reports mode/uid/gid); a filesystem mounted noexec " +
					"refuses execution even at 755, which `mount` on the remote will show."
			} else if *exit == 127 {
				headers["Hint"] = "exit 127: the shell could not find that program — check the name and the " +
					"remote PATH, not the file's permissions."
			}
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

// readTimeout is the blocking wait a read gets.
//
// IT USED TO CAP AT 60 SILENTLY, which made one parameter mean two things: `run` waited
// the caller's full window and `read` waited at most a minute, with nothing in the reply
// saying which bound had applied (issue #161, P1-3). The cap is gone: the budget passed
// here is already what is LEFT of the caller's window (see readSlot), so the number is
// the caller's own and no second, hidden one is needed.
//
// Zero stays zero: a non-blocking read really is non-blocking.
func readTimeout(win termArgs) float64 {
	if win.TimeoutSeconds <= 0 {
		return 0
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
			return []envelope.Slot{s.mcpStdio(ctx, stmt, sub, stmt.Target, []string{frame}, win)}
		}
		h = found
	}

	// THE WINDOW IS THIS SIDE'S OWN DEADLINE (#158). Without it the only bound on a
	// peer call is the caller's context, which belongs to a host watchdog this daemon
	// cannot see — so a peer that never answers hangs until the host kills the turn,
	// and what the caller reads is a watchdog abort rather than "the peer is not
	// answering". A waiter gets the caller's window, or 15s when none was asked for.
	callCtx, stopCall := ctx, context.CancelFunc(func() {})
	if sub == "call" || sub == "recv" {
		callCtx, stopCall = context.WithTimeout(ctx, peerCallWindow(win))
		defer stopCall()
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
			return []envelope.Slot{s.cdpSend(callCtx, stmt, h, index, f)}
		}
		return []envelope.Slot{namePeerTimeout(callCtx, s.cdpCall(callCtx, stmt, h, index, f))}
	}
	addr := peerKey(h.sessionID, h.kind, h.index)
	out := stmt
	out.Op = h.kind
	return []envelope.Slot{namePeerTimeout(callCtx, s.jrpcChannel(callCtx, out, win, h.kind, sub, []string{addr, frame}))}
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
	case "task":
		// TASKS ARE NOT A SESSION'S COLLECTION. A transfer belongs to the process, not
		// to a session handle, so `ls task` takes no target — the same shape as
		// `ls host`.
		if stmt.Key != "" {
			return []envelope.Slot{badRequest(stmt, "`ls task` lists every transfer; for one of them use `get task "+
				stmt.Key+"` (pass a `timeout` on the call and it waits).")}
		}
		return []envelope.Slot{s.directiveLsTask(stmt)}
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
		// A NAME HERE WOULD BE SILENTLY DROPPED, exactly as it was on `ls host` and
		// `ls <session> forward`. `ls ext evrything` answered the whole list, so a
		// caller checking that an extension exists read fourteen rows as confirmation.
		if stmt.Key != "" {
			return []envelope.Slot{badRequest(stmt, "`ls` lists every extension; for one of them use `get ext "+
				stmt.Key+"`.")}
		}
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
	//
	// A CHANNEL ADDRESS IS NOT A COLLECTION, AND IT USED TO BE ANSWERED AS ONE
	// (issue #148). The parser folds `ls s1 5` into the address `s1:5`, so the index
	// arrives on the target rather than in Key. Dropped here, `ls s1 5` answered the
	// whole listing — or, on a session with no live channels, the global "(nothing is
	// running)", which is a sentence about a different collection. A caller using a
	// stale index to ask "is my channel up" read that as "the session is dead".
	if idx := locator.SplitHandle(stmt.Target).Index; idx > 0 {
		sid := locator.SplitHandle(stmt.Target).SessionID
		return []envelope.Slot{badRequest(stmt, "`ls "+sid+"` lists every channel; for one of them "+
			"use `get "+sid+":"+strconv.Itoa(idx)+"`.")}
	}
	// A TARGET NAMES A COLLECTION, SO A TARGET THAT NAMES NOTHING MUST SAY SO.
	//
	// `ls nosuchthing` (and `ls <a session that was rm'd>`) fell through to the same
	// filtered listing a real session gets, the filter matched nothing, and the reply was
	// the GLOBAL sentence "(nothing is running)" plus the hint about `ls host`. That
	// sentence is about a different collection and answers a different question: a caller
	// asking "what is on this link" was told the machine has no sessions at all. Same
	// shape as #148 one branch up — a stale or mistyped target read as a fact about the
	// world (found by dogfooding).
	//
	// THE PREDICATE IS THE LISTING'S OWN. `ls <target>` narrows to a session by id OR by
	// profile name — `ls internal` is how a caller lists the session on the internal
	// profile — so a check that only knew about ids refused a spelling that had been
	// working, and the browser test caught it. A target that names neither is the one
	// case where the empty listing is a lie about the collection.
	if !s.lsTargetNames(stmt.Target) {
		return []envelope.Slot{s.slotFromFailure(ctx, stmt, &toolFailure{
			code: CodeSessionNotFound,
			msg: fmt.Sprintf("no session %q, so there is nothing to list for it. `ls` lists every live "+
				"link, `ls --all` includes the ones that have exited.", stmt.Target),
		})}
	}
	return []envelope.Slot{s.directivePS(ctx, withFlags(stmt, map[string]string{
		"shells": "true", "only-session": stmt.Target, "all": "true",
	}), win)}
}

// lsTargetNames reports whether a target names a session the listing can show: its id,
// or the profile it was opened on.
//
// Both spellings are what directivePS's own filter accepts, and keeping the two in step
// is the point — a check that answered a narrower question than the filter would refuse
// targets the filter handles.
func (s *Server) lsTargetNames(target string) bool {
	for _, sess := range s.sessMgr.ListAll() {
		if sess.ID == target || sess.SSHConfig == target {
			return true
		}
	}
	return false
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

// --- task: the receipt side of an asynchronous transfer (#158) ---

// directiveGetTask answers `get task <id>`, waiting out the caller's window.
//
// THE WINDOW IS THE WAIT, exactly as it is for a pty read: the reply comes back the
// instant the transfer finishes, and one that does not finish inside the window
// answers with its progress rather than an error. That is the whole point of a
// receipt — the caller decides how long to wait, not a watchdog it cannot see.
func (s *Server) directiveGetTask(ctx context.Context, stmt directive.Statement, win termArgs) envelope.Slot {
	id := strings.TrimSpace(stmt.Key)
	if id == "" {
		id = strings.TrimSpace(stmt.Target)
	}
	if id == "" {
		return badRequest(stmt, "get task needs the id from the receipt: get task task_abc123")
	}
	timeout := time.Duration(win.TimeoutSeconds * float64(time.Second))
	v, err := s.tasks.Await(ctx, id, timeout)
	if err != nil {
		return notFoundSlot(stmt, CodeInvalidArgument, err.Error()+
			"\nA receipt is answerable for ten minutes after the transfer ends; `ls task` lists the live ones.")
	}
	code := 200
	if v.Status == task.Failed {
		code = 500
	}
	return envelope.Slot{Code: code, Headers: map[string]string{
		"Path": stmt.Path, "Task": v.ID, "Status": string(v.Status),
	}, Body: renderTask(v)}
}

// directiveLsTask answers `ls task`: every transfer this process knows about.
func (s *Server) directiveLsTask(stmt directive.Statement) envelope.Slot {
	list := s.tasks.List()
	if len(list) == 0 {
		return envelope.Slot{Code: 200, Headers: map[string]string{"Path": stmt.Path},
			Body: "no transfers. A `write <session> file <path> --local-path <file>` starts one and answers with a task id."}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d task(s):\n", len(list))
	for _, v := range list {
		fmt.Fprintf(&b, "  %-16s %-9s %-9s %s\n", v.ID, v.Kind, v.Status, taskProgress(v))
	}
	b.WriteString("\n`get task <id>` waits for one; `close task <id>` cancels it.")
	return envelope.Slot{Code: 200, Headers: map[string]string{"Path": stmt.Path}, Body: b.String()}
}

// directiveCloseTask answers `close task <id>`: cancel the transfer, do not delete
// the receipt.
func (s *Server) directiveCloseTask(ctx context.Context, stmt directive.Statement) envelope.Slot {
	id := strings.TrimSpace(stmt.Key)
	if id == "" {
		id = strings.TrimSpace(stmt.Target)
	}
	if id == "" {
		return badRequest(stmt, "close task needs the id: close task task_abc123")
	}
	stopped, err := s.tasks.Cancel(id)
	if err != nil {
		return notFoundSlot(stmt, CodeInvalidArgument, err.Error()+"\n`ls task` lists the live ones.")
	}
	v, gerr := s.tasks.Get(id)
	if gerr != nil {
		return notFoundSlot(stmt, CodeInvalidArgument, gerr.Error())
	}
	if !stopped {
		// Already finished: the caller asked to stop something that is not running, and
		// the honest answer names the state it is in instead of pretending to cancel.
		return envelope.Slot{Code: 409, Headers: map[string]string{"Path": stmt.Path, "Task": id},
			Body: "nothing to cancel: " + id + " is already " + string(v.Status) + "\n" + renderTask(v)}
	}
	return envelope.Slot{Code: 200, Headers: map[string]string{"Path": stmt.Path, "Task": id, "Status": "cancelled"},
		Body: "cancelled " + id + "; the partial file was removed at both ends\n" + renderTask(v)}
}

// renderTask is the one shape every task reply uses.
func renderTask(v task.View) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Task:     %s\n", v.ID)
	fmt.Fprintf(&b, "Kind:     %s\n", v.Kind)
	fmt.Fprintf(&b, "Status:   %s\n", v.Status)
	fmt.Fprintf(&b, "Progress: %s\n", taskProgress(v))
	if rate := v.Rate(); rate > 0 {
		fmt.Fprintf(&b, "Rate:     %s/s\n", humanBytes(rate))
	}
	fmt.Fprintf(&b, "Path:     %s -> %s\n", v.LocalPath, v.Path)
	if v.Error != "" {
		fmt.Fprintf(&b, "Error:    %s\n", v.Error)
	}
	return b.String()
}

// taskProgress renders "done/total (pct)" without inventing a total.
func taskProgress(v task.View) string {
	if v.BytesTotal <= 0 {
		return humanBytes(v.BytesDone) + " (total unknown)"
	}
	return fmt.Sprintf("%s/%s (%d%%)", humanBytes(v.BytesDone), humanBytes(v.BytesTotal), v.Percent())
}

// peerCallWindow chooses the deadline for a peer call.
//
// Defaults to 15s: short enough that a 30s host watchdog never kills the call
// from outside, long enough for any ordinary peer operation (a CDP evaluate, an
// MCP tool list).
//
// IT HAS TO ASK WHETHER THE CALLER SPOKE, not what the value came out as. `timeout` has a
// default of its own (2s, the read window), so `win.TimeoutSeconds` is never zero and the
// 15s branch below was unreachable from the term door: every peer call was bounded by 2
// seconds, which a cold tunnel through the internal SSH server can exceed — CI caught it
// as a 504 on an ordinary LSP frame. An explicit `timeout` still wins, including one
// shorter than the default; an explicit 0 means "do not wait", which is not a deadline a
// request/response call can use, so it falls back to the peer default.
const defaultPeerWindow = 15 * time.Second

func peerCallWindow(win termArgs) time.Duration {
	if win.TimeoutSet && win.TimeoutSeconds > 0 {
		return time.Duration(win.TimeoutSeconds * float64(time.Second))
	}
	return defaultPeerWindow
}

// namePeerTimeout turns a deadline expiry into a 504 that names the peer.
func namePeerTimeout(ctx context.Context, slot envelope.Slot) envelope.Slot {
	if ctx.Err() == context.DeadlineExceeded && (slot.Code == 500 || slot.Code == 0) {
		slot.Code = 504
		slot.Headers["Error-Code"] = "peer_timeout"
		slot.Body = "the peer did not answer within the window. `call` waited its full budget; " +
			"give the call a larger `timeout` if this method needs longer, or use `--send` for fire-and-forget."
	}
	return slot
}
