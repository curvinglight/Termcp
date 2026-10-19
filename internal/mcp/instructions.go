package mcp

// mcpServerInstructions is returned in initialize (MCP "instructions") so clients may
// inject it into the model context. Keep it terse because clients may include it
// in every model turn. It is a fixed rule set and never carries this instance's
// address: `instructions` is optional in MCP and often discarded, so the address
// rides on the `term` tool description instead (see decorateTools).
//
// There is one tool (`term`) whose argument is a command line, so these rules are
// about the COMMAND SPACE: how to get bearings, how to address a channel, how to
// read a window, and the hard boundary around asking a human. The directive
// table is not restated here — `help` and `learn_mode` deliver it at the moment
// it is needed, which is what lets the surface grow without this text growing.
//
// Rule 6 is the one rule about review mode that belongs here: the reply it
// describes (review_pending) answers a call the model just made, so the rule has
// to be in context before that call. The tool RESULT carries the operational half
// at the moment it matters (see reviewPendingReply); that repetition is
// deliberate, because a model that meets the reply mid-session reads the reply,
// not a rule from the start of the session.
const mcpServerInstructions = `termcp agent rules:

0) One tool: term(cmd), a terminal line: <directive> <handle> <payload>. No cmd = bearings (syntax, what runs, what is installed); help lists every directive, help <directive> teaches one. Guides: api.md.
1) Handles: <session>[:N] = a connection, optionally a channel in it. Never invent one; copy the first column of ps or ext list. termcp:// locators work verbatim wherever a handle does.
2) Mode: interactive shell (pty, the default) for multi-step work; drive it with run/type/key and read with read. A read returns ONLY new bytes: empty != done, keep reading. pipe --argv ONLY for a program that runs to exit; never split steps across separate opens (loses cwd/env). pipe internal = the termcp host's own terminal (local, no SSH).
3) A reply is TERM/1: status line, headers only when meaningful (Path, Status, Exit-Code, Handle, Content-Range, Error-Code, Usage), then the raw body. code>=400 = isError. Content-Range end is inclusive: end+1 is the next --cursor.
4) Windows bound the reply, never the command: timeout (wait), bytes_limit/lines_limit (one slot), total_bytes_limit (whole reply). bytes_limit=0 returns the handle and consumes nothing. Truncation is 206, not a failure.
5) After discovery act with concrete calls, not prose; verify via output or an explicit success field.
6) Lifecycle: close <handle> closes one channel, the connection and its forwards/files stay; close <session> --force terminates it (entry stays as exited, output still readable); rm <session> erases it.
7) Human in the loop (HARD BOUNDARY): notify FIRST (--level warn|error --sticky --on <session>) before any password/sudo/passphrase/MFA, confirmation, approval, choice or interactive input, and say it in your reply. Never guess, paste, echo or store secrets; never bypass sudo/auth/approval (sudo -n, NOPASSWD/setuid/sudoers, reword, encode, wrap, retry loops).
8) review_pending=true: held, nothing ran, no id to poll - do NOT resubmit, reword or retry it. notify (rule 7), then WAIT (not a stall): staged text queues when its ending key arrives, and read later shows whether it ran.
9) Other keys use JSON \u001b escapes in type. Repeating traceback: close --force, retry with PYTHON_BASIC_REPL=1. Silent hang: info <handle>.
10) fwd --action local|remote|dynamic = ssh -L/-R/-D. profile --action list returns names only; never expose credentials.
11) watch registers async wake-up rules (--event output|exit|silence); read when woken. watch wakes the Agent, notify the human. Human at a prompt: notify, then watch --event output on that handle.`
