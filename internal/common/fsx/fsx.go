// Package fsx holds the filesystem operations that have to survive a long
// unattended run.
//
// Deleting a git repository is the awkward one. git marks pack and idx files
// read-only, and on Windows os.RemoveAll then fails with "Access is denied";
// separately, Defender or the search indexer can hold a transient handle on a file
// that was just written. The staging directory is deleted twice for every
// repository, so a delete that fails even occasionally would break a run over
// hundreds of them.
package fsx

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Retry schedule for a delete that loses a race with another process.
const (
	removeAttempts = 8
	removeBackoff  = 100 * time.Millisecond
	removeMaxWait  = 3 * time.Second
)

// RemoveAllRetry deletes path and everything under it.
//
// It escalates: a plain delete first, then clearing read-only attributes, then
// retrying while something else lets go of a handle, and finally renaming the
// directory aside so the run can continue. trashDir may be empty to skip that last
// step.
//
// A returned error means the path is still there. Callers treat that as a warning
// rather than a failure: leaving litter behind is much better than abandoning
// hours of completed work.
func RemoveAllRetry(path, trashDir string) error {
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		return nil
	}

	var lastErr error
	wait := removeBackoff

	for attempt := range removeAttempts {
		err := os.RemoveAll(path)
		if err == nil {
			return nil
		}
		lastErr = err

		// Read-only files are the common cause and are worth fixing before
		// spending any time waiting.
		if attempt == 0 {
			clearReadOnly(path)
			continue
		}
		if !isRetryableRemoveErr(lastErr) {
			break
		}

		time.Sleep(wait)
		if wait *= 2; wait > removeMaxWait {
			wait = removeMaxWait
		}
	}

	// Renaming often succeeds where deleting does not, because it needs no access
	// to the locked file itself.
	if trashDir != "" {
		if err := moveAside(path, trashDir); err == nil {
			return nil
		}
	}
	return fmt.Errorf("remove %s: %w", path, lastErr)
}

// moveAside renames path into trashDir under a unique name.
func moveAside(path, trashDir string) error {
	if err := os.MkdirAll(trashDir, 0o755); err != nil {
		return err
	}
	// The nanosecond stamp only has to be unique within one run, and is never
	// read back.
	name := filepath.Base(path) + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	return os.Rename(path, filepath.Join(trashDir, name))
}

// EmptyTrash removes whatever earlier deletes had to leave behind, and reports how
// many directories are still stuck so the run summary can mention them.
func EmptyTrash(trashDir string) (stuck int) {
	entries, err := os.ReadDir(trashDir)
	if err != nil {
		return 0
	}
	for _, entry := range entries {
		target := filepath.Join(trashDir, entry.Name())
		clearReadOnly(target)
		if err := os.RemoveAll(target); err != nil {
			stuck++
		}
	}
	_ = os.Remove(trashDir)
	return stuck
}

// DirSize sums the sizes of every regular file under root.
func DirSize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total, err
}

// Writable reports whether a directory can be created and written to, by actually
// doing it. Creating the directory when it is missing is deliberate: the run
// directory does not exist yet at the point this is checked.
func Writable(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	probe := filepath.Join(dir, ".clonezip-probe-"+strconv.Itoa(os.Getpid()))
	f, err := os.Create(probe)
	if err != nil {
		return err
	}
	writeErr := errors.Join(writeAndSync(f), f.Close())
	return errors.Join(writeErr, os.Remove(probe))
}

func writeAndSync(f *os.File) error {
	if _, err := f.WriteString("clonezip"); err != nil {
		return err
	}
	// Sync so a filesystem that only reports failure on flush is caught here
	// rather than part-way through writing an archive.
	return f.Sync()
}

// FreeSpace returns the bytes available on the volume holding dir.
func FreeSpace(dir string) (uint64, error) {
	return freeSpace(dir)
}
