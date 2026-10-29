package webui

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestASerialCardClickAsksForNoTerminalSize runs sessionStartBody() under node.
//
// The card's one-click connect passes no options at all, and the body it built
// always carried rows:24 cols:80 — a pty's shape. The server refuses a size on a
// serial session rather than discarding it (a device has no terminal, so a size is
// a request it cannot honour), so every click on a serial card came back "a serial
// device has no terminal to size": a refusal naming fields the user never chose,
// on the one gesture the whole connection panel exists for.
//
// The kind comes from the connection list the panel already holds, which is the
// only place the page knows it without asking the server mid-click.
func TestASerialCardClickAsksForNoTerminalSize(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; cannot exercise the start body")
	}
	var src strings.Builder
	for _, part := range []struct{ file, fn string }{
		{"static/js/util.js", "function connProfileKind(name)"},
		{"static/js/util.js", "function isSerialProfile(name)"},
		{"static/js/terminal-window.js", "function sessionStartBody(connName, opt)"},
	} {
		body := readAssetLF(t, part.file)
		start := strings.Index(body, part.fn)
		if start < 0 {
			t.Fatalf("%s no longer defines %s", part.file, part.fn)
		}
		end := strings.Index(body[start:], "\n}\n")
		if end < 0 {
			t.Fatalf("could not delimit %s", part.fn)
		}
		src.WriteString(body[start : start+end+2])
		src.WriteString("\n")
	}

	tmp := t.TempDir()
	srcPath := filepath.Join(tmp, "body.js")
	if err := os.WriteFile(srcPath, []byte(src.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	harness := `
const fs = require('fs');
const vm = require('vm');
const sandbox = { console };
vm.createContext(sandbox);
vm.runInContext('var window = { _lastConnections: [] };', sandbox);
vm.runInContext(fs.readFileSync(process.argv[2], 'utf8'), sandbox);

let bad = 0;
function check(name, ok, detail) {
  if (ok) { console.log('PASS ' + name); return; }
  console.log('FAIL ' + name + (detail ? ': ' + detail : '')); bad++;
}

// The profile list as GET /api/connections answers it.
vm.runInContext(` + "`" + `window._lastConnections = [
  { name: 'office', kind: 'remote' },
  { name: 'com4', kind: 'serial' }
];` + "`" + `, sandbox);

const ssh = vm.runInContext("sessionStartBody('office', {})", sandbox);
check('an SSH host still gets its size', ssh.rows === 24 && ssh.cols === 80, JSON.stringify(ssh));

const serial = vm.runInContext("sessionStartBody('com4', {})", sandbox);
check('a serial card asks for no size', !('rows' in serial) && !('cols' in serial), JSON.stringify(serial));
check('and no command, mode or args', serial.command === '' && serial.mode === '' && Array.isArray(serial.args) && serial.args.length === 0, JSON.stringify(serial));
check('the target still travels', serial.ssh_config === 'com4', JSON.stringify(serial));
check('unnamed launches default to the profile name', serial.name === 'com4', JSON.stringify(serial));

// The launch-options dialog names the session itself.
const named = vm.runInContext("sessionStartBody('com4', { name: 'board', command: '', mode: '' })", sandbox);
check('a typed name wins', named.name === 'board', JSON.stringify(named));
check('and the serial profile still gets no size', !('rows' in named), JSON.stringify(named));

// An unknown name is not serial: guessing "serial" for it would strip a size from
// a host that needs one.
const unknown = vm.runInContext("sessionStartBody('never-seen', {})", sandbox);
check('an unknown profile keeps the pty size', unknown.rows === 24 && unknown.cols === 80, JSON.stringify(unknown));

process.exit(bad === 0 ? 0 : 1);
`
	harnessPath := filepath.Join(tmp, "body_test.js")
	if err := os.WriteFile(harnessPath, []byte(harness), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, harnessPath, srcPath).CombinedOutput()
	if strings.TrimSpace(string(out)) != "" {
		t.Logf("node said:\n%s", out)
	}
	if err != nil {
		t.Fatalf("a serial card click still asks for a terminal size: %v", err)
	}
}
