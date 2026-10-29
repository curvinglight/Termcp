package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// TestFileToolURLsAddressTheCallerNotTheBindAddress pins that the URLs handed
// back by file_stat and file_urls point at the instance THE CALLER REACHED.
//
// They used to be built from s.baseURL, which Start() derives from the listen
// address and rewrites to 127.0.0.1 whenever the bind host is 0.0.0.0 -- the
// ordinary way to run this on a machine others need. Measured on a remote
// deployment bound to 0.0.0.0:18765, `get <session> file <path> --op stat`
// answered
//
//	download_url: http://127.0.0.1:18765/api/sessions/<sid>/files/download?path=…
//
// which works only from the daemon's own console. Every other caller gets
// "connection refused", and that reads as "the server is down" rather than "this
// URL is addressed to somebody else" -- the caller has no reason to suspect the
// address it was just handed.
//
// The origin rides in on the request (see originFor), so a request that named a
// host must be answered with that host.
func TestFileToolURLsAddressTheCallerNotTheBindAddress(t *testing.T) {
	s, sid, _, _, _ := locatorFixture(t)
	// The fallback: this is what the old code used unconditionally.
	s.baseURL = "http://127.0.0.1:0"

	dir := t.TempDir()
	probe := filepath.Join(dir, "probe.txt")
	if err := os.WriteFile(probe, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The address a caller on the LAN actually dialed.
	req := httptest.NewRequest(http.MethodPost, "/api/term", nil)
	req.Host = "172.16.13.126:18765"
	ctx := withRequestOrigin(context.Background(), req)

	for _, tc := range []struct {
		what   string
		handle func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error)
	}{
		{"file_stat", s.handleFileStat},
		{"file_urls", s.handleGetFileURLs},
	} {
		res, err := tc.handle(ctx, makeRequest(map[string]any{
			"session_id":  sid,
			"remote_path": probe,
		}))
		if err != nil || res.IsError {
			t.Fatalf("%s failed: %v %v", tc.what, err, res)
		}
		body := parseResult(t, res)
		for _, key := range []string{"download_url", "upload_url"} {
			u, _ := body[key].(string)
			if !strings.HasPrefix(u, "http://172.16.13.126:18765/api/sessions/") {
				t.Errorf("%s %s = %q, want it to start with the address the caller dialed", tc.what, key, u)
			}
			if strings.Contains(u, "127.0.0.1") {
				t.Errorf("%s %s = %q still carries the loopback fallback", tc.what, key, u)
			}
		}
	}

	// A proxy that recorded the original host wins over the one it rewrote to,
	// because the outside name is the one that resolves for the caller.
	req2 := httptest.NewRequest(http.MethodPost, "/api/term", nil)
	req2.Host = "10.0.0.9:8080"
	req2.Header.Set("X-Forwarded-Host", "termcp.example.test")
	res, err := s.handleGetFileURLs(withRequestOrigin(context.Background(), req2),
		makeRequest(map[string]any{"session_id": sid, "remote_path": probe}))
	if err != nil || res.IsError {
		t.Fatalf("file_urls behind a proxy failed: %v %v", err, res)
	}
	if got, _ := parseResult(t, res)["download_url"].(string); !strings.HasPrefix(got, "http://termcp.example.test/api/sessions/") {
		t.Errorf("download_url = %q, want the X-Forwarded-Host the caller knows", got)
	}

	// AND WITHOUT AN ORIGIN NOTHING CHANGES. An in-process call names no host, and
	// the bind address is the only answer available -- inventing one would be
	// worse than the fallback it replaces.
	res, err = s.handleGetFileURLs(context.Background(),
		makeRequest(map[string]any{"session_id": sid, "remote_path": probe}))
	if err != nil || res.IsError {
		t.Fatalf("in-process file_urls failed: %v %v", err, res)
	}
	if got, _ := parseResult(t, res)["download_url"].(string); !strings.HasPrefix(got, "http://127.0.0.1:0/api/sessions/") {
		t.Errorf("download_url = %q, want the bind-address fallback when no request named a host", got)
	}
}
