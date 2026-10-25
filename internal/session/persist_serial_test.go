package session

import (
	"sync"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/sshserver"
	"github.com/open-mcp-ai/termcp/internal/storage"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// TestTwoPersistsCannotInterleave pins the lock around persistOne.
//
// persistOne takes its snapshot at the top and writes it at the bottom, with the shell
// manifests in between (an fsync each, measured ~7ms). Two concurrent persists are
// therefore a read-modify-write race whose loser is the NEWER state: a persist that
// started before a session died can finish after the one that recorded why it died, and
// the manifest on disk goes back to the older copy with the reason gone.
//
// That is not a test-only race. `get` after a restart is where a user asks why their
// session disappeared (issue #176), and the answer was being overwritten — observed as
// `TestWhyASessionDiedIsRecordedAndSurvivesARestart` failing on CI with the reason live
// in memory and empty on disk, about once in sixty runs, reproduced locally at -cpu=1.
//
// THE WINDOW IS HELD OPEN RATHER THAN RACED FOR. Forty iterations of the real
// interleaving still passed with the lock removed (measured), so the test drives the hook
// inside persistOne instead: the first persist stops between snapshot and write, and the
// second must not get in. Without the lock it walks straight through and the assertion
// fires.
func TestTwoPersistsCannotInterleave(t *testing.T) {
	store := storage.New(t.TempDir())
	t.Cleanup(func() { _ = store.Close() })
	mm := message.NewManager(store)
	srv := sshserver.New()
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Stop() })

	m := NewManager(mm, store, srv)
	sess, err := m.Create(Config{Command: testShell(), Mode: api.ModePipe, Name: "brief"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	id := sess.ID

	inside := make(chan struct{})
	release := make(chan struct{})
	var holdOnce sync.Once
	m.persistHook = func() {
		holdOnce.Do(func() {
			close(inside)
			<-release
		})
	}
	defer func() { m.persistHook = nil }()

	firstDone := make(chan struct{})
	go func() { m.persistOne(id); close(firstDone) }()
	<-inside // the first persist is inside the window, snapshot taken, write pending

	secondDone := make(chan struct{})
	go func() { m.persistOne(id); close(secondDone) }()
	select {
	case <-secondDone:
		close(release)
		t.Fatalf("a second persist ran inside the first one's window. The snapshot read and " +
			"the write are one operation: whichever finishes last wins, so a persist that " +
			"started before a fact changed can put the older state back on disk after the " +
			"persist that recorded the new one")
	case <-time.After(300 * time.Millisecond):
		// Still blocked, which is the invariant.
	}
	close(release)
	<-firstDone
	<-secondDone
}
