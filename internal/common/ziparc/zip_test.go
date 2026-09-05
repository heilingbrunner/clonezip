package ziparc

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const manifestEntry = "clonezip-manifest.json"

// writeBareRepo builds a directory shaped like the bare clone clonezip archives: a
// large already-compressed pack file, an LFS payload, empty directories, an
// executable hook sample, and read-only pack files as git writes them.
func writeBareRepo(t *testing.T, root string) {
	t.Helper()

	// Pseudo-random so the pack is genuinely incompressible, with a fixed seed so
	// the test stays deterministic.
	pack := make([]byte, 3<<20)
	rng := rand.New(rand.NewPCG(1, 2))
	for i := range pack {
		pack[i] = byte(rng.UintN(256))
	}

	files := []struct {
		rel     string
		content []byte
		mode    os.FileMode
	}{
		{"HEAD", []byte("ref: refs/heads/main\n"), 0o644},
		{"config", []byte("[core]\n\trepositoryformatversion = 0\n\tbare = true\n"), 0o644},
		{"packed-refs", []byte("# pack-refs with: peeled fully-peeled sorted\n"), 0o644},
		{"objects/pack/pack-0123456789ab.pack", pack, 0o444},
		{"objects/pack/pack-0123456789ab.idx", []byte("fake index contents"), 0o444},
		{"refs/heads/main", []byte("9f3c1ab000000000000000000000000000000000\n"), 0o644},
		{"lfs/objects/ab/cd/abcdef0123456789", []byte("large file payload"), 0o644},
		{"hooks/pre-commit.sample", []byte("#!/bin/sh\nexit 0\n"), 0o755},
	}
	for _, f := range files {
		path := filepath.Join(root, filepath.FromSlash(f.rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, f.content, f.mode); err != nil {
			t.Fatal(err)
		}
	}
	// Empty directories a bare repo really has. 7-Zip stores these, and
	// archive/zip drops them unless directory entries are written explicitly.
	for _, dir := range []string{"branches", "objects/info", "refs/tags"} {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(dir)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// treeDigest maps every path under root to a digest of its contents, with
// directories recorded as a marker, so two trees can be compared exactly.
func treeDigest(t *testing.T, root string) map[string]string {
	t.Helper()

	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		key := filepath.ToSlash(rel)
		if d.IsDir() {
			out[key] = "dir"
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			return err
		}
		out[key] = hex.EncodeToString(h.Sum(nil))
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

func TestRoundTripPreservesEveryFileAndEmptyDirectory(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "0001-Proj-MyRepo")
	writeBareRepo(t, src)

	archive := filepath.Join(base, "out", "MyRepo.git.zip")
	size, err := CreateFromDir(context.Background(), src, "MyRepo.git", archive,
		map[string][]byte{manifestEntry: []byte(`{"schemaVersion":1,"tool":"clonezip/0.1.0"}`)}, nil)
	if err != nil {
		t.Fatalf("CreateFromDir: %v", err)
	}
	if size == 0 {
		t.Error("CreateFromDir reported a zero-byte archive")
	}

	a, err := Open(archive)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = a.Close() }()

	// The manifest is a sibling of the bare directory, never inside it, so it
	// cannot end up in the restored repository.
	if root, err := a.TopLevelDir(); err != nil || root != "MyRepo.git" {
		t.Errorf("TopLevelDir = %q, %v; want MyRepo.git", root, err)
	}
	manifest, err := a.ReadFile(manifestEntry)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", manifestEntry, err)
	}
	if !strings.Contains(string(manifest), `"schemaVersion":1`) {
		t.Errorf("manifest entry = %q", manifest)
	}

	dest := filepath.Join(base, "restored")
	if err := a.ExtractToDir(context.Background(), dest, "", nil); err != nil {
		t.Fatalf("ExtractToDir: %v", err)
	}

	want := treeDigest(t, src)
	got := treeDigest(t, filepath.Join(dest, "MyRepo.git"))

	for key, wantDigest := range want {
		gotDigest, ok := got[key]
		if !ok {
			t.Errorf("%s is missing after the round trip", key)
			continue
		}
		if gotDigest != wantDigest {
			t.Errorf("%s: digest %s, want %s", key, gotDigest, wantDigest)
		}
	}
	for key := range got {
		if _, ok := want[key]; !ok {
			t.Errorf("%s appeared out of nowhere", key)
		}
	}
}

func TestCreateStoresAlreadyCompressedEntries(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src")
	writeBareRepo(t, src)

	archive := filepath.Join(base, "MyRepo.git.zip")
	if _, err := CreateFromDir(context.Background(), src, "MyRepo.git", archive, nil, nil); err != nil {
		t.Fatalf("CreateFromDir: %v", err)
	}

	r, err := zip.OpenReader(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()

	// Deflating a pack file, an idx or an LFS blob spends CPU to save almost
	// nothing, because all three are already compressed.
	wantStored := map[string]bool{
		"MyRepo.git/objects/pack/pack-0123456789ab.pack": true,
		"MyRepo.git/objects/pack/pack-0123456789ab.idx":  true,
		"MyRepo.git/lfs/objects/ab/cd/abcdef0123456789":  true,
	}
	wantDeflated := map[string]bool{
		"MyRepo.git/HEAD":   true,
		"MyRepo.git/config": true,
	}

	for _, f := range r.File {
		switch {
		case wantStored[f.Name] && f.Method != zip.Store:
			t.Errorf("%s uses method %d, want Store", f.Name, f.Method)
		case wantDeflated[f.Name] && f.Method != zip.Deflate:
			t.Errorf("%s uses method %d, want Deflate", f.Name, f.Method)
		}
	}
}

func TestCreateLeavesNoPartFileBehind(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src")
	writeBareRepo(t, src)

	archive := filepath.Join(base, "MyRepo.git.zip")
	if _, err := CreateFromDir(context.Background(), src, "MyRepo.git", archive, nil, nil); err != nil {
		t.Fatalf("CreateFromDir: %v", err)
	}
	if _, err := os.Stat(archive + ".part"); !os.IsNotExist(err) {
		t.Errorf(".part file survived a successful create (stat err: %v)", err)
	}
}

func TestCreateAbandonsThePartFileOnCancellation(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src")
	writeBareRepo(t, src)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	archive := filepath.Join(base, "MyRepo.git.zip")
	if _, err := CreateFromDir(ctx, src, "MyRepo.git", archive, nil, nil); err == nil {
		t.Fatal("CreateFromDir succeeded despite a cancelled context")
	}
	// Neither a partial nor a final archive may remain: an archive that exists has
	// to mean an archive that is complete.
	if _, err := os.Stat(archive + ".part"); !os.IsNotExist(err) {
		t.Errorf(".part file left behind after cancellation (stat err: %v)", err)
	}
	if _, err := os.Stat(archive); !os.IsNotExist(err) {
		t.Errorf("archive created despite cancellation (stat err: %v)", err)
	}
}

func TestCreateOverwritesAnExistingArchive(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src")
	writeBareRepo(t, src)

	archive := filepath.Join(base, "MyRepo.git.zip")
	if err := os.WriteFile(archive, []byte("stale contents from an earlier run"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateFromDir(context.Background(), src, "MyRepo.git", archive, nil, nil); err != nil {
		t.Fatalf("CreateFromDir over an existing archive: %v", err)
	}
	a, err := Open(archive)
	if err != nil {
		t.Fatalf("the overwritten archive is not readable: %v", err)
	}
	// Closing matters on Windows, where an open handle blocks the temp directory
	// cleanup that follows.
	if err := a.Close(); err != nil {
		t.Error(err)
	}
}

func TestCreateReportsProgressUpToTheTotal(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src")
	writeBareRepo(t, src)

	var lastDone, lastTotal int64
	calls := 0
	_, err := CreateFromDir(context.Background(), src, "MyRepo.git",
		filepath.Join(base, "MyRepo.git.zip"), nil,
		func(done, total int64) {
			calls++
			lastDone, lastTotal = done, total
		})
	if err != nil {
		t.Fatalf("CreateFromDir: %v", err)
	}
	if calls == 0 {
		t.Fatal("progress was never reported")
	}
	if lastTotal == 0 || lastDone != lastTotal {
		t.Errorf("final progress %d/%d, want them equal and non-zero", lastDone, lastTotal)
	}
}

func TestExtractRenamesTheTopLevelDirectory(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src")
	writeBareRepo(t, src)

	// A legacy archive holds "<repo>/" where a current one holds "<repo>.git/".
	// Renaming during extraction is what lets both restore to the same layout.
	archive := filepath.Join(base, "MyRepo.git.7z")
	if _, err := CreateFromDir(context.Background(), src, "MyRepo", archive, nil, nil); err != nil {
		t.Fatalf("CreateFromDir: %v", err)
	}

	a, err := Open(archive)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = a.Close() }()

	dest := filepath.Join(base, "restored")
	if err := a.ExtractToDir(context.Background(), dest, "MyRepo.git", nil); err != nil {
		t.Fatalf("ExtractToDir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "MyRepo.git", "HEAD")); err != nil {
		t.Errorf("renamed tree is missing HEAD: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "MyRepo")); !os.IsNotExist(err) {
		t.Errorf("the original directory name survived the rename (stat err: %v)", err)
	}
}

func TestExtractPreservesTheExecutableBitWhereItIsRecorded(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not record an executable bit to preserve")
	}
	base := t.TempDir()
	src := filepath.Join(base, "src")
	writeBareRepo(t, src)

	archive := filepath.Join(base, "MyRepo.git.zip")
	if _, err := CreateFromDir(context.Background(), src, "MyRepo.git", archive, nil, nil); err != nil {
		t.Fatalf("CreateFromDir: %v", err)
	}
	a, err := Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()

	dest := filepath.Join(base, "restored")
	if err := a.ExtractToDir(context.Background(), dest, "", nil); err != nil {
		t.Fatalf("ExtractToDir: %v", err)
	}

	info, err := os.Stat(filepath.Join(dest, "MyRepo.git", "hooks", "pre-commit.sample"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("hook sample came back as %v, want it executable", info.Mode().Perm())
	}
}

func TestExtractRejectsZipSlip(t *testing.T) {
	base := t.TempDir()
	archive := filepath.Join(base, "evil.git.zip")

	f, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	// A legitimate-looking directory, plus an entry that climbs out of it.
	for _, name := range []string{"MyRepo.git/HEAD", "../escaped.txt"} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	a, err := Open(archive)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = a.Close() }()

	dest := filepath.Join(base, "restored")
	if err := a.ExtractToDir(context.Background(), dest, "", nil); err == nil {
		t.Fatal("extraction accepted an entry that escapes the destination")
	}
	if _, err := os.Stat(filepath.Join(base, "escaped.txt")); !os.IsNotExist(err) {
		t.Error("a file was written outside the destination directory")
	}
}

func TestOpenRejectsAGenuine7zFile(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "real.7z")
	// The 7z signature, which clonezip cannot read - and must say so plainly rather
	// than failing with a confusing parse error.
	if err := os.WriteFile(archive, append(magic7z, 0x00, 0x04), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Open(archive)
	if err == nil {
		t.Fatal("Open accepted a real 7z archive")
	}
	var formatErr *FormatError
	if !errors.As(err, &formatErr) {
		t.Fatalf("error is %T, want *FormatError", err)
	}
	if formatErr.Detected != "7z" {
		t.Errorf("Detected = %q, want 7z", formatErr.Detected)
	}
}

func TestOpenRejectsSomethingElseEntirely(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(archive, []byte("this is not an archive at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	var formatErr *FormatError
	if _, err := Open(archive); !errors.As(err, &formatErr) {
		t.Fatalf("error is %T, want *FormatError", err)
	}
}

func TestTopLevelDirRejectsAmbiguousArchives(t *testing.T) {
	base := t.TempDir()
	archive := filepath.Join(base, "two-roots.git.zip")

	f, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for _, name := range []string{"RepoA.git/HEAD", "RepoB.git/HEAD"} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte("ref: refs/heads/main\n")); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	a, err := Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()

	// Guessing which of two roots is the repository would restore the wrong thing
	// half the time, so this has to be refused.
	if root, err := a.TopLevelDir(); err == nil {
		t.Errorf("TopLevelDir = %q for an archive with two roots, want an error", root)
	}
}

func TestReadFileReportsAMissingEntry(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src")
	writeBareRepo(t, src)

	archive := filepath.Join(base, "MyRepo.git.zip")
	if _, err := CreateFromDir(context.Background(), src, "MyRepo.git", archive, nil, nil); err != nil {
		t.Fatalf("CreateFromDir: %v", err)
	}
	a, err := Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()

	// A legacy archive carries no manifest, and restore has to treat that as
	// normal rather than as a failure.
	if _, err := a.ReadFile(manifestEntry); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("ReadFile for a missing manifest = %v, want fs.ErrNotExist", err)
	}
}

func TestVerifyDetectsCorruption(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src")
	writeBareRepo(t, src)

	archive := filepath.Join(base, "MyRepo.git.zip")
	if _, err := CreateFromDir(context.Background(), src, "MyRepo.git", archive, nil, nil); err != nil {
		t.Fatalf("CreateFromDir: %v", err)
	}
	if err := Verify(context.Background(), archive); err != nil {
		t.Fatalf("Verify on a good archive: %v", err)
	}

	// Flip bytes in the middle of the compressed data. Reading every entry to the
	// end is what makes the per-entry CRC32 catch this.
	data, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	for i := len(data) / 2; i < len(data)/2+64 && i < len(data); i++ {
		data[i] ^= 0xFF
	}
	corrupt := filepath.Join(base, "corrupt.git.zip")
	if err := os.WriteFile(corrupt, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Verify(context.Background(), corrupt); err == nil {
		t.Error("Verify passed a corrupted archive")
	}
}

func TestUncompressedSizeAndEntryCount(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src")
	writeBareRepo(t, src)

	archive := filepath.Join(base, "MyRepo.git.zip")
	if _, err := CreateFromDir(context.Background(), src, "MyRepo.git", archive, nil, nil); err != nil {
		t.Fatalf("CreateFromDir: %v", err)
	}
	a, err := Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()

	// Used for the free-space check before extracting, so it has to be at least
	// the size of the biggest member.
	if got := a.UncompressedSize(); got < 3<<20 {
		t.Errorf("UncompressedSize = %d, want at least the 3 MiB pack", got)
	}
	if a.Entries() == 0 {
		t.Error("Entries = 0")
	}
}

// The premise of dropping the 7-Zip dependency is that "7z a -tzip" produced plain
// ZIP containers. This proves it against the real tool, in both directions.
func TestInteropWithReal7Zip(t *testing.T) {
	exe := find7Zip()
	if exe == "" {
		t.Skip("no 7z, 7zz or 7za available")
	}

	base := t.TempDir()
	src := filepath.Join(base, "MyRepo")
	writeBareRepo(t, src)

	// 7-Zip writes it, clonezip reads it: this is an existing .git.7z backup.
	legacy := filepath.Join(base, "MyRepo.git.7z")
	if out, err := exec.Command(exe, "a", "-tzip", legacy, src).CombinedOutput(); err != nil {
		t.Fatalf("7z a -tzip failed: %v\n%s", err, out)
	}

	a, err := Open(legacy)
	if err != nil {
		t.Fatalf("clonezip cannot read a 7z-authored archive: %v", err)
	}
	root, err := a.TopLevelDir()
	if err != nil {
		t.Fatalf("TopLevelDir: %v", err)
	}
	if root != "MyRepo" {
		t.Errorf("TopLevelDir = %q, want MyRepo (7-Zip stores only the leaf name)", root)
	}
	dest := filepath.Join(base, "from7z")
	if err := a.ExtractToDir(context.Background(), dest, "MyRepo.git", nil); err != nil {
		t.Fatalf("extracting a 7z-authored archive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "MyRepo.git", "HEAD")); err != nil {
		t.Errorf("HEAD missing after extracting a 7z-authored archive: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Error(err)
	}

	// clonezip writes it, 7-Zip reads it: existing tooling keeps working.
	mine := filepath.Join(base, "Mine.git.zip")
	if _, err := CreateFromDir(context.Background(), src, "MyRepo.git", mine, nil, nil); err != nil {
		t.Fatalf("CreateFromDir: %v", err)
	}
	if out, err := exec.Command(exe, "t", mine).CombinedOutput(); err != nil {
		t.Errorf("7z cannot verify an clonezip archive: %v\n%s", err, out)
	}
}

// find7Zip looks on PATH and then in the usual install locations.
func find7Zip() string {
	for _, name := range []string{"7z", "7zz", "7za"} {
		if path, err := exec.LookPath(name); err == nil {
			return path
		}
	}
	for _, path := range []string{`C:\Program Files\7-Zip\7z.exe`, "/usr/bin/7z", "/usr/local/bin/7zz"} {
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path
		}
	}
	return ""
}
