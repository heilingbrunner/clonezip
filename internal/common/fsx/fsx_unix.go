//go:build !windows

package fsx

import (
	"errors"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// clearReadOnly has nothing to do on this platform.
//
// Unlinking a file needs write permission on its parent directory, not on the file
// itself, so the 0444 pack files git writes delete perfectly well. The Windows
// implementation exists because there os.RemoveAll genuinely cannot unlink a
// read-only file.
func clearReadOnly(string) {}

// isRetryableRemoveErr reports whether a delete failed for a reason that waiting
// might fix. Rare locally, but a staging directory on NFS or an overlay mount can
// report EBUSY while another process still holds the file open.
func isRetryableRemoveErr(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	switch errno {
	case syscall.EBUSY, syscall.ENOTEMPTY, syscall.EACCES, syscall.EPERM:
		return true
	}
	return false
}

// freeSpace reports the bytes available to an unprivileged user on the filesystem
// holding dir. Bavail rather than Bfree, because the blocks reserved for root are
// not available to us.
func freeSpace(dir string) (uint64, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return 0, err
	}
	var stat unix.Statfs_t
	if err := unix.Statfs(abs, &stat); err != nil {
		return 0, err
	}
	return stat.Bavail * uint64(stat.Bsize), nil
}
