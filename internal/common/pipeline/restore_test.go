package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/heilingbrunner/clonezip/internal/common/gitx"
	"github.com/heilingbrunner/clonezip/internal/common/manifest"
	"github.com/heilingbrunner/clonezip/internal/common/ziparc"
)

// bareFixture describes the archive a test wants built.
type bareFixture struct {
	// Repo is the repository name, which decides the archive filename.
	Repo string

	// RootName is the archive's top-level directory. A current archive holds
	// "<repo>.git", a PowerShell-era one holds "<repo>".
	RootName string

	// Suffix is the archive extension, ".git.zip" or the legacy ".git.7z".
	Suffix string

	// WithLFS adds an LFS payload to be seeded into the working clone.
	WithLFS bool

	// Shallow adds git's own truncation marker.
	Shallow bool

	// Manifest, when false, produces an archive with no manifest at all, as the
	// PowerShell scripts wrote.
	Manifest bool

	// Depth is recorded in the manifest.
	Depth int
}

const fixtureSourceURL = "https://dev.azure.com/org/Proj/_git/MyRepo"

// buildArchive writes a bare-repo-shaped tree and archives it for real, so restore is
// exercised against an actual zip rather than a stub.
func buildArchive(t *testing.T, dir string, f bareFixture) string {
	t.Helper()

	if f.Repo == "" {
		f.Repo = "MyRepo"
	}
	if f.RootName == "" {
		f.RootName = f.Repo + ".git"
	}
	if f.Suffix == "" {
		f.Suffix = ".git.zip"
	}

	src := filepath.Join(dir, "src-"+f.Repo)
	files := map[string]string{
		"HEAD":            "ref: refs/heads/main\n",
		"config":          "[core]\n\tbare = true\n",
		"refs/heads/main": "9f3c1ab000000000000000000000000000000000\n",
	}
	if f.WithLFS {
		files["lfs/objects/ab/cd/abcdef0123456789"] = "large file payload"
	}
	if f.Shallow {
		files["shallow"] = "9f3c1ab000000000000000000000000000000000\n"
	}
	for rel, content := range files {
		path := filepath.Join(src, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	extra := map[string][]byte{}
	if f.Manifest {
		man := manifest.New("test", time.Date(2026, 7, 27, 16, 42, 11, 0, time.UTC))
		man.Source = manifest.Source{
			URL:          fixtureSourceURL,
			Host:         "dev.azure.com",
			Organization: "org",
			Project:      "Proj",
			Repo:         f.Repo,
			HeadRef:      "refs/heads/main",
		}
		man.Clone = manifest.Clone{Bare: true, Depth: f.Depth, Shallow: f.Shallow}
		man.Archive = manifest.Archive{BareDir: f.RootName}
		man.Refs = []manifest.Ref{{Name: "refs/heads/main"}}
		data, err := man.JSON()
		if err != nil {
			t.Fatal(err)
		}
		extra[manifest.EntryName] = data
	}

	archive := filepath.Join(dir, f.Repo+f.Suffix)
	if _, err := ziparc.CreateFromDir(context.Background(), src, f.RootName, archive, extra, nil); err != nil {
		t.Fatalf("CreateFromDir: %v", err)
	}
	return archive
}

// restore runs a restore against a fake git.
func restore(t *testing.T, archive, dest string, fake *gitx.FakeRunner,
	adjust func(*RestoreOptions),
) (RestoreResult, *recordingReporter, error) {
	t.Helper()

	// Cloning is opt-in for the caller, but most of these tests are about the working
	// clone, so the helper asks for one unless a test says otherwise.
	opts := RestoreOptions{
		ArchivePath: archive,
		Dest:        dest,
		Origin:      OriginSource,
		Clone:       true,
		Git:         gitx.New(fake),
	}
	if adjust != nil {
		adjust(&opts)
	}

	rep := &recordingReporter{}
	result, err := RunRestore(context.Background(), opts, rep)
	return result, rep, err
}

func TestRunRestoreHappyPath(t *testing.T) {
	base := t.TempDir()
	archive := buildArchive(t, base, bareFixture{Manifest: true, WithLFS: true})
	dest := filepath.Join(base, "restored")

	fake := gitx.NewFakeRunner().Respond("for-each-ref", gitx.Response{
		Stdout: []string{"refs/heads/main"},
	})

	result, _, err := restore(t, archive, dest, fake, nil)
	if err != nil {
		t.Fatalf("RunRestore: %v", err)
	}

	// The bare is kept next to the working copy: it holds every backed-up ref and is
	// what you push to a new remote.
	if _, err := os.Stat(filepath.Join(result.BarePath, "HEAD")); err != nil {
		t.Errorf("bare repository is missing HEAD: %v", err)
	}
	if filepath.Base(result.BarePath) != "MyRepo.git" {
		t.Errorf("bare path = %q, want it to end in MyRepo.git", result.BarePath)
	}

	// The manifest must not end up inside the restored repository, or git would see a
	// stray file.
	if _, err := os.Stat(filepath.Join(result.BarePath, manifest.EntryName)); !os.IsNotExist(err) {
		t.Error("the manifest was extracted into the bare repository")
	}

	// Origin points at the real remote, which is the only reason the manifest records
	// it: without this nobody can ever push the restored work back.
	if result.OriginURL != fixtureSourceURL {
		t.Errorf("origin = %q, want %q", result.OriginURL, fixtureSourceURL)
	}
	if len(fake.CallsMatching("remote add backup")) != 1 {
		t.Error("no backup remote was added for the local bare")
	}

	if !result.LFSSeeded {
		t.Error("the LFS cache was not seeded from the archive")
	}
	seeded := filepath.Join(result.WorkPath, ".git", "lfs", "objects", "ab", "cd", "abcdef0123456789")
	if _, err := os.Stat(seeded); err != nil {
		t.Errorf("LFS object was not copied into the working clone: %v", err)
	}
	if len(fake.CallsMatching("lfs checkout")) != 1 {
		t.Error("git lfs checkout did not run, so LFS files would stay as pointers")
	}

	if result.Shallow {
		t.Error("a full archive was reported as shallow")
	}
	if lines := strings.Join(result.SummaryLines(), "\n"); !strings.Contains(lines, "Restore complete.") {
		t.Errorf("summary does not report completion:\n%s", lines)
	}
}

func TestRunRestoreLeavesNoTemporaryDirectory(t *testing.T) {
	base := t.TempDir()
	archive := buildArchive(t, base, bareFixture{Manifest: true})
	dest := filepath.Join(base, "restored")

	if _, _, err := restore(t, archive, dest, gitx.NewFakeRunner(), nil); err != nil {
		t.Fatalf("RunRestore: %v", err)
	}

	// Extraction goes through a .tmp-<pid> directory so a failure never leaves a
	// half-named repository; on success it must be gone.
	entries, err := os.ReadDir(dest)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp-") {
			t.Errorf("temporary directory left behind: %s", entry.Name())
		}
	}
}

func TestRunRestoreRefusesAnExistingTarget(t *testing.T) {
	base := t.TempDir()
	archive := buildArchive(t, base, bareFixture{Manifest: true})
	dest := filepath.Join(base, "restored")

	// A working copy may hold uncommitted work, so replacing one silently is a
	// different class of mistake from overwriting a backup that can be remade.
	existing := filepath.Join(dest, "MyRepo")
	if err := os.MkdirAll(existing, 0o755); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(existing, "uncommitted.txt")
	if err := os.WriteFile(keep, []byte("work in progress"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, _, err := restore(t, archive, dest, gitx.NewFakeRunner(), nil)
	if !errors.Is(err, ErrTargetExists) {
		t.Fatalf("error = %v, want ErrTargetExists", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("the existing working copy was disturbed: %v", err)
	}

	// With --force it proceeds and replaces it.
	if _, _, err := restore(t, archive, dest, gitx.NewFakeRunner(), func(o *RestoreOptions) {
		o.Force = true
	}); err != nil {
		t.Fatalf("RunRestore with --force: %v", err)
	}
	if _, err := os.Stat(keep); !os.IsNotExist(err) {
		t.Error("--force did not replace the existing working copy")
	}
}

func TestRunRestoreWithoutCloneLeavesOnlyTheBare(t *testing.T) {
	base := t.TempDir()
	archive := buildArchive(t, base, bareFixture{Manifest: true, WithLFS: true})
	dest := filepath.Join(base, "restored")

	fake := gitx.NewFakeRunner()
	result, _, err := restore(t, archive, dest, fake, func(o *RestoreOptions) { o.Clone = false })
	if err != nil {
		t.Fatalf("RunRestore: %v", err)
	}

	// The bare is the valuable artifact, so it is still extracted in full.
	if _, err := os.Stat(filepath.Join(result.BarePath, "HEAD")); err != nil {
		t.Errorf("bare repository is missing HEAD: %v", err)
	}
	if result.Cloned {
		t.Error("the result claims a working clone was made")
	}
	if result.WorkPath != "" {
		t.Errorf("WorkPath = %q, want empty when no clone was asked for", result.WorkPath)
	}
	if _, err := os.Stat(filepath.Join(dest, "MyRepo")); !os.IsNotExist(err) {
		t.Error("a working clone was created even though cloning was not requested")
	}

	// Every one of these only makes sense against a working clone, so none of them
	// should be attempted.
	for _, unwanted := range []string{"clone", "lfs install", "lfs checkout", "remote add", "status --porcelain"} {
		if calls := fake.CallsMatching(unwanted); len(calls) > 0 {
			t.Errorf("git %q ran without a working clone: %v", unwanted, calls)
		}
	}

	if lines := strings.Join(result.SummaryLines(), "\n"); strings.Contains(lines, "working copy") {
		t.Errorf("summary reports a working copy that was never created:\n%s", lines)
	}
}

func TestRunRestoreWithoutCloneLeavesAnExistingWorkingCopyAlone(t *testing.T) {
	base := t.TempDir()
	archive := buildArchive(t, base, bareFixture{Manifest: true})
	dest := filepath.Join(base, "restored")

	// A working copy from an earlier restore, possibly holding uncommitted work.
	// Nothing is going to be written there, so --force must not reach it either:
	// deleting it would be pure data loss for no gain.
	existing := filepath.Join(dest, "MyRepo")
	if err := os.MkdirAll(existing, 0o755); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(existing, "uncommitted.txt")
	if err := os.WriteFile(keep, []byte("work in progress"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, _, err := restore(t, archive, dest, gitx.NewFakeRunner(), func(o *RestoreOptions) {
		o.Clone = false
		o.Force = true
	}); err != nil {
		t.Fatalf("RunRestore: %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("an existing working copy was removed even though no clone was requested: %v", err)
	}
}

func TestRunRestoreRefusesAShallowArchiveWithoutConfirmation(t *testing.T) {
	base := t.TempDir()
	archive := buildArchive(t, base, bareFixture{Manifest: true, Shallow: true, Depth: 1})
	dest := filepath.Join(base, "restored")

	result, rep, err := restore(t, archive, dest, gitx.NewFakeRunner(), nil)
	if !errors.Is(err, ErrShallowNeedsConfirm) {
		t.Fatalf("error = %v, want ErrShallowNeedsConfirm", err)
	}
	if !result.Shallow {
		t.Error("the result does not record that the archive is shallow")
	}

	// Detected from the manifest before extraction, so a refused restore leaves no
	// trace at all.
	if entries, err := os.ReadDir(dest); err == nil && len(entries) > 0 {
		t.Errorf("%d entries were written despite the refusal: %v", len(entries), entries)
	}

	// The warning has to be unmissable, and has to say how to recover the history.
	var banner []string
	for _, event := range rep.Events() {
		if log, ok := event.(RepoLog); ok && log.Level == LevelWarn {
			banner = append(banner, log.Line)
		}
	}
	joined := strings.Join(banner, "\n")
	for _, want := range []string{"SHALLOW ARCHIVE", "fetch --unshallow", fixtureSourceURL} {
		if !strings.Contains(joined, want) {
			t.Errorf("shallow banner does not mention %q:\n%s", want, joined)
		}
	}
}

func TestRunRestoreShallowWithYesUsesTheGitTransport(t *testing.T) {
	base := t.TempDir()
	archive := buildArchive(t, base, bareFixture{Manifest: true, Shallow: true, Depth: 1})
	dest := filepath.Join(base, "restored")

	fake := gitx.NewFakeRunner()
	result, _, err := restore(t, archive, dest, fake, func(o *RestoreOptions) { o.ConfirmShallow = true })
	if err != nil {
		t.Fatalf("RunRestore: %v", err)
	}
	if !result.Shallow {
		t.Error("the result does not record that history is truncated")
	}

	// A local clone hardlink-copies objects, which git refuses outright for a shallow
	// source: "source repository is shallow, reject to clone". Going through the git
	// transport is what negotiates the boundary correctly.
	clones := fake.CallsMatching("clone --progress")
	if len(clones) == 0 {
		t.Fatal("no working clone was attempted")
	}
	joined := clones[0].Joined()
	if !strings.Contains(joined, "--no-local") {
		t.Errorf("clone args %q missing --no-local", joined)
	}
	if !strings.Contains(joined, "file://") {
		t.Errorf("clone args %q should address the bare as a file:// URL", joined)
	}

	if lines := strings.Join(result.SummaryLines(), "\n"); !strings.Contains(lines, "TRUNCATED") {
		t.Errorf("summary does not flag the truncated history:\n%s", lines)
	}
}

func TestRunRestoreShallowFallsBackToNoHardlinks(t *testing.T) {
	base := t.TempDir()
	archive := buildArchive(t, base, bareFixture{Manifest: true, Shallow: true, Depth: 1})
	dest := filepath.Join(base, "restored")

	// Some git configurations refuse --no-local over file://; the fallback copies
	// objects instead and carries the shallow marker across by hand, because without
	// it the working clone looks corrupt.
	fake := gitx.NewFakeRunner().Fail("--no-local", 128, "fatal: cannot clone")

	result, _, err := restore(t, archive, dest, fake, func(o *RestoreOptions) { o.ConfirmShallow = true })
	if err != nil {
		t.Fatalf("RunRestore: %v", err)
	}
	if len(fake.CallsMatching("--no-hardlinks")) != 1 {
		t.Error("the --no-hardlinks fallback was not used")
	}
	marker := filepath.Join(result.WorkPath, ".git", "shallow")
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("the shallow marker was not carried into the working clone: %v", err)
	}
}

func TestRunRestoreHandlesALegacyArchive(t *testing.T) {
	base := t.TempDir()
	// What the PowerShell script produced: a ".git.7z" name holding "<repo>/" and no
	// manifest at all.
	archive := buildArchive(t, base, bareFixture{
		RootName: "MyRepo",
		Suffix:   ".git.7z",
		Manifest: false,
	})
	dest := filepath.Join(base, "restored")

	result, rep, err := restore(t, archive, dest, gitx.NewFakeRunner(), nil)
	if err != nil {
		t.Fatalf("RunRestore on a legacy archive: %v", err)
	}

	// The top-level directory is renamed during extraction, so both archive layouts
	// restore to the same shape.
	if _, err := os.Stat(filepath.Join(result.BarePath, "HEAD")); err != nil {
		t.Errorf("legacy archive did not restore to <repo>.git: %v", err)
	}
	if result.Manifest != nil {
		t.Error("a manifest was reported for an archive that has none")
	}

	// Without a manifest there is no source URL, so origin stays on the bare - and
	// that has to be said rather than left as a surprise.
	if result.OriginURL != result.BarePath {
		t.Errorf("origin = %q, want the local bare %q", result.OriginURL, result.BarePath)
	}
	if len(result.Warnings) == 0 {
		t.Error("no warning that origin could not be pointed at the real remote")
	}
	if !rep.has(func(e Event) bool {
		log, ok := e.(RepoLog)
		return ok && strings.Contains(log.Line, "predates clonezip")
	}) {
		t.Error("the missing manifest was not reported")
	}
}

func TestRunRestoreRejectsAWrongFilename(t *testing.T) {
	base := t.TempDir()
	notes := filepath.Join(base, "notes.txt")
	if err := os.WriteFile(notes, []byte("not an archive"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, _, err := restore(t, notes, base, gitx.NewFakeRunner(), nil); !errors.Is(err, ErrNotAnArchive) {
		t.Fatalf("error = %v, want ErrNotAnArchive", err)
	}
}

func TestRunRestoreRejectsAGenuine7zArchive(t *testing.T) {
	base := t.TempDir()
	// Correctly named, but actually 7z-compressed rather than a zip container. The
	// message has to say so plainly instead of failing as a parse error.
	fake7z := filepath.Join(base, "Real.git.7z")
	if err := os.WriteFile(fake7z, []byte{'7', 'z', 0xBC, 0xAF, 0x27, 0x1C, 0, 4}, 0o644); err != nil {
		t.Fatal(err)
	}

	_, _, err := restore(t, fake7z, base, gitx.NewFakeRunner(), nil)
	var formatErr *ziparc.FormatError
	if !errors.As(err, &formatErr) {
		t.Fatalf("error = %v, want *ziparc.FormatError", err)
	}
}

func TestRunRestoreVerifyReportsARefMismatch(t *testing.T) {
	base := t.TempDir()
	archive := buildArchive(t, base, bareFixture{Manifest: true}) // manifest records 1 ref
	dest := filepath.Join(base, "restored")

	// The extracted repository reports more refs than were captured, which usually
	// means the archive and its manifest disagree.
	fake := gitx.NewFakeRunner().Respond("for-each-ref", gitx.Response{
		Stdout: []string{"refs/heads/main", "refs/heads/extra", "refs/tags/v1"},
	})

	result, _, err := restore(t, archive, dest, fake, func(o *RestoreOptions) { o.Verify = true })
	if err != nil {
		t.Fatalf("RunRestore: %v", err)
	}
	if len(fake.CallsMatching("fsck")) != 1 {
		t.Error("git fsck did not run despite --verify")
	}

	var found bool
	for _, warning := range result.Warnings {
		if strings.Contains(warning, "manifest recorded") {
			found = true
		}
	}
	if !found {
		t.Errorf("no warning about the ref mismatch: %v", result.Warnings)
	}
}

func TestRunRestoreKeepsTheBareWhenCloningFails(t *testing.T) {
	base := t.TempDir()
	archive := buildArchive(t, base, bareFixture{Manifest: true})
	dest := filepath.Join(base, "restored")

	fake := gitx.NewFakeRunner().Fail("clone --progress", 128, "fatal: could not create work tree")

	result, _, err := restore(t, archive, dest, fake, nil)
	if err == nil {
		t.Fatal("RunRestore succeeded despite the clone failing")
	}
	// The bare is the valuable artifact - it holds every ref that was backed up - so a
	// failure after extraction must not throw it away.
	if _, statErr := os.Stat(filepath.Join(result.BarePath, "HEAD")); statErr != nil {
		t.Errorf("the bare repository was discarded on failure: %v", statErr)
	}
	if _, statErr := os.Stat(result.WorkPath); !os.IsNotExist(statErr) {
		t.Error("a partial working clone was left behind")
	}
}

func TestRunRestoreReportsADirtyFreshClone(t *testing.T) {
	base := t.TempDir()
	archive := buildArchive(t, base, bareFixture{Manifest: true})
	dest := filepath.Join(base, "restored")

	// A fresh clone that is already modified is the classic line-ending or
	// .gitattributes symptom. It is detected and reported, never silently corrected.
	dirty := []string{
		" M src/a.txt", " M src/b.txt", " M src/c.txt",
		" M src/d.txt", " M src/e.txt", " M src/f.txt",
	}
	fake := gitx.NewFakeRunner().Respond("status --porcelain", gitx.Response{Stdout: dirty})

	result, _, err := restore(t, archive, dest, fake, nil)
	if err != nil {
		t.Fatalf("RunRestore: %v", err)
	}
	if len(result.DirtyFiles) != len(dirty) {
		t.Errorf("DirtyFiles = %d, want %d", len(result.DirtyFiles), len(dirty))
	}

	sample := result.DirtySample()
	if len(sample) != dirtySample+1 {
		t.Fatalf("DirtySample returned %d lines, want %d plus a summary", len(sample), dirtySample)
	}
	if !strings.Contains(sample[len(sample)-1], "and 1 more") {
		t.Errorf("the sample does not say how many were omitted: %q", sample[len(sample)-1])
	}

	var found bool
	for _, warning := range result.Warnings {
		if strings.Contains(warning, "line-ending") {
			found = true
		}
	}
	if !found {
		t.Errorf("no warning explaining the dirty clone: %v", result.Warnings)
	}
}

func TestRunRestoreWithoutLFSPayload(t *testing.T) {
	base := t.TempDir()
	archive := buildArchive(t, base, bareFixture{Manifest: true, WithLFS: false})
	dest := filepath.Join(base, "restored")

	fake := gitx.NewFakeRunner()
	result, rep, err := restore(t, archive, dest, fake, nil)
	if err != nil {
		t.Fatalf("RunRestore: %v", err)
	}

	// A repository that never used LFS, or an archive from before the LFS fix. The
	// clone is still usable and nothing should pretend otherwise.
	if result.LFSSeeded {
		t.Error("LFS was reported as seeded when the archive had no objects")
	}
	if len(fake.CallsMatching("lfs checkout")) != 0 {
		t.Error("git lfs checkout ran with no objects to check out")
	}
	if !rep.has(func(e Event) bool {
		log, ok := e.(RepoLog)
		return ok && strings.Contains(log.Line, "pointer files")
	}) {
		t.Error("the absent LFS payload was not explained")
	}
}

func TestRunRestoreOriginBare(t *testing.T) {
	base := t.TempDir()
	archive := buildArchive(t, base, bareFixture{Manifest: true})
	dest := filepath.Join(base, "restored")

	fake := gitx.NewFakeRunner()
	result, _, err := restore(t, archive, dest, fake, func(o *RestoreOptions) {
		o.Origin = OriginBare
	})
	if err != nil {
		t.Fatalf("RunRestore: %v", err)
	}
	if result.OriginURL != result.BarePath {
		t.Errorf("origin = %q, want the bare %q", result.OriginURL, result.BarePath)
	}
	if len(fake.CallsMatching("remote set-url")) != 0 {
		t.Error("origin was rewritten despite --origin=bare")
	}
}
