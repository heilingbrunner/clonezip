// Package lockx takes an exclusive, OS-level advisory lock on a file so that
// only one process at a time can hold it.
//
// Unlike a PID file, the lock needs no stale-entry cleanup: the OS releases it
// automatically when the holding process exits, for any reason including a
// crash or a forced kill.
package lockx

import (
	"errors"
	"fmt"
	"os"
	"time"
)

// ErrLocked is returned by Acquire when another process already holds the
// lock on path.
var ErrLocked = errors.New("lockx: already locked by another process")

// FileLock is a held lock on a file. The lock is released by calling Release.
type FileLock struct {
	f *os.File
}

// Acquire takes an exclusive, non-blocking lock on path, creating the file if
// it does not already exist. It returns ErrLocked if another process already
// holds the lock.
func Acquire(path string) (*FileLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}

	if err := tryLock(f); err != nil {
		_ = f.Close()
		if errors.Is(err, ErrLocked) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}

	// Best-effort diagnostics for a human inspecting the file - not read back
	// by lockx itself, so a write failure here does not affect the lock.
	_ = f.Truncate(0)
	_, _ = f.Seek(0, 0)
	_, _ = fmt.Fprintf(f, "pid=%d\nstarted=%s\n", os.Getpid(), time.Now().Format(time.RFC3339))

	return &FileLock{f: f}, nil
}

// Release releases the lock and closes the file. Safe to call once; calling
// it more than once returns an error from the second call onward.
func (l *FileLock) Release() error {
	return l.f.Close()
}
