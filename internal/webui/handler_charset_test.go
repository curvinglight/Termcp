package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshserver"
	"github.com/open-mcp-ai/termcp/internal/storage"
)

// The charset selector's REST half: a legal name switches the session's decode
// (and the reply echoes what is now in force), a typo'd name is refused rather
// than silently turning decoding off, and an unknown session is a 404.
func TestSessionCharsetPatch(t *testing.T) {
	if testing.Short() {
		t.Skip("starts an SSH server")
	}
	srv := sshserver.New()
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	store := storage.New(dir)
	msgMgr := message.NewManager(store)
	sessMgr := session.NewManager(msgMgr, store, srv)
	t.Cleanup(func() {
		for _, s := range sessMgr.ListAll() {
			_ = sessMgr.Delete(s.ID)
		}
		srv.Stop()
	})
	sess, err := sessMgr.Create(session.Config{Mode: "pty", Rows: 24, Cols: 80})
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{Sessions: sessMgr}
	mux := http.NewServeMux()
	h.Register(mux)

	patch := func(body, sid string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("PATCH", "/api/sessions/"+sid+"/charset", strings.NewReader(body))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	sid := sess.ID
	if rec := patch(`{"charset":"gbk"}`, sid); rec.Code != http.StatusOK {
		t.Fatalf("PATCH gbk: %d %s", rec.Code, rec.Body.String())
	}
	// The session info now names the charset in force — this is the field the
	// window's selector renders its current selection from.
	req := httptest.NewRequest("GET", "/api/sessions/"+sid, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), `"charset":"gbk"`) {
		t.Fatalf("session info does not carry the override: %s", rec.Body.String())
	}

	if rec := patch(`{"charset":"gbkrate"}`, sid); rec.Code != http.StatusBadRequest {
		t.Fatalf("PATCH gbkrate: %d, want 400", rec.Code)
	}
	// auto and "" are legal: one re-arms the sniff, one returns to the profile.
	for _, legal := range []string{`{"charset":"auto"}`, `{"charset":""}`} {
		if rec := patch(legal, sid); rec.Code != http.StatusOK {
			t.Errorf("PATCH %s: %d", legal, rec.Code)
		}
	}
	if rec := patch(`{"charset":"gbk"}`, sid); rec.Code != http.StatusOK {
		t.Fatalf("PATCH gbk again: %d", rec.Code)
	}
	if rec := patch(`{"charset":"gbk"}`, "nope"); rec.Code != http.StatusNotFound {
		t.Fatalf("PATCH unknown session: %d, want 404", rec.Code)
	}
}
