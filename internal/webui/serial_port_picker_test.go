package webui

import (
	"strings"
	"testing"
)

// TestSerialPortChipsAreWired checks the visible half of the device picker is
// actually hooked up: markup in index.html, rendering and click handling in
// conn-form.js. The datalist alone is invisible until the field is focused, so
// a chip row that was declared but never filled would look identical to a
// picker that was never built — at runtime, and to every other test here.
func TestSerialPortChipsAreWired(t *testing.T) {
	index := readAssetLF(t, "index.html")
	if !strings.Contains(index, `id="conn-f-device-chips"`) {
		t.Error("index.html has no #conn-f-device-chips row for the device field")
	}

	body := readAssetLF(t, "static/js/conn-form.js")
	block := between(t, body, "var _serialPortsCache", "function _setSelect")
	for _, want := range []string{
		"document.getElementById('conn-f-device-chips')",
		"_renderSerialDeviceChips();",
		`class="conn-port-chip"`,
		// The empty enumeration needs an answer of its own, in every language
		// the catalog carries (i18n_test.go polices the catalog side).
		"modal.conn.field.deviceNone",
		// The placeholder is platform text: COM3 on Windows, a by-id path
		// everywhere else.
		"navigator.platform",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("conn-form.js device picker lost %q", want)
		}
	}
}
