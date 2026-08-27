package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
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
// must not start, or every automation fires twice.
func TestASecondDaemonIsTurnedAwayWhileTheFirstHoldsTheLock(t *testing.T) {
	t.Setenv("HERDR_PLUGIN_STATE_DIR", t.TempDir())

	release, err := acquireLock()
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	if second, err := acquireLock(); err == nil {
		second()
		t.Fatal("two schedulers held the lock at once, so every automation would fire twice")
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
