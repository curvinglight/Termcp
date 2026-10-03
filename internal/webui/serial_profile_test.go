package webui

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/serial"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
	"github.com/open-mcp-ai/termcp/internal/storage"
)

// serialEnv is the Web UI wired to both profile stores, the shape main.go
// builds: one mux serving ssh and serial profiles from a single set of routes.
// No SSH server is started — these tests are about profile dispatch and the
// serial session path, neither of which dials a host.
type serialEnv struct {
	mux     *http.ServeMux
	handler *Handler
	sessMgr *session.Manager
	ssh     *sshconfig.Store
	serial  *serial.Store
}

func newSerialEnv(t *testing.T) *serialEnv {
	t.Helper()
	dir := t.TempDir()
	store := storage.New(dir)
	msgMgr := message.NewManager(store)
	sessMgr := session.NewManager(msgMgr, store, nil)
	ssh := sshconfig.NewStore(dir)
	sr := serial.NewStore(dir)
	t.Cleanup(func() {
		for _, s := range sessMgr.ListAll() {
			_ = sessMgr.Delete(s.ID)
		}
	})

	h := &Handler{Sessions: sessMgr, SSH: ssh, Serial: sr}
	mux := http.NewServeMux()
	h.Register(mux)
	return &serialEnv{mux: mux, handler: h, sessMgr: sessMgr, ssh: ssh, serial: sr}
}

func (e *serialEnv) do(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	e.mux.ServeHTTP(rr, r)
	return rr
}

func (e *serialEnv) decode(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &m); err != nil {
		t.Fatalf("invalid JSON %q: %v", rr.Body.String(), err)
	}
	return m
}

// serialDeviceFor names a device path the allowlist accepts on this host. The
// profile does not have to exist for anything but the open itself, so the
// placeholder works for every test that stops short of opening a port.
func serialDeviceFor() string {
	if runtime.GOOS == "windows" {
		return "COM3"
	}
	return "/dev/ttyUSB0"
}

func serialTOML(device string) string {
	return "kind = \"serial\"\ndevice = \"" + device + "\"\nbaud = 115200\n"
}

func remoteTOML() string {
	// Resolution and dispatch, not authentication: the credential is arbitrary.
	return "kind = \"remote\"\nhost = \"example.com\"\nuser = \"root\"\npassword = \"secret\"\n"
}

// The entry grid is one list serving both profile kinds: a serial profile must
// appear beside the ssh ones with its own kind label and the device line the
// card shows, not behind a second endpoint the UI would have to merge.
func TestConnectionListMergesBothKinds(t *testing.T) {
	e := newSerialEnv(t)
	if err := e.ssh.Save("hosta", []byte(remoteTOML())); err != nil {
		t.Fatal(err)
	}
	if err := e.serial.Save("esp32", []byte(serialTOML(serialDeviceFor()))); err != nil {
		t.Fatal(err)
	}

	rr := e.do(t, http.MethodGet, "/api/connections", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", rr.Code, rr.Body.String())
	}
	list := e.decode(t, rr)["connections"].([]any)

	byName := map[string]map[string]any{}
	for _, item := range list {
		m := item.(map[string]any)
		byName[m["name"].(string)] = m
	}
	// The built-in internal profile is always listed; it is not the subject
	// here, only the reason the list is checked by name rather than by length.
	for _, name := range []string{"internal", "hosta", "esp32"} {
		if _, ok := byName[name]; !ok {
			t.Fatalf("profile %q missing from the merged list: %v", name, list)
		}
	}
	if got := byName["esp32"]["kind"]; got != "serial" {
		t.Errorf("esp32 kind = %v, want serial", got)
	}
	if got := byName["hosta"]["kind"]; got != "remote" {
		t.Errorf("hosta kind = %v, want remote", got)
	}
	if got := byName["esp32"]["device"]; got == nil || got == "" {
		t.Errorf("esp32 device = %v, want the profile device", got)
	}
	if got := byName["esp32"]["baud"]; got != float64(115200) {
		t.Errorf("esp32 baud = %v, want 115200", got)
	}
	// A serial profile has no host: its transport is the device line, and
	// inventing a host field would render a card that dials nothing.
	if _, ok := byName["esp32"]["host"]; ok {
		t.Errorf("esp32 summary carries a host: %v", byName["esp32"])
	}
}

// GET/DELETE dispatch on where the profile lives; PUT dispatches on the kind
// the body declares, because a create has no existing file to consult.
func TestConnectionRoutesDispatchByKind(t *testing.T) {
	e := newSerialEnv(t)

	// PUT creates each kind in its own directory.
	rr := e.do(t, http.MethodPut, "/api/connections/esp32", serialTOML(serialDeviceFor()))
	if rr.Code != http.StatusNoContent {
		t.Fatalf("PUT serial = %d: %s", rr.Code, rr.Body.String())
	}
	rr = e.do(t, http.MethodPut, "/api/connections/hosta", remoteTOML())
	if rr.Code != http.StatusNoContent {
		t.Fatalf("PUT ssh = %d: %s", rr.Code, rr.Body.String())
	}
	if _, err := e.ssh.Load("esp32"); err == nil {
		t.Error("the serial profile landed in the ssh store")
	}
	if _, err := e.serial.Load("hosta"); err == nil {
		t.Error("the ssh profile landed in the serial store")
	}

	// GET returns each profile's own bytes.
	rr = e.do(t, http.MethodGet, "/api/connections/esp32", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET serial = %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "kind = \"serial\"") {
		t.Errorf("GET serial body = %q, want the serial profile", rr.Body.String())
	}
	rr = e.do(t, http.MethodGet, "/api/connections/hosta", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET ssh = %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "example.com") {
		t.Errorf("GET ssh body = %q, want the ssh profile", rr.Body.String())
	}

	// An unknown name is a 404 whichever store is asked first.
	rr = e.do(t, http.MethodGet, "/api/connections/nope", "")
	if rr.Code != http.StatusNotFound {
		t.Errorf("GET unknown = %d, want 404", rr.Code)
	}

	// DELETE removes only the profile it dispatched to.
	rr = e.do(t, http.MethodDelete, "/api/connections/esp32", "")
	if rr.Code != http.StatusNoContent {
		t.Fatalf("DELETE serial = %d: %s", rr.Code, rr.Body.String())
	}
	if _, err := e.serial.Load("esp32"); err == nil {
		t.Error("the serial profile survived its delete")
	}
	if _, err := e.ssh.Load("hosta"); err != nil {
		t.Errorf("the delete removed the ssh profile too: %v", err)
	}
}

// The test button has nothing to press for a serial profile: probing would open
// (and claim) the device, changing machine state from a button labelled "test".
func TestTestConnectionRefusesSerialBody(t *testing.T) {
	e := newSerialEnv(t)

	rr := e.do(t, http.MethodPost, "/api/connections/test", serialTOML(serialDeviceFor()))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("test serial = %d, want 400: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "nothing to dial") {
		t.Errorf("test serial message = %q, want the reason it is refused", rr.Body.String())
	}

	// An ssh body still takes the normal path (internal is a loopback and
	// answers ok without dialing).
	rr = e.do(t, http.MethodPost, "/api/connections/test",
		"kind = \"internal\"\n")
	if rr.Code != http.StatusOK {
		t.Errorf("test internal = %d: %s", rr.Code, rr.Body.String())
	}
}

// The device dropdown is the Web UI's copy of the MCP serial_ports tool: same
// enumeration, same shape, refused only by --no-serial.
func TestSerialPortsEndpoint(t *testing.T) {
	e := newSerialEnv(t)

	rr := e.do(t, http.MethodGet, "/api/serial/ports", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("serial ports = %d: %s", rr.Code, rr.Body.String())
	}
	if _, ok := e.decode(t, rr)["ports"]; !ok {
		t.Errorf("serial ports response has no ports array: %s", rr.Body.String())
	}

	// An instance started with --no-serial has no store and says so rather
	// than answering an empty list, which would read as "nothing plugged in".
	bare := &Handler{Sessions: e.sessMgr, SSH: e.ssh}
	mux := http.NewServeMux()
	bare.Register(mux)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/serial/ports", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("serial ports without a store = %d, want 503", rr.Code)
	}
}

// The termcp:// locator resolves through the same shared resolver as
// session_start, so a serial profile answers as an entry instead of 404.
func TestResolveEntryAcceptsSerialProfiles(t *testing.T) {
	e := newSerialEnv(t)
	if err := e.serial.Save("esp32", []byte(serialTOML(serialDeviceFor()))); err != nil {
		t.Fatal(err)
	}

	rr := e.do(t, http.MethodGet, "/api/resolve?url="+url.QueryEscape("termcp://esp32"), "")
	if rr.Code != http.StatusOK {
		t.Fatalf("resolve serial = %d: %s", rr.Code, rr.Body.String())
	}
	var resp resolveResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode %q: %v", rr.Body.String(), err)
	}
	if resp.Kind != "entry" || resp.SSHConfig != "esp32" {
		t.Fatalf("resolve serial = %+v, want kind=entry ssh_config=esp32", resp)
	}

	// A name in both directories is a configuration error, resolved by neither:
	// picking one would make the other profile silently unreachable.
	if err := e.ssh.Save("both", []byte(remoteTOML())); err != nil {
		t.Fatal(err)
	}
	if err := e.serial.Save("both", []byte(serialTOML(serialDeviceFor()))); err != nil {
		t.Fatal(err)
	}
	rr = e.do(t, http.MethodGet, "/api/resolve?url="+url.QueryEscape("termcp://both"), "")
	if rr.Code != http.StatusConflict {
		t.Errorf("resolve ambiguous = %d, want 409: %s", rr.Code, rr.Body.String())
	}
}

// A serial session has no process to run and no shell mode to select. The
// parameters are refused rather than ignored, and the refusal happens before
// anything touches the device — so it needs no hardware to prove.
func TestCreateSessionSerialRefusesCommandAndMode(t *testing.T) {
	e := newSerialEnv(t)
	if err := e.serial.Save("esp32", []byte(serialTOML(serialDeviceFor()))); err != nil {
		t.Fatal(err)
	}

	rr := e.do(t, http.MethodPost, "/api/sessions", `{"ssh_config":"esp32","command":"ls"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("command = %d, want 400: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "no process to run") {
		t.Errorf("command message = %q, want the reason", rr.Body.String())
	}

	rr = e.do(t, http.MethodPost, "/api/sessions", `{"ssh_config":"esp32","mode":"pty"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("mode = %d, want 400: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "no shell mode") {
		t.Errorf("mode message = %q, want the reason", rr.Body.String())
	}

	if n := len(e.sessMgr.ListAll()); n != 0 {
		t.Errorf("a refused start still created %d session(s)", n)
	}
}

// An unknown name is still a not-found, reported through the ssh path's
// message — there is no serial profile to open, so nothing about dispatch
// changes the answer.
func TestCreateSessionUnknownProfileStillFails(t *testing.T) {
	e := newSerialEnv(t)

	rr := e.do(t, http.MethodPost, "/api/sessions", `{"ssh_config":"ghost"}`)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("unknown profile = %d, want 400: %s", rr.Code, rr.Body.String())
	}
}

var ptyPathRe = regexp.MustCompile(`/dev/pts/\d+|/dev/ttys\d+`)

// startSerialPTY hands the test a real device: socat's linked pty pair with the
// device end symlinked under /dev/serial/by-id — an allowed location that
// resolves to the pty, which is exactly the shape a real by-id path has, so the
// production allowlist runs unchanged.
//
// Skipped where that shape cannot be built: Windows has no pty pairs, and the
// alias directory needs write access to /dev. The dispatch tests above cover
// everything except the open itself on those platforms.
func startSerialPTY(t *testing.T) (device string, peer *os.File) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("no pty pairs on Windows")
	}
	if _, err := exec.LookPath("socat"); err != nil {
		t.Skip("socat not installed")
	}

	cmd := exec.Command("socat", "-d", "-d", "pty,raw,echo=0", "pty,raw,echo=0")
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("socat stderr pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start socat: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	// socat prints one "PTY is" line per end on stderr.
	scanner := bufio.NewScanner(stderr)
	var paths []string
	deadline := time.After(5 * time.Second)
	found := make(chan struct{})
	go func() {
		defer close(found)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.Contains(line, "PTY is") {
				if m := ptyPathRe.FindString(line); m != "" {
					paths = append(paths, m)
					if len(paths) == 2 {
						return
					}
				}
			}
		}
	}()
	select {
	case <-found:
	case <-deadline:
		t.Fatal("timed out waiting for socat to report its pty pair")
	}
	if len(paths) < 2 {
		t.Fatalf("socat reported %d ptys, want 2", len(paths))
	}

	peer, err = os.OpenFile(paths[1], os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open peer pty %s: %v", paths[1], err)
	}
	t.Cleanup(func() { peer.Close() })

	if err := os.MkdirAll("/dev/serial/by-id", 0o755); err != nil {
		t.Skipf("cannot create /dev/serial/by-id (need root): %v", err)
	}
	alias := filepath.Join("/dev/serial/by-id", "termcp-webui-test-"+strconv.Itoa(os.Getpid()))
	if err := os.Symlink(paths[0], alias); err != nil {
		t.Skipf("cannot symlink %s -> %s: %v", alias, paths[0], err)
	}
	t.Cleanup(func() { os.Remove(alias) })
	return alias, peer
}

// The whole point of the dispatch work: POST /api/sessions accepts a serial
// profile and returns a session carrying kind "serial" and the device record,
// with rows/cols left at zero because a serial device has no terminal.
// The claim rules come with it — a second start on the same device is refused
// while the first session holds it, and only deleting that session frees it
// (design §5.2/§2.6: the claim releases at finalize, not at terminate).
func TestCreateSessionSerialOpensTheDevice(t *testing.T) {
	e := newSerialEnv(t)
	device, _ := startSerialPTY(t)
	if err := e.serial.Save("esp32", []byte(serialTOML(device))); err != nil {
		t.Fatal(err)
	}

	rr := e.do(t, http.MethodPost, "/api/sessions", `{"ssh_config":"esp32"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("start serial = %d: %s", rr.Code, rr.Body.String())
	}
	created := e.decode(t, rr)
	if created["kind"] != "serial" {
		t.Errorf("kind = %v, want serial", created["kind"])
	}
	if created["ssh_config"] != "esp32" {
		t.Errorf("ssh_config = %v, want esp32", created["ssh_config"])
	}
	serialRec, _ := created["serial"].(map[string]any)
	if serialRec == nil || serialRec["device"] != device || serialRec["baud"] != float64(115200) {
		t.Errorf("serial = %v, want device %s @115200", created["serial"], device)
	}
	if created["shell_id"] == nil || created["shell_id"] == "" {
		t.Errorf("shell_id = %v, want the primary shell", created["shell_id"])
	}
	sid, _ := created["session_id"].(string)
	if sid == "" {
		t.Fatalf("session_id = %v", created["session_id"])
	}

	// The session record reports the kind and the zero terminal size: there is
	// no window to report for a device, and the front-end xterm lays itself out.
	rr = e.do(t, http.MethodGet, "/api/sessions/"+sid, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("session info = %d: %s", rr.Code, rr.Body.String())
	}
	info := e.decode(t, rr)
	if info["kind"] != "serial" {
		t.Errorf("session kind = %v, want serial", info["kind"])
	}
	if info["rows"] != float64(0) || info["cols"] != float64(0) {
		t.Errorf("session rows/cols = %v/%v, want 0/0", info["rows"], info["cols"])
	}
	if info["encoding"] == nil || info["encoding"] == "" {
		t.Errorf("session encoding = %v, want the profile encoding (auto by default)", info["encoding"])
	}

	// Second start, same device: the claim is held by the live session.
	rr = e.do(t, http.MethodPost, "/api/sessions", `{"ssh_config":"esp32"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("second start = %d, want the claim refusal: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(strings.ToLower(rr.Body.String()), "busy") {
		t.Errorf("second start message = %q, want the busy device", rr.Body.String())
	}

	// Terminate keeps the claim (the transcript stays reviewable); purge
	// releases it and the next start succeeds.
	rr = e.do(t, http.MethodPost, "/api/sessions/"+sid+"/terminate", "")
	if rr.Code != http.StatusOK && rr.Code != http.StatusNoContent {
		t.Fatalf("terminate = %d: %s", rr.Code, rr.Body.String())
	}
	rr = e.do(t, http.MethodPost, "/api/sessions", `{"ssh_config":"esp32"}`)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("start after terminate = %d, want the claim still held: %s", rr.Code, rr.Body.String())
	}

	rr = e.do(t, http.MethodDelete, "/api/sessions/"+sid, "")
	if rr.Code != http.StatusOK && rr.Code != http.StatusNoContent {
		t.Fatalf("purge = %d: %s", rr.Code, rr.Body.String())
	}
	rr = e.do(t, http.MethodPost, "/api/sessions", `{"ssh_config":"esp32"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("start after purge = %d: %s", rr.Code, rr.Body.String())
	}
	second := e.decode(t, rr)
	if secondID, _ := second["session_id"].(string); secondID != "" {
		_ = e.do(t, http.MethodDelete, "/api/sessions/"+secondID, "")
	}
}
