package daemon

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/DnzzL/herdr-automations/internal/config"
)

// acquireLock keeps a single scheduler alive per machine. Two daemons would
// fire every automation twice, and a stale one is easy to end up with: the
// startup hook runs again on every Herdr server restart.
//
// What excludes the second daemon is the kernel's flock, not the pid inside the
// file. A pid only records who held the lock at some point: the holder can die
// without clearing it, and by the time anyone reads the number back it may
// belong to an unrelated process — which reads as "held forever", and every
// occurrence is then missed with nothing on screen to say why. The kernel drops
// a flock however the process ends, SIGKILL and power loss included, and taking
// it is one operation rather than a read-then-write that two daemons starting
// together can interleave with.
//
// The file is never removed, only unlocked: deleting it would let one daemon
// unlink the inode another is already holding, leaving both to believe they are
// the only scheduler. An idle lock file costs nothing.
func acquireLock() (release func(), err error) {
	return acquireLockWithin(lockWait)
}

const (
	// lockWait is how long a starting daemon waits for the holder to let go
	// before it gives up. A Herdr server handoff runs the startup hook while the
	// daemon the old server started is still shutting down: the hook's daemon
	// used to refuse at that instant, the old one finished dying a moment later,
	// and the machine was left with no scheduler at all until someone noticed
	// the reports had stopped. Long enough to cover that teardown, short enough
	// that a hook facing a healthy daemon does not sit here for the machine's
	// uptime.
	lockWait = 10 * time.Second
	// lockPoll is how often the lock is retried while waiting. The holder
	// releases by closing a descriptor, so there is nothing to be woken by.
	lockPoll = 100 * time.Millisecond
)

// acquireLockWithin is acquireLock with the wait spelled out, so a test can
// exercise the waiting without spending the production timeout on it.
func acquireLockWithin(wait time.Duration) (release func(), err error) {
	if err := os.MkdirAll(config.StateDir(), 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(config.StateDir(), "daemon.pid")

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}

	deadline := time.Now().Add(wait)
	announced := false
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		// Only a refusal to wait means someone else has it. Anything else — a
		// filesystem without locks, a bad descriptor — is its own failure, and
		// saying "another daemon is already running" would send whoever reads
		// the log hunting for a process that does not exist.
		if err != syscall.EWOULDBLOCK {
			f.Close()
			return nil, fmt.Errorf("locking %s: %w", path, err)
		}
		if !time.Now().Before(deadline) {
			held := recordedPID(f)
			f.Close()
			return nil, fmt.Errorf("another daemon is already running (pid %s)", held)
		}
		// Said once, not once per poll: the wait is the interesting part, and
		// the pid is what a person reading the log will go look for.
		if !announced {
			log.Printf("another daemon holds the lock (pid %s); waiting up to %s for it to exit",
				recordedPID(f), wait)
			announced = true
		}
		time.Sleep(lockPoll)
	}

	// The pid is written for whoever goes looking in the state directory by
	// hand; nothing decides anything on it, so a truncate that fails costs a
	// confusing file rather than a second scheduler.
	if err := f.Truncate(0); err == nil {
		f.WriteAt([]byte(strconv.Itoa(os.Getpid())), 0)
	}

	// Closing the file is what hands the lock on — including to the process
	// that replaces this one across a re-exec.
	return func() { f.Close() }, nil
}

// recordedPID reports what the lock's holder wrote about itself, for the error
// that names it. It is a label, not evidence: the holder is whoever the kernel
// says holds the flock, whatever the file says.
func recordedPID(f *os.File) string {
	raw := make([]byte, 32)
	n, _ := f.ReadAt(raw, 0)
	if pid := strings.TrimSpace(string(raw[:n])); pid != "" {
		return pid
	}
	return "unknown"
}
