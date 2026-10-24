package webui

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// copyEmbeddedAssets writes the page out so file:// can load the REAL index.html,
// stylesheets and scripts.
//
// The alternative — assembling a fragment and a few rules — is what let this bug
// survive: a fragment can restate the ceiling and look fine, while the page's own
// chrome (banners, the trigger above each plate, the dock) is what pushes it over.
func copyEmbeddedAssets(t *testing.T, dst string) {
	t.Helper()
	root := Assets()
	err := fs.WalkDir(root, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		target := filepath.Join(dst, filepath.FromSlash(p))
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := fs.ReadFile(root, p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestNoColumnOutgrowsTheViewport pins issue #153.
//
// The invariant the whole page depends on — stated in workspace.css next to the
// ceiling it justifies — is that NO column may outgrow the viewport, so the page
// itself never scrolls. It was not true: the centre extension plate had no ceiling
// at all, and the ceilings the other two columns did have were `100dvh` arithmetic
// over two MEASURED values, so they were only as right as the measurement and one of
// those values (`--dock-h`) came from an element in the same layout.
//
// Measured on the real page before the fix, at 1400x900 with 80 extensions: the
// plate was 1456px, the page overflowed by 794px, and the dock was pushed below the
// viewport. On a platform whose scrollbars take width, the appearance of that
// scrollbar then narrows the page, which re-wraps the cards, which changes the
// height again — a layout that feeds back into its own inputs.
//
// The threshold is deliberately not a pixel budget: a column may not make the
// DOCUMENT taller than the window, and the panels' heights must be constant across
// samples rather than settling on some number.
func TestNoColumnOutgrowsTheViewport(t *testing.T) {
	chrome := findChrome()
	if chrome == "" {
		t.Skip("no Chrome or Edge installed; cannot measure the layout")
	}
	dir := t.TempDir()
	copyEmbeddedAssets(t, dir)
	index := filepath.Join(dir, "index.html")

	// The probe drives the page's OWN renderer, so it is the real list that gets
	// laid out — 80 rows is the repro in the issue, and a synthetic <div> would only
	// prove that a <div> is tall.
	script := `
(function () {
  function sample() {
    var frame = document.querySelector('.workspace-frame');
    var plate = document.getElementById('ext-plate');
    var panel = document.getElementById('extpanel-body');
    var doc = document.scrollingElement;
    return {
      docOver: doc.scrollHeight - doc.clientHeight,
      scrollbarW: window.innerWidth - document.documentElement.clientWidth,
      frameH: frame ? frame.getBoundingClientRect().height : -1,
      plateH: plate ? plate.getBoundingClientRect().height : -1,
      panelH: panel ? panel.getBoundingClientRect().height : -1,
      innerH: window.innerHeight,
      bodyH: getComputedStyle(document.body).height,
      bodyOverflow: getComputedStyle(document.body).overflow,
      dockOverflow: (function(){var d=document.querySelector('.dock'); return d?getComputedStyle(d).overflow:'-';})(),
      dockH: (function(){var d=document.querySelector('.dock'); return d?d.getBoundingClientRect().height:-1;})(),
      extMax: plate ? getComputedStyle(plate).maxHeight : '-',
      extDisplay: plate ? getComputedStyle(plate).display : '-',
      frameRows: frame ? getComputedStyle(frame).gridTemplateRows : '-',
      frameAlign: frame ? getComputedStyle(frame).alignItems : '-'
    };
  }
  setTimeout(function () {
    // The banners are the page's reaction to having no server under file://, not
    // layout, and they sit above the frame — hide them so the measurement is about
    // the columns.
    var banner = document.getElementById('conn-load-banner');
    if (banner) banner.style.display = 'none';
    var sb = document.getElementById('session-load-banner');
    if (sb) sb.style.display = 'none';

    var rows = [];
    for (var i = 0; i < 80; i++) rows.push({ name: 'ext-' + i, enabled: false, status: 'disabled', kind: 'mcp', target: 'internal' });
    if (typeof renderExtensionGrid === 'function') renderExtensionGrid(rows);

    var seen = [];
    var n = 0;
    var tick = setInterval(function () {
      // Re-render every tick: a socket frame does the same thing, and a layout that
      // only settles when nothing touches it is not settled.
      if (typeof renderExtensionGrid === 'function') renderExtensionGrid(rows);
      seen.push(sample());
      if (++n >= 24) {
        clearInterval(tick);
        document.title = 'RESULT::' + JSON.stringify(seen);
      }
    }, 40);
  }, 1200);
})();
`
	if err := os.WriteFile(filepath.Join(dir, "layoutprobe.js"), []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	page, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	page = []byte(strings.Replace(string(page), "</body>", `<script src="layoutprobe.js"></script></body>`, 1))
	if err := os.WriteFile(index, page, 0o600); err != nil {
		t.Fatal(err)
	}

	dom := renderedProbeDOMSized(t, chrome, index, 1400, 900)
	samples := layoutSamples(t, dom)
	if len(samples) < 12 {
		t.Fatalf("the probe came back with %d samples; it did not run", len(samples))
	}

	t.Logf("sample0: frame=%.0f plate=%.0f panel=%.0f vp=%.0f docOver=%.0f | body h=%s ov=%s | dock h=%.0f ov=%s | rows=%s align=%s | plate max=%s display=%s",
		samples[0].FrameH, samples[0].PlateH, samples[0].PanelH, samples[0].InnerH, samples[0].DocOver,
		samples[0].BodyH, samples[0].BodyOver, samples[0].DockH, samples[0].DockOver,
		samples[0].FrameRows, samples[0].FrameAlign, samples[0].ExtMax, samples[0].ExtDisplay)
	last := samples[len(samples)-1]
	for i, s := range samples {
		// 1px of slack: subpixel layout means scrollHeight and clientHeight can differ
		// by a rounding step without anything being wrong.
		if s.DocOver > 1 {
			t.Fatalf("sample %d: the page is %0.fpx taller than the viewport (columns must never outgrow it):\n  frame=%.0f plate=%.0f panel=%.0f viewport=%.0f",
				i, s.DocOver, s.FrameH, s.PlateH, s.PanelH, s.InnerH)
		}
	}
	// And it has to be STILL. A layout that reaches the right number by oscillating
	// between two of them passes the check above on the samples that land on one.
	for _, key := range []struct {
		name string
		get  func(layoutSample) float64
	}{
		{"frame", func(s layoutSample) float64 { return s.FrameH }},
		{"plate", func(s layoutSample) float64 { return s.PlateH }},
		{"panel", func(s layoutSample) float64 { return s.PanelH }},
	} {
		want := key.get(last)
		for i, s := range samples {
			if got := key.get(s); got != want {
				t.Fatalf("the %s height changed between samples (%0.f at %d, %0.f at the last one); it has to settle, not oscillate",
					key.name, got, i, want)
			}
		}
	}
}

// layoutSample is one measurement of the probe.
type layoutSample struct {
	BodyH      string  `json:"bodyH"`
	BodyOver   string  `json:"bodyOverflow"`
	DockOver   string  `json:"dockOverflow"`
	DockH      float64 `json:"dockH"`
	ExtMax     string  `json:"extMax"`
	ExtDisplay string  `json:"extDisplay"`
	FrameRows  string  `json:"frameRows"`
	FrameAlign string  `json:"frameAlign"`
	DocOver    float64 `json:"docOver"`
	ScrollbarW float64 `json:"scrollbarW"`
	FrameH     float64 `json:"frameH"`
	PlateH     float64 `json:"plateH"`
	PanelH     float64 `json:"panelH"`
	InnerH     float64 `json:"innerH"`
}

// layoutSamples pulls the measurements out of the title the probe left behind.
func layoutSamples(t *testing.T, dom string) []layoutSample {
	t.Helper()
	const marker = "RESULT::"
	i := strings.Index(dom, marker)
	if i < 0 {
		t.Fatalf("the probe did not report; the page never reached its sample loop")
	}
	rest := dom[i+len(marker):]
	end := strings.Index(rest, "</title>")
	if end < 0 {
		t.Fatalf("the probe's result is not a title: %.120q", rest)
	}
	var out []layoutSample
	if err := json.Unmarshal([]byte(htmlUnescape(rest[:end])), &out); err != nil {
		t.Fatalf("decoding the probe's samples: %v", err)
	}
	return out
}

// htmlUnescape undoes the entity escaping the serializer applied to the title.
func htmlUnescape(s string) string {
	r := strings.NewReplacer("&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`, "&#34;", `"`, "&#39;", "'")
	return r.Replace(s)
}
