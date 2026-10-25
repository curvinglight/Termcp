package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A FLAG THAT CANNOT BE HONOURED IS REFUSED, NOT DROPPED (#190).
//
// `--mode` was validated on the `--data` path and IGNORED on the `--local-path` one:
// the same `--mode 0755` answered 500 on the first and 202 "done" on the second, and
// the file landed with the default permissions. The caller who meant "make this script
// executable" then saw `open --argv` fail with "permission denied" and went looking at
// mounts, users and paths — the parameter had been dropped in silence.
func TestAModeTheWriteCannotHonourIsRefusedOnBothPaths(t *testing.T) {
	s := newTestServer(t)
	sid := internalSession(t, s)
	dir := t.TempDir()
	local := filepath.Join(dir, "x.sh")
	if err := os.WriteFile(local, []byte("#!/bin/sh\necho hi\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, cmd := range []string{
		"write " + sid + " file /tmp/mode-a.sh --data hello --mode 0755",
		"write " + sid + " file /tmp/mode-b.sh --local-path " + local + " --mode 0755",
	} {
		t.Run(cmd, func(t *testing.T) {
			got := termText(t, s, map[string]any{"cmd": cmd})
			if !statusLineIs(got, "400") {
				t.Fatalf("a mode this write cannot honour must be refused, not dropped:\n%s", got)
			}
			if !strings.Contains(got, "mode") {
				t.Errorf("the refusal did not name the flag:\n%s", got)
			}
		})
	}
}

// AND A LOCAL FILE IS NOT DECODED, so a decoding mode says so instead of pretending
// to apply. Without this the flag was accepted and did nothing, which is the shape a
// caller cannot tell from success.
func TestADecodingModeOnALocalFileIsRefused(t *testing.T) {
	s := newTestServer(t)
	sid := internalSession(t, s)
	dir := t.TempDir()
	local := filepath.Join(dir, "x.sh")
	if err := os.WriteFile(local, []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := termText(t, s, map[string]any{
		"cmd": "write " + sid + " file /tmp/dec.sh --local-path " + local + " --mode hex",
	})
	if !statusLineIs(got, "400") {
		t.Fatalf("--mode hex on a local file does nothing and must say so:\n%s", got)
	}
	if !strings.Contains(got, "--mode file") {
		t.Errorf("the refusal did not name the mode that would apply:\n%s", got)
	}
}

// NO MODE IS THE ORDINARY CASE, and it has to stay accepted: an explicit "text" on a
// --local-path write is a flag that cannot be honoured, while saying nothing is what
// every caller does. Spelling the default as "text" made the two indistinguishable.
func TestALocalWriteWithoutAModeIsStillAccepted(t *testing.T) {
	s := newTestServer(t)
	sid := internalSession(t, s)
	dir := t.TempDir()
	local := filepath.Join(dir, "x.sh")
	if err := os.WriteFile(local, []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := termText(t, s, map[string]any{
		"cmd": "write " + sid + " file /tmp/plain.sh --local-path " + local,
	})
	if !statusLineIs(got, "202") {
		t.Fatalf("a local write with no --mode is the ordinary case:\n%s", got)
	}
}

// STAT HAS TO ANSWER "IS THIS EXECUTABLE" (#191). A script that had just been written
// failed to run, and every surface that could have said "mode 664" reported size and
// mtime instead: the caller went looking at mounts, users and paths while the fact sat
// in the stat response nobody had asked for.
func TestStatReportsTheModeOfAFile(t *testing.T) {
	s := newTestServer(t)
	sid := internalSession(t, s)
	// THE PATH IS THE TEST'S OWN TEMP DIR, not /tmp. On a Windows runner /tmp resolves to
	// D:	mp, which does not exist, so the write was refused with "the directory /tmp does
	// not exist" — a green local suite (this machine has C:	mp) and a red windows leg.
	// t.TempDir is the repo's rule for exactly this reason: never hardcode a shared
	// absolute path.
	path := filepath.Join(t.TempDir(), "stat-mode-probe.sh")
	if got := termText(t, s, map[string]any{
		"cmd": "write " + sid + " file " + path + " --data hi",
	}); !statusLineIs(got, "200") {
		t.Fatalf("write:\n%s", got)
	}
	defer termText(t, s, map[string]any{"cmd": "rm " + sid + " file " + path})

	got := termText(t, s, map[string]any{
		"cmd": "get " + sid + " file " + path + " --op stat",
	})
	if !statusLineIs(got, "200") {
		t.Fatalf("stat:\n%s", got)
	}
	// Four octal digits, e.g. "0644": the spelling a caller passes to chmod.
	if !strings.Contains(got, "mode: 0") {
		t.Fatalf("stat did not report the file's mode:\n%s", got)
	}
}

// A LOCAL PATH IS RESOLVED, AND THE RESOLUTION IS WHAT AN ERROR NAMES (#192).
//
// A relative `--local-path` is read by the DAEMON, so it is relative to the daemon's
// working directory — not the caller's. The error used to quote the path the caller
// TYPED, so the first reading was "but the file is right there".
func TestARelativeLocalPathSaysWhereItLooked(t *testing.T) {
	s := newTestServer(t)
	sid := internalSession(t, s)
	got := termText(t, s, map[string]any{
		"cmd": "write " + sid + " file /tmp/nowhere.sh --local-path ./no-such-dir/x.sh",
	})
	if !statusLineIs(got, "202") {
		t.Fatalf("a local-file write answers with a receipt:\n%s", got)
	}
	taskID := headerValueOr(got, "Task", "")
	if taskID == "" {
		t.Fatalf("the receipt carries no task id:\n%s", got)
	}
	final := termText(t, s, map[string]any{"cmd": "get task " + taskID, "timeout": 10})
	if !strings.Contains(final, "no-such-dir") {
		t.Fatalf("the failure lost the path:\n%s", final)
	}
	// An absolute spelling on either platform: a drive letter, or a leading slash.
	if !strings.Contains(final, ":\\") && !strings.Contains(final, ":/") && !strings.Contains(final, " /") {
		t.Fatalf("the failure did not name an absolute path, so the caller cannot tell what was tried:\n%s", final)
	}
}
