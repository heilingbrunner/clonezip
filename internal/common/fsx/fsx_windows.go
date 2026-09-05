package fsx

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// clearReadOnly clears the read-only attribute on everything under root.
//
// This is the single most common reason deleting a clone fails on Windows: git
// marks pack and idx files read-only, and os.RemoveAll cannot unlink a read-only
// file. Errors are ignored throughout, because this is a best-effort pass before a
// retry, and the retry reports the real failure.
func clearReadOnly(root string) {
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Keep walking: one unreadable subtree should not stop the rest from
			// being made deletable.
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.Mode().Perm()&0o200 == 0 {
			_ = os.Chmod(path, info.Mode().Perm()|0o200)
		}
		return nil
	})
}

// isRetryableRemoveErr reports whether a delete failed for a reason that waiting
// might fix - another process holding a handle, which on this platform means
// Defender, the search indexer, or a git helper that has not exited yet.
func isRetryableRemoveErr(err error) bool {
	var errno windows.Errno
	if !errors.As(err, &errno) {
		return false
	}
	switch errno {
	case windows.ERROR_ACCESS_DENIED,
		windows.ERROR_SHARING_VIOLATION,
		windows.ERROR_LOCK_VIOLATION,
		windows.ERROR_DIR_NOT_EMPTY:
		return true
	}
	return false
}

// freeSpace reports the bytes available to this user on the volume holding dir.
func freeSpace(dir string) (uint64, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return 0, err
	}
	path, err := windows.UTF16PtrFromString(abs)
	if err != nil {
		return 0, err
	}
	// The first value is the caller's quota-adjusted free space, which is the one
	// that decides whether a write will actually succeed.
	var freeToCaller, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(path, &freeToCaller, &total, &totalFree); err != nil {
		return 0, err
	}
	return freeToCaller, nil
}
