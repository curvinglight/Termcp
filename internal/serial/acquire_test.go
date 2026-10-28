package serial

import (
	"errors"
	"sync"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/serial/port"
)

func TestAcquireRejectsSecondClaim(t *testing.T) {
	ReleaseAllForTest()
	defer ReleaseAllForTest()

	release, err := Acquire("/dev/ttyUSB0", "first")
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	defer release()

	_, err = Acquire("/dev/ttyUSB0", "second")
	if !errors.Is(err, port.ErrBusy) {
		t.Fatalf("second Acquire = %v, want ErrBusy", err)
	}
}

func TestAcquireReleaseAllowsReclaim(t *testing.T) {
	ReleaseAllForTest()
	defer ReleaseAllForTest()

	release, err := Acquire("/dev/ttyUSB0", "first")
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	release()

	if _, err := Acquire("/dev/ttyUSB0", "second"); err != nil {
		t.Fatalf("after release the device should be free, got %v", err)
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	ReleaseAllForTest()
	defer ReleaseAllForTest()

	release, err := Acquire("/dev/ttyUSB0", "first")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	// The release function is handed to a resource scope that may run it on
	// several teardown paths.
	release()
	release()
	release()

	if _, err := Acquire("/dev/ttyUSB0", "second"); err != nil {
		t.Fatalf("the device should be free after release, got %v", err)
	}
}

func TestAcquireIsConcurrencySafe(t *testing.T) {
	// The race this exists to prevent: two callers both check "is it free"
	// before either claims it. Only one may win.
	ReleaseAllForTest()
	defer ReleaseAllForTest()

	const attempts = 16
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0

	start := make(chan struct{})
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := Acquire("/dev/ttyUSB0", "racer"); err == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	if wins != 1 {
		t.Fatalf("%d callers acquired the same device, want exactly 1", wins)
	}
}

func TestAcquireRejectsEmptyDevice(t *testing.T) {
	ReleaseAllForTest()
	defer ReleaseAllForTest()

	if _, err := Acquire("  ", "profile"); err == nil {
		t.Fatal("an empty device should be refused")
	}
}

func TestAcquireDistinctDevicesAreIndependent(t *testing.T) {
	ReleaseAllForTest()
	defer ReleaseAllForTest()

	r1, err := Acquire("/dev/ttyUSB0", "a")
	if err != nil {
		t.Fatalf("Acquire ttyUSB0: %v", err)
	}
	defer r1()

	if _, err := Acquire("/dev/ttyUSB1", "b"); err != nil {
		t.Fatalf("a different device should be acquirable, got %v", err)
	}
}

func TestReleaseDoesNotFreeAReclaimedDevice(t *testing.T) {
	// A stale release arriving after the device was closed and reopened must not
	// drop the new holder's claim.
	ReleaseAllForTest()
	defer ReleaseAllForTest()

	releaseOld, err := Acquire("/dev/ttyUSB0", "old")
	if err != nil {
		t.Fatalf("Acquire old: %v", err)
	}

	// The old holder releases and a new one takes it.
	releaseOld()
	if _, err := Acquire("/dev/ttyUSB0", "new"); err != nil {
		t.Fatalf("Acquire new: %v", err)
	}

	// A second release from the old holder must be a no-op, not a free.
	releaseOld()

	if _, err := Acquire("/dev/ttyUSB0", "third"); !errors.Is(err, port.ErrBusy) {
		t.Fatalf("the new holder's claim should survive a stale release, got %v", err)
	}
}
