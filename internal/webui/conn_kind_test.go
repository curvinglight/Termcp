package webui

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestANewProfileCanBecomeASerialPort exercises the profile dialog's kind picker.
//
// The dialog was reachable only as an SSH host: `openConnModal(false, "")` ->
// editingKind "remote", and nothing in the markup could change it. A serial
// profile could be pasted in the config view, but the form — the only path a
// user takes — never offered the choice, so on a fresh install there was no way
// to create one. The picker is now built into the form (index.html is at its
// byte budget) and this test drives the same gesture a user makes: switch the
// picker to serial, watch the SSH half hide and the device half appear.
//
// The second rule here is the one that keeps the two halves honest: the picker
// is only offered while CREATING. A profile's kind is fixed once the document
// exists, and switching it in the form would leave the other kind's fields in
// the TOML for the server to refuse.
func TestANewProfileCanBecomeASerialPort(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; cannot exercise the profile dialog")
	}
	body := readAssetLF(t, "static/js/conn-form.js")
	var src strings.Builder
	for _, fn := range []string{"function ensureKindField()", "function applyConnKindView()"} {
		start := strings.Index(body, fn)
		if start < 0 {
			t.Fatalf("conn-form.js no longer defines %s; the dialog cannot pick a kind", strings.TrimPrefix(strings.TrimSuffix(fn, "()"), "function "))
		}
		end := strings.Index(body[start:], "\n}\n")
		if end < 0 {
			t.Fatalf("could not delimit %s", fn)
		}
		src.WriteString(body[start : start+end+2])
		src.WriteString("\n")
	}

	script, err := os.CreateTemp("", "conn-kind-*.js")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(script.Name())
	harness := `
const fs = require('fs');
const vm = require('vm');

const src = fs.readFileSync(process.argv[2], 'utf8');
const sandbox = { console };
vm.createContext(sandbox);
// A DOM just big enough for the two functions: getElementById answers from a
// registry, createElement hands back a node that registers itself when the code
// inserts it, and the fields the real markup owns are pre-registered.
vm.runInContext(String.raw` + "`" + `
var editingKind = 'remote';
var editingConnName = '';
var chipsLoaded = 0;
var registry = {};
function fakeEl(id) {
  return {
    id: id, style: { display: '' }, value: '', className: '', innerHTML: '',
    addEventListener: function (ev, fn) { this._on = this._on || {}; this._on[ev] = fn; },
    closest: function () { return need('conn-f-shell-row'); },
    parentNode: { insertBefore: function (n) {
      // The markup inside an inserted node is part of the page too: the picker
      // is written as innerHTML and must answer getElementById right after.
      if (n.id) registry[n.id] = n;
      String(n.innerHTML || '').replace(/id="([^"]+)"/g, function (_, id) {
        if (!registry[id]) registry[id] = fakeEl(id);
        return '';
      });
    } }
  };
}
function need(id) { if (!registry[id]) registry[id] = fakeEl(id); return registry[id]; }
var document = {
  getElementById: function (id) { return registry[id] || null; },
  createElement: function () { return fakeEl(''); }
};
function t(k) { return k; }
function _hesc(s) { return String(s); }
var serialBuilt = 0;
function ensureSerialFields() { serialBuilt++; need('conn-f-serial-fields'); }
function loadSerialPortChips() { chipsLoaded++; }
need('conn-f-remote-fields');
need('conn-f-remote-extra');
need('conn-f-shell');
need('conn-f-shell-row');
` + "`" + `, sandbox);
vm.runInContext(src, sandbox);

let bad = 0;
function eq(name, got, want) {
  if (got !== want) { console.log('FAIL ' + name + ': got ' + JSON.stringify(got) + ' want ' + JSON.stringify(want)); bad++; }
  else console.log('PASS ' + name);
}
function disp(id) {
  const e = sandbox.registry[id];
  return e ? e.style.display : 'MISSING';
}
function kind() { return vm.runInContext('editingKind', sandbox); }

vm.runInContext('ensureKindField()', sandbox);
eq('the picker is in the form', disp('conn-f-kind-wrap'), '');
eq('the picker offers a serial profile', String(sandbox.registry['conn-f-kind-wrap'].innerHTML).indexOf('value="serial"') >= 0, true);

// What the dialog shows the moment it opens for a new profile.
vm.runInContext('applyConnKindView()', sandbox);
eq('a new profile starts on the SSH half', disp('conn-f-remote-fields'), '');
eq('with no device fields yet', disp('conn-f-serial-fields'), 'MISSING');
eq('and the picker still offered', disp('conn-f-kind-wrap'), '');

// The gesture: pick serial.
const sel = sandbox.registry['conn-f-kind'];
sel.value = 'serial';
sel._on.change.call(sel);
eq('the picker sets the kind', kind(), 'serial');
eq('the device half appears', disp('conn-f-serial-fields'), '');
eq('the SSH half hides', disp('conn-f-remote-fields'), 'none');
eq('so does the dial path', disp('conn-f-remote-extra'), 'none');
eq('and the default shell row', disp('conn-f-shell-row'), 'none');
eq('the port chips are fetched', vm.runInContext('chipsLoaded', sandbox), 1);
eq('the picker stays for a create', disp('conn-f-kind-wrap'), '');

// And back: the halves are one switch, not two independent ones.
sel.value = 'remote';
sel._on.change.call(sel);
eq('switching back restores the SSH half', disp('conn-f-remote-fields'), '');
eq('and hides the device half', disp('conn-f-serial-fields'), 'none');

// An existing profile: the kind is a fact about the document, not a choice.
vm.runInContext("editingConnName = 'office'; editingKind = 'remote'; applyConnKindView()", sandbox);
eq('editing hides the picker', disp('conn-f-kind-wrap'), 'none');
vm.runInContext("editingKind = 'serial'; applyConnKindView()", sandbox);
eq('editing a serial profile shows its half', disp('conn-f-serial-fields'), '');
eq('and still no picker', disp('conn-f-kind-wrap'), 'none');

process.exit(bad === 0 ? 0 : 1);
`
	if err := os.WriteFile(script.Name(), []byte(harness), 0o600); err != nil {
		t.Fatal(err)
	}

	assets := filepath.Join(t.TempDir(), "conn-kind-src.js")
	if err := os.WriteFile(assets, []byte(src.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, script.Name(), assets)
	out, err := cmd.CombinedOutput()
	if strings.TrimSpace(string(out)) != "" {
		t.Logf("node said:\n%s", out)
	}
	if err != nil {
		t.Fatalf("the profile dialog cannot reach a serial profile: %v", err)
	}
}
