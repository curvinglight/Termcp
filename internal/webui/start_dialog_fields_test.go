package webui

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestTheLaunchDialogKeepsItsConnectButtonForASerialProfile runs openStartModal()
// under node.
//
// A serial profile takes no command and no shell mode, so the dialog hides those
// two fields. It hid `parentElement` — and in that dialog a field is a bare
// <input>/<select> beside its own <label>, both direct children of .modal-body.
// So the body itself was hidden: what the user got was the title, a Close button,
// and nothing else — no name field, no Connect button, no way to start anything.
//
// The rule this pins: hiding a field hides the field and its label, never the
// container that also holds the actions.
func TestTheLaunchDialogKeepsItsConnectButtonForASerialProfile(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; cannot exercise the launch dialog")
	}
	body := readAssetLF(t, "static/js/conn-form.js")
	var src strings.Builder
	for _, fn := range []string{"function hideStartField(el, hide)", "function openStartModal(connName)"} {
		start := strings.Index(body, fn)
		if start < 0 {
			t.Fatalf("conn-form.js no longer defines %s", fn)
		}
		end := strings.Index(body[start:], "\n}\n")
		if end < 0 {
			t.Fatalf("could not delimit %s", fn)
		}
		src.WriteString(body[start : start+end+2])
		src.WriteString("\n")
	}

	tmp := t.TempDir()
	srcPath := filepath.Join(tmp, "start.js")
	if err := os.WriteFile(srcPath, []byte(src.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	harness := `
const fs = require('fs');
const vm = require('vm');
const sandbox = { console };
vm.createContext(sandbox);
vm.runInContext(String.raw` + "`" + `
var window = { _lastConnections: [] };
var els = {}, labels = {};
function el(id) {
  if (!els[id]) els[id] = { id: id, style: { display: '' }, value: '', textContent: '' };
  return els[id];
}
function label(id) {
  if (!labels[id]) labels[id] = { tagName: 'LABEL', style: { display: '' } };
  return labels[id];
}
var document = {
  getElementById: function (id) { return el(id); },
  querySelector: function (sel) {
    var m = /label\[for="([^"]+)"\]/.exec(sel);
    return m ? label(m[1]) : null;
  }
};
function t(k) { return k; }
function showModal() {}
function isSerialProfile(name) {
  return !!window._lastConnections.find(function (c) { return c.name === name && c.kind === 'serial'; });
}
` + "`" + `, sandbox);
// The connect button is never touched by openStartModal, so it has to exist
// before the open to prove the open left it alone.
vm.runInContext("el('start-run')", sandbox);
vm.runInContext(fs.readFileSync(process.argv[2], 'utf8'), sandbox);

let bad = 0;
function eq(name, got, want) {
  if (got !== want) { console.log('FAIL ' + name + ': got ' + JSON.stringify(got) + ' want ' + JSON.stringify(want)); bad++; }
  else console.log('PASS ' + name);
}
function disp(id) { return sandbox.els[id] ? sandbox.els[id].style.display : 'MISSING'; }
function labDisp(id) { return sandbox.labels[id] ? sandbox.labels[id].style.display : 'MISSING'; }

vm.runInContext("window._lastConnections = [{name:'office',kind:'remote'},{name:'com4',kind:'serial'}]", sandbox);

// An SSH host: every field of the dialog is offered.
vm.runInContext("openStartModal('office')", sandbox);
eq('ssh keeps the command field', disp('start-cmd'), '');
eq('ssh keeps the mode field', disp('start-mode'), '');
eq('ssh keeps their labels', labDisp('start-cmd') + '/' + labDisp('start-mode'), '/');
eq('ssh keeps the name field', disp('start-name'), '');
eq('ssh keeps the connect button', disp('start-run'), '');

// A serial profile: the two fields go, and NOTHING ELSE does.
vm.runInContext("openStartModal('com4')", sandbox);
eq('serial hides the command field', disp('start-cmd'), 'none');
eq('serial hides the mode field', disp('start-mode'), 'none');
eq('serial hides their labels too', labDisp('start-cmd') + '/' + labDisp('start-mode'), 'none/none');
eq('serial KEEPS the name field', disp('start-name'), '');
eq('serial KEEPS the connect button', disp('start-run'), '');
eq('and the error line is cleared, not hidden with the body', disp('modal-start-err'), 'none');

// Back to an SSH host: the fields come back.
vm.runInContext("openStartModal('office')", sandbox);
eq('switching back restores the command field', disp('start-cmd'), '');
eq('switching back restores its label', labDisp('start-cmd'), '');

process.exit(bad === 0 ? 0 : 1);
`
	harnessPath := filepath.Join(tmp, "start_test.js")
	if err := os.WriteFile(harnessPath, []byte(harness), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, harnessPath, srcPath).CombinedOutput()
	if strings.TrimSpace(string(out)) != "" {
		t.Logf("node said:\n%s", out)
	}
	if err != nil {
		t.Fatalf("the launch dialog hides its own connect button: %v", err)
	}
}
