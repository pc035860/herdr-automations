package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// The lock a crashed daemon leaves behind names a pid that is gone, and by the
// time anyone reads it the number may have been handed to something else. The
// scheduler used to take that as proof another daemon was running and refuse to
// start — silently, for as long as the impostor lived. Nothing about the pid on
// disk may stand in the way of starting.
func TestALockLeftBehindNamingSomeoneElseIsTakenOver(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HERDR_PLUGIN_STATE_DIR", dir)
	path := filepath.Join(dir, "daemon.pid")
	// A process of this test's own: alive beyond doubt, signallable by this
	// user, and not a daemon. A pid the test cannot signal would prove nothing —
	// a liveness check would read it as dead and start for the wrong reason.
	impostor := exec.Command("sleep", "30")
	if err := impostor.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		impostor.Process.Kill()
		impostor.Wait()
	}()
	if err := os.WriteFile(path, []byte(strconv.Itoa(impostor.Process.Pid)), 0o644); err != nil {
		t.Fatal(err)
	}

	release, err := acquireLock()
	if err != nil {
		t.Fatalf("an unlocked lock file wedges the scheduler on whatever pid it names: %v", err)
	}
	release()
}

// The other half of the same judgement: while a daemon is running, a second one
// must not start, or every automation fires twice. The wait is shortened here
// because the answer, not the patience, is what this test is about.
func TestASecondDaemonIsTurnedAwayWhileTheFirstHoldsTheLock(t *testing.T) {
	t.Setenv("HERDR_PLUGIN_STATE_DIR", t.TempDir())

	release, err := acquireLock()
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	if second, err := acquireLockWithin(50 * time.Millisecond); err == nil {
		second()
		t.Fatal("two schedulers held the lock at once, so every automation would fire twice")
	}
}

// A Herdr server handoff runs the plugin's startup hook while the daemon the
// old server started is still on its way out. Refusing at that instant is what
// left this machine with no scheduler for a day and a half: the hook's daemon
// gave up, the old daemon finished dying a moment later, and nothing took its
// place. The replacement has to wait the old one out.
func TestAStartingDaemonWaitsOutTheOneThatIsShuttingDown(t *testing.T) {
	t.Setenv("HERDR_PLUGIN_STATE_DIR", t.TempDir())

	dying, err := acquireLock()
	if err != nil {
		t.Fatal(err)
	}

	// The replacement starts while the old daemon still holds the lock, and
	// only gets it once that one is gone.
	started := make(chan func(), 1)
	go func() {
		release, err := acquireLockWithin(5 * time.Second)
		if err != nil {
			started <- nil
			return
		}
		started <- release
	}()

	time.Sleep(100 * time.Millisecond) // long enough for it to find the lock held
	dying()

	select {
	case release := <-started:
		if release == nil {
			t.Fatal("the replacement daemon gave up while the old one was still shutting down, so the schedule would stop until someone noticed")
		}
		release()
	case <-time.After(5 * time.Second):
		t.Fatal("the replacement daemon never took the lock the old one released")
	}
}

// A re-exec releases the lock and immediately takes it again, so releasing has
// to leave it genuinely free rather than merely look free.
func TestReleasingTheLockLetsTheNextDaemonStart(t *testing.T) {
	t.Setenv("HERDR_PLUGIN_STATE_DIR", t.TempDir())

	release, err := acquireLock()
	if err != nil {
		t.Fatal(err)
	}
	release()

	next, err := acquireLock()
	if err != nil {
		t.Fatalf("the lock outlived the daemon that released it: %v", err)
	}
	next()
}

// The pid is diagnostic, and the only thing a person reading the state
// directory by hand has to go on.
func TestTheLockNamesTheDaemonHoldingIt(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HERDR_PLUGIN_STATE_DIR", dir)
	// A longer pid already in the file would otherwise show through behind a
	// shorter one written over it.
	if err := os.WriteFile(filepath.Join(dir, "daemon.pid"), []byte("999999999"), 0o644); err != nil {
		t.Fatal(err)
	}

	release, err := acquireLock()
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	raw, err := os.ReadFile(filepath.Join(dir, "daemon.pid"))
	if err != nil {
		t.Fatal(err)
	}
	if want := os.Getpid(); string(raw) != strconv.Itoa(want) {
		t.Errorf("lock file says %q, want the running daemon's pid %d", raw, want)
	}
}
