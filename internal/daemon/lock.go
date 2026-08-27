package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

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
	if err := os.MkdirAll(config.StateDir(), 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(config.StateDir(), "daemon.pid")

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		held := recordedPID(f)
		f.Close()
		// Only a refusal to wait means someone else has it. Anything else — a
		// filesystem without locks, a bad descriptor — is its own failure, and
		// saying "another daemon is already running" would send whoever reads
		// the log hunting for a process that does not exist.
		if err == syscall.EWOULDBLOCK {
			return nil, fmt.Errorf("another daemon is already running (pid %s)", held)
		}
		return nil, fmt.Errorf("locking %s: %w", path, err)
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
