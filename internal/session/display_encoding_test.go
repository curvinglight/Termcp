package session

import (
	"context"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/buffer"
	"github.com/open-mcp-ai/termcp/internal/encoding"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// Design §3.2: every outlet converts through the one encoder, so the same
// bytes read two different ways — the session's aggregate read and the
// channel's stream read, which is what MCP shell_output and the Web UI
// terminal actually consume — come back identical. Two outlets each converting
// on their own is how one copy ends up hexed, another replacement-chars and a
// third raw.
func TestOutletsAgreeOnTheSameBytes(t *testing.T) {
	tests := []struct {
		name    string
		enc     string
		payload []byte
	}{
		{"text session", encoding.Text, []byte("ok\r\n")},
		{"hex session, binary payload", encoding.Hex, []byte{0x00, 0xff, 0x1b, '['}},
		{"auto session, invalid utf8", encoding.Auto, []byte("ok\xff\n")},
		{"auto session, valid utf8", encoding.Auto, []byte("温度: 25\r\n")},
		// A session with no profile encoding: the normalised answer must still
		// be one every outlet shares, not an empty string each reads its own way.
		{"no encoding set", "", []byte{0xff, 0xfe}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := buffer.New(4096)
			sessionReader, _ := b.NewReader()
			shellReader, _ := b.NewReader()

			s := &Session{Session: api.Session{Encoding: tc.enc}, buf: b, readerID: sessionReader}
			// The root shell takes its session's encoding, as New() does, over
			// the same buffer the session reads: this is the real pairing, not
			// two objects that merely happen to be configured alike.
			cs := &ChildShell{buf: b, displayEnc: s.DisplayEncoding()}

			if err := b.Write(tc.payload); err != nil {
				t.Fatal(err)
			}

			fromSession, err := s.readOutput(context.Background(), sessionReader, 0, false, 0, false, 0)
			if err != nil {
				t.Fatal(err)
			}
			fromShell, err := cs.ReadTerminalStream(context.Background(), shellReader, 0, false, 0, 0)
			if err != nil {
				t.Fatal(err)
			}

			want := encoding.EncodeForDisplay(tc.payload, tc.enc)
			if fromSession != want {
				t.Errorf("session outlet = %q, want %q", fromSession, want)
			}
			if fromShell != want {
				t.Errorf("shell outlet = %q, want %q", fromShell, want)
			}
		})
	}
}

// The ANSI strip must not undo the encoding: a hex caller gets its bytes with
// the escape sequences still in them (as hex, so the strip has nothing to
// find), while a text caller is stripped exactly as before.
func TestStripANSIComposesWithEncoding(t *testing.T) {
	payload := []byte("\x1b[31mred\x1b[0m\n")

	b := buffer.New(4096)
	rid, _ := b.NewReader()
	s := &Session{Session: api.Session{Encoding: encoding.Text}, buf: b, readerID: rid}
	if err := b.Write(payload); err != nil {
		t.Fatal(err)
	}

	stripped, err := s.readOutput(context.Background(), rid, 0, true, 0, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if stripped != "red\n" {
		t.Errorf("text + strip_ansi = %q, want %q", stripped, "red\n")
	}

	// The same bytes through a hex session, on a buffer of its own so the two
	// reads do not compete for one reader cursor.
	hb := buffer.New(4096)
	hrid, _ := hb.NewReader()
	hs := &Session{Session: api.Session{Encoding: encoding.Hex}, buf: hb, readerID: hrid}
	if err := hb.Write(payload); err != nil {
		t.Fatal(err)
	}
	hexed, err := hs.readOutput(context.Background(), hrid, 0, true, 0, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if want := encoding.EncodeForDisplay(payload, encoding.Hex); hexed != want {
		t.Errorf("hex + strip_ansi = %q, want the whole payload as hex %q", hexed, want)
	}
	if hexed == encoding.EncodeForDisplay([]byte("red\n"), encoding.Hex) {
		t.Error("hex + strip_ansi dropped the escape sequences the caller asked to see")
	}
}

// The empty encoding never reaches a reader: it is normalised once, at the
// session, so four outlets cannot each invent a different answer for "".
func TestDisplayEncodingNormalisesEmpty(t *testing.T) {
	var nilSession *Session
	if got := nilSession.DisplayEncoding(); got != encoding.Text {
		t.Errorf("nil session = %q, want %q", got, encoding.Text)
	}
	if got := new(Session).DisplayEncoding(); got != encoding.Text {
		t.Errorf("session with no encoding = %q, want %q", got, encoding.Text)
	}
	s := &Session{Session: api.Session{Encoding: encoding.Auto}}
	if got := s.DisplayEncoding(); got != encoding.Auto {
		t.Errorf("serial session = %q, want %q", got, encoding.Auto)
	}
}

// Design §6.4: api.Session is shared by the container and its shells, so
// container-level properties are set on the container and left off the shell.
// A shell carrying kind:"serial" would tell the front end a channel can be
// resized; one carrying the device record would advertise a connection a
// shell does not have. Kind predates this feature, Encoding and Serial were
// added to the same struct and must follow it.
func TestContainerOnlyFieldsStayOffChildShellInfo(t *testing.T) {
	s := &Session{Session: api.Session{
		ID:       "sess",
		Kind:     api.KindSerial,
		Encoding: encoding.Hex,
		Serial:   &api.SerialInfo{Profile: "esp32", Device: "/dev/ttyUSB0", Baud: 115200},
	}}
	info := s.Info()
	if info.Kind != api.KindSerial {
		t.Errorf("session Info().Kind = %q, want %q", info.Kind, api.KindSerial)
	}
	if info.Encoding != encoding.Hex {
		t.Errorf("session Info().Encoding = %q, want %q", info.Encoding, encoding.Hex)
	}
	if info.Serial == nil || info.Serial.Baud != 115200 || info.Serial.Device != "/dev/ttyUSB0" {
		t.Errorf("session Info().Serial = %+v, want the device record", info.Serial)
	}

	cs := &ChildShell{ID: "shell", Name: "shell", displayEnc: s.DisplayEncoding()}
	got := cs.Info()
	if got.Kind != "" {
		t.Errorf("ChildShell.Info().Kind = %q, want empty", got.Kind)
	}
	if got.Encoding != "" {
		t.Errorf("ChildShell.Info().Encoding = %q, want empty", got.Encoding)
	}
	if got.Serial != nil {
		t.Errorf("ChildShell.Info().Serial = %+v, want nil", got.Serial)
	}
	// The shell still reads the session's encoding — it just does not claim
	// the session's record.
	if cs.displayEnc != encoding.Hex {
		t.Errorf("shell displayEnc = %q, want the session's %q", cs.displayEnc, encoding.Hex)
	}
}

// The outlets that hold an id rather than a session (range endpoints, rail,
// MCP tail/offset reads) resolve through the manager, and must reach the same
// answer for a session id, a live shell id and a shell known only from its
// snapshot after the session went DEAD — otherwise a replayed transcript
// renders differently from the live one it came from.
func TestManagerDisplayEncodingFor(t *testing.T) {
	m := NewManager(nil, nil, nil)
	s := &Session{Session: api.Session{ID: "s1", Encoding: encoding.B64}}
	s.shells.Store("live", &ChildShell{ID: "live", displayEnc: s.DisplayEncoding()})
	s.shellHistory.Store("gone", api.Session{ID: "gone"})
	m.sessions.Store("s1", s)

	cases := map[string]string{
		"s1":    encoding.B64, // the session itself
		"live":  encoding.B64, // a live shell
		"gone":  encoding.B64, // a shell whose session is DEAD or restored
		"other": encoding.Text,
	}
	for id, want := range cases {
		if got := m.DisplayEncodingFor(id); got != want {
			t.Errorf("DisplayEncodingFor(%q) = %q, want %q", id, got, want)
		}
	}
}
