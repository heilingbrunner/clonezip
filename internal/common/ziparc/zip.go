// Package ziparc reads and writes the archives clonezip produces.
//
// These are plain ZIP containers, which is not a change: the PowerShell script
// called 7-Zip with -tzip, so every archive it ever wrote was already a ZIP
// wearing a ".7z" extension. Using the standard library therefore reads every
// existing archive and removes the dependency on 7-Zip entirely.
package ziparc

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/heilingbrunner/clonezip/internal/common/safepath"
)

// copyBufSize is reused for every entry, so memory stays flat regardless of how
// large a pack file is.
const copyBufSize = 1 << 20

// Magic numbers used to tell a ZIP from a genuine 7z archive.
var (
	magicZIP      = []byte{'P', 'K', 0x03, 0x04}
	magicZIPEmpty = []byte{'P', 'K', 0x05, 0x06} // an archive with no entries
	magic7z       = []byte{'7', 'z', 0xBC, 0xAF, 0x27, 0x1C}
)

// storedExtensions are already compressed, so deflating them costs CPU for roughly
// nothing. Git pack files are zlib streams and LFS objects are usually media or
// binaries.
var storedExtensions = map[string]bool{
	".pack":   true,
	".idx":    true,
	".rev":    true,
	".bitmap": true,
}

// FormatError reports a file that is not a ZIP container.
type FormatError struct {
	Path     string
	Detected string
}

func (e *FormatError) Error() string {
	return fmt.Sprintf("%s is not a zip archive (looks like %s); clonezip reads zip only",
		e.Path, e.Detected)
}

// Progress reports bytes processed so far, against a known total.
type Progress func(done, total int64)

func (p Progress) report(done, total int64) {
	if p != nil {
		p(done, total)
	}
}

// CreateFromDir writes srcDir into a new archive at dest.
//
// Entries are placed under rootName, matching the layout 7-Zip produced, so an
// archive stays interchangeable with the ones already on disk. rootName is passed
// separately from srcDir because the staging directory is named by list index to
// stay collision-free, while the archive has to carry the repository name.
//
// extra holds additional root-level files, used for the manifest. Those are
// siblings of rootName rather than members of it, so nothing lands inside the
// restored bare repository and git fsck stays clean.
//
// The archive is written to dest+".part" and renamed on success only. That is what
// makes "the archive exists" mean "the archive is complete" - the PowerShell
// version never checked 7-Zip's exit code, so a truncated archive was
// indistinguishable from a good one.
func CreateFromDir(
	ctx context.Context,
	srcDir, rootName, dest string,
	extra map[string][]byte,
	progress Progress,
) (size int64, err error) {
	total, err := treeSize(srcDir)
	if err != nil {
		return 0, fmt.Errorf("measure %s: %w", srcDir, err)
	}

	part := dest + ".part"
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return 0, err
	}
	// A .part left by an interrupted earlier run must not be appended to.
	if err := os.Remove(part); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return 0, err
	}

	f, err := os.Create(part)
	if err != nil {
		return 0, err
	}
	// Any failure past this point leaves no half-written file behind.
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(part)
		}
	}()

	zw := zip.NewWriter(f)
	buf := make([]byte, copyBufSize)
	var done int64

	for name, content := range extra {
		if err = writeBytes(zw, name, content); err != nil {
			return 0, err
		}
	}

	err = filepath.WalkDir(srcDir, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}

		rel, err := filepath.Rel(srcDir, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		entry, err := safepath.EntryName(filepath.Join(rootName, rel))
		if err != nil {
			return err
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		switch {
		case d.IsDir():
			// 7-Zip stores directory entries and archive/zip does not unless
			// told, so an empty branches/ or refs/tags/ would otherwise vanish.
			return writeDir(zw, entry, info)

		case info.Mode()&fs.ModeSymlink != 0:
			// A bare clone contains no symlinks; one appearing means something is
			// wrong, and following it from an archive is a needless risk.
			return nil

		case !info.Mode().IsRegular():
			return nil
		}

		n, err := writeFile(zw, entry, p, info, buf)
		if err != nil {
			return err
		}
		done += n
		progress.report(done, total)
		return nil
	})
	if err != nil {
		return 0, err
	}

	if err = zw.Close(); err != nil {
		return 0, err
	}
	if err = f.Sync(); err != nil {
		return 0, err
	}
	if err = f.Close(); err != nil {
		return 0, err
	}

	info, err := os.Stat(part)
	if err != nil {
		return 0, err
	}
	// Replacing an existing archive is intentional: a re-run overwrites.
	if err = os.Remove(dest); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return 0, err
	}
	if err = os.Rename(part, dest); err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func writeDir(zw *zip.Writer, entry string, info fs.FileInfo) error {
	header, err := zip.FileInfoHeader(info)
	if err != nil {
		return err
	}
	header.Name = entry + "/"
	header.Method = zip.Store
	_, err = zw.CreateHeader(header)
	return err
}

func writeFile(zw *zip.Writer, entry, srcPath string, info fs.FileInfo, buf []byte) (int64, error) {
	header, err := zip.FileInfoHeader(info)
	if err != nil {
		return 0, err
	}
	header.Name = entry
	header.Method = compressionFor(entry)
	// SetMode keeps the executable bit, which matters for the hooks/*.sample files
	// when an archive written on Linux is restored on Linux.
	header.SetMode(info.Mode())

	w, err := zw.CreateHeader(header)
	if err != nil {
		return 0, err
	}
	src, err := os.Open(srcPath)
	if err != nil {
		return 0, err
	}
	// Read-only, so a failed close cannot lose data; the copy error is what matters.
	defer func() { _ = src.Close() }()

	return io.CopyBuffer(w, src, buf)
}

func writeBytes(zw *zip.Writer, name string, content []byte) error {
	header := &zip.FileHeader{Name: name, Method: zip.Deflate}
	header.SetMode(0o644)
	w, err := zw.CreateHeader(header)
	if err != nil {
		return err
	}
	_, err = w.Write(content)
	return err
}

// compressionFor chooses deflate or store for one entry.
func compressionFor(entry string) uint16 {
	if storedExtensions[strings.ToLower(path.Ext(entry))] {
		return zip.Store
	}
	// LFS objects are content-addressed blobs with no extension, holding whatever
	// was committed - usually already-compressed media.
	if strings.Contains(entry, "/lfs/objects/") {
		return zip.Store
	}
	return zip.Deflate
}

func treeSize(root string) (int64, error) {
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

// Archive is an open archive.
type Archive struct {
	Path string

	r *zip.ReadCloser
}

// Open opens an archive, rejecting anything that is not a ZIP container.
func Open(archivePath string) (*Archive, error) {
	if err := checkMagic(archivePath); err != nil {
		return nil, err
	}
	r, err := zip.OpenReader(archivePath)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", archivePath, err)
	}
	return &Archive{Path: archivePath, r: r}, nil
}

// Close releases the archive.
func (a *Archive) Close() error { return a.r.Close() }

// checkMagic distinguishes a ZIP from a real 7z file, so an archive that genuinely
// needs 7-Zip produces a clear message rather than a parse error.
func checkMagic(archivePath string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	head := make([]byte, 6)
	n, err := io.ReadFull(f, head)
	if err != nil && n == 0 {
		return fmt.Errorf("read %s: %w", archivePath, err)
	}
	head = head[:n]

	switch {
	case bytes.HasPrefix(head, magicZIP), bytes.HasPrefix(head, magicZIPEmpty):
		return nil
	case bytes.HasPrefix(head, magic7z):
		return &FormatError{Path: archivePath, Detected: "7z"}
	default:
		return &FormatError{Path: archivePath, Detected: "an unknown format"}
	}
}

// UncompressedSize is what extraction will occupy, for a free-space check that can
// report a real number instead of dying half way through.
func (a *Archive) UncompressedSize() int64 {
	var total int64
	for _, f := range a.r.File {
		total += int64(f.UncompressedSize64)
	}
	return total
}

// Entries returns the number of members.
func (a *Archive) Entries() int { return len(a.r.File) }

// ReadFile returns the contents of one entry, used for the manifest.
func (a *Archive) ReadFile(name string) ([]byte, error) {
	for _, f := range a.r.File {
		if f.Name != name {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer func() { _ = rc.Close() }()
		return io.ReadAll(rc)
	}
	return nil, fs.ErrNotExist
}

// TopLevelDir returns the single directory every other entry sits under.
//
// Deriving the name from the archive rather than from its filename is what lets a
// legacy archive restore correctly: those hold "<repo>/" while current ones hold
// "<repo>.git/", and the filename cannot be trusted to say which.
func (a *Archive) TopLevelDir() (string, error) {
	var name string
	for _, f := range a.r.File {
		trimmed := strings.TrimPrefix(f.Name, "./")
		first, _, nested := strings.Cut(trimmed, "/")
		if first == "" {
			continue
		}
		// Root-level files such as the manifest are siblings, not candidates.
		if !nested && !f.FileInfo().IsDir() {
			continue
		}
		switch {
		case name == "":
			name = first
		case name != first:
			return "", fmt.Errorf("%s has more than one top-level directory (%q and %q)",
				a.Path, name, first)
		}
	}
	if name == "" {
		return "", fmt.Errorf("%s contains no directory", a.Path)
	}
	return name, nil
}

// ExtractToDir extracts the archive into destDir.
//
// rootRename, when set, replaces the archive's top-level directory name, which is
// how a legacy "<repo>/" becomes "<repo>.git" without a separate rename step.
func (a *Archive) ExtractToDir(ctx context.Context, destDir, rootRename string, progress Progress) error {
	destAbs, err := filepath.Abs(destDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(destAbs, 0o755); err != nil {
		return err
	}

	var root string
	if rootRename != "" {
		if root, err = a.TopLevelDir(); err != nil {
			return err
		}
	}

	total := a.UncompressedSize()
	buf := make([]byte, copyBufSize)
	var done int64

	for _, f := range a.r.File {
		if err := ctx.Err(); err != nil {
			return err
		}

		name := f.Name
		if root != "" {
			if name == root+"/" {
				name = rootRename + "/"
			} else if after, ok := strings.CutPrefix(name, root+"/"); ok {
				name = rootRename + "/" + after
			}
		}

		// Every entry name is resolved through the zip-slip guard, which also
		// rejects absolute paths, backslashes and reserved device names.
		target, err := safepath.SafeJoin(destAbs, name)
		if err != nil {
			return err
		}

		info := f.FileInfo()
		switch {
		case strings.HasSuffix(f.Name, "/") || info.IsDir():
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		case info.Mode()&fs.ModeSymlink != 0:
			// Refused rather than recreated: a bare repo has none, so an entry
			// like this means corruption or tampering.
			return fmt.Errorf("%s contains a symlink entry (%s), which clonezip will not extract",
				a.Path, f.Name)
		}

		n, err := extractFile(f, target, info, buf)
		if err != nil {
			return err
		}
		done += n
		progress.report(done, total)
	}
	return nil
}

func extractFile(f *zip.File, target string, info fs.FileInfo, buf []byte) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return 0, err
	}

	// Preserve the executable bit when the archive recorded one. An archive written
	// on Windows records none, in which case hooks/*.sample come back
	// non-executable - harmless, because git never runs them.
	mode := fs.FileMode(0o644)
	if info.Mode().Perm()&0o111 != 0 {
		mode = 0o755
	}

	rc, err := f.Open()
	if err != nil {
		return 0, err
	}
	defer func() { _ = rc.Close() }()

	out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return 0, err
	}
	n, copyErr := io.CopyBuffer(out, rc, buf)
	closeErr := out.Close()
	if copyErr != nil {
		return n, copyErr
	}
	return n, closeErr
}

// Verify reads every entry to the end, which forces the CRC32 each one carries to
// be checked. This is how a truncated or corrupted archive is caught before it is
// relied on, rather than years later.
func Verify(ctx context.Context, archivePath string) error {
	a, err := Open(archivePath)
	if err != nil {
		return err
	}
	defer func() { _ = a.Close() }()

	buf := make([]byte, copyBufSize)
	for _, f := range a.r.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		if strings.HasSuffix(f.Name, "/") {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return fmt.Errorf("%s: open entry %s: %w", archivePath, f.Name, err)
		}
		_, copyErr := io.CopyBuffer(io.Discard, rc, buf)
		closeErr := rc.Close()
		if copyErr != nil {
			return fmt.Errorf("%s: entry %s: %w", archivePath, f.Name, copyErr)
		}
		if closeErr != nil {
			return fmt.Errorf("%s: entry %s: %w", archivePath, f.Name, closeErr)
		}
	}
	return nil
}
