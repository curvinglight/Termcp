package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestSessionsFrameCarriesBothProfileKinds pins the single source for the
// entry grid. The WebSocket sessions frame used to be built from the SSH store
// alone, so a profile save broadcast a list with the serial cards left out: the
// grid repainted without them, and they only reappeared on a language switch —
// which repaints from the HTTP snapshot instead. Both surfaces now go through
// one builder, and this asserts they return the same thing from it.
func TestSessionsFrameCarriesBothProfileKinds(t *testing.T) {
	e := newSerialEnv(t)
	if err := e.ssh.Save("hosta", []byte(remoteTOML())); err != nil {
		t.Fatal(err)
	}
	if err := e.serial.Save("esp32", []byte(serialTOML(serialDeviceFor()))); err != nil {
		t.Fatal(err)
	}

	c := &uiWS{h: e.handler, ctx: context.Background(), send: make(chan []byte, 1)}
	c.enqueueSessions()

	var raw []byte
	select {
	case raw = <-c.send:
	case <-time.After(2 * time.Second):
		t.Fatal("enqueueSessions sent no frame")
	}

	var frame map[string]json.RawMessage
	if err := json.Unmarshal(raw, &frame); err != nil {
		t.Fatalf("frame %q does not parse: %v", raw, err)
	}
	if got := string(frame["type"]); got != `"sessions"` {
		t.Errorf("frame type = %s, want \"sessions\"", got)
	}
	// A missing key leaves the grid untouched and a null one paints it empty;
	// neither is the list the HTTP route just served, so both fail here.
	conns, ok := frame["connections"]
	if !ok {
		t.Fatalf("frame carries no connections key: %s", raw)
	}
	if string(conns) == "null" {
		t.Fatalf("frame connections is null; the grid would repaint as empty: %s", raw)
	}
	var fromWS []map[string]any
	if err := json.Unmarshal(conns, &fromWS); err != nil {
		t.Fatalf("frame connections: %v", err)
	}

	rr := e.do(t, http.MethodGet, "/api/connections", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", rr.Code, rr.Body.String())
	}
	var fromHTTP struct {
		Connections []map[string]any `json:"connections"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &fromHTTP); err != nil {
		t.Fatalf("http list: %v", err)
	}
	if !reflect.DeepEqual(fromWS, fromHTTP.Connections) {
		t.Errorf("the WS frame and GET /api/connections disagree:\n ws:   %v\n http: %v",
			fromWS, fromHTTP.Connections)
	}

	kinds := map[string]string{}
	for _, m := range fromWS {
		name, _ := m["name"].(string)
		kind, _ := m["kind"].(string)
		kinds[name] = kind
	}
	if kinds["esp32"] != "serial" {
		t.Errorf("esp32 kind = %q, want serial — the frame dropped the serial store", kinds["esp32"])
	}
	if kinds["hosta"] != "remote" {
		t.Errorf("hosta kind = %q, want remote", kinds["hosta"])
	}
}

// TestSessionsFrameRepaintSyncsTheSnapshot guards the client half of the same
// fix. renderConnGrid draws the list, but a language switch repaints from
// window._lastConnections: if the frame only redraws, the two snapshots drift
// apart and the next repaint shows a different set of profiles than the one on
// screen — which is how a refresh lost the serial cards while a language
// switch brought them back.
func TestSessionsFrameRepaintSyncsTheSnapshot(t *testing.T) {
	body := readAssetLF(t, "static/js/ui-socket.js")
	block := between(t, body, "if (j.type === 'sessions')", "else if (j.type === 'terminal')")

	atSync := strings.Index(block, "window._lastConnections = j.connections;")
	atDraw := strings.Index(block, "renderConnGrid(")
	if atSync < 0 {
		t.Error("the sessions frame no longer refreshes window._lastConnections; a language switch would repaint from the previous load")
	}
	if atDraw < 0 {
		t.Error("the sessions frame no longer repaints the connection grid")
	}
	if atSync >= 0 && atDraw >= 0 && atSync > atDraw {
		t.Errorf("the snapshot is updated after the redraw (offset %d > %d); the repaint uses the stale list", atSync, atDraw)
	}
}
