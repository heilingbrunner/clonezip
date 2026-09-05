package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"

	"github.com/heilingbrunner/clonezip/internal/common/fsx"
	"github.com/heilingbrunner/clonezip/internal/common/gitx"
	"github.com/heilingbrunner/clonezip/internal/common/layout"
	"github.com/heilingbrunner/clonezip/internal/common/manifest"
	"github.com/heilingbrunner/clonezip/internal/common/ziparc"
)

// Sentinels the caller turns into a precondition failure, so the exit code says
// "you need to do something" rather than "the work went wrong".
var (
	ErrTargetExists        = errors.New("target already exists")
	ErrShallowNeedsConfirm = errors.New("archive has truncated history")
	ErrNotAnArchive        = errors.New("not an clonezip archive")
)

// OriginSource and OriginBare are the choices for what the restored clone's origin
// points at.
const (
	OriginSource = "source"
	OriginBare   = "bare"
)

// RestoreOptions configures restoring one archive.
type RestoreOptions struct {
	ArchivePath string
	Dest        string

	// Origin selects what the working clone's origin points at: the Azure DevOps
	// URL from the manifest, or the local bare repository. Only consulted when Clone
	// is set, because without a clone there is nothing to point anywhere.
	Origin string

	// Clone also makes a working copy next to the bare. Off by default: the bare is
	// what the archive is restored for, and a working clone roughly doubles both the
	// disk used and the time taken for something the caller often does not need.
	Clone bool

	Force bool

	// ConfirmShallow accepts restoring an archive with truncated (shallow/single-branch)
	// history, which restore refuses by default since it cannot be recovered later.
	ConfirmShallow bool
	Verify         bool

	Git *gitx.Git
}

// RestoreResult describes what was restored.
type RestoreResult struct {
	Repo     string
	BarePath string

	// WorkPath is empty unless a working clone was asked for.
	WorkPath string

	// Cloned records whether the working copy was actually made.
	Cloned bool

	Shallow   bool
	LFSSeeded bool
	LFSBytes  int64
	Refs      int
	OriginURL string

	Manifest   *manifest.Manifest
	Warnings   []string
	DirtyFiles []string
}

// RunRestore extracts one archive into a bare repository, and optionally clones a
// working copy next to it.
//
// The bare is the point: it holds every ref that was backed up and is what you push to
// a new remote. A working clone is extra, so it is made only when asked for. If anything
// fails after extraction the bare is left in place too, because it is the valuable
// artifact - only a half-made working clone is cleaned up.
func RunRestore(ctx context.Context, o RestoreOptions, rep Reporter) (RestoreResult, error) {
	var result RestoreResult

	repo, ok := layout.RepoNameFromArchive(o.ArchivePath)
	if !ok {
		return result, fmt.Errorf("%w: %s does not end in %s or %s",
			ErrNotAnArchive, filepath.Base(o.ArchivePath),
			layout.ArchiveSuffix, layout.LegacyArchiveSuffix)
	}
	result.Repo = repo

	dest, err := filepath.Abs(o.Dest)
	if err != nil {
		return result, err
	}
	result.BarePath = filepath.Join(dest, repo+layout.BareSuffix)
	if o.Clone {
		result.WorkPath = filepath.Join(dest, repo)
	}

	archive, err := ziparc.Open(o.ArchivePath)
	if err != nil {
		return result, err
	}
	defer func() { _ = archive.Close() }()

	// Read the manifest before extracting anything, so a truncated archive can be
	// refused before it has written a single file.
	result.Manifest = readManifest(archive, rep)
	if result.Manifest != nil {
		result.Refs = len(result.Manifest.Refs)
		if result.Manifest.Truncated() {
			result.Shallow = true
			reportShallow(rep, result.Manifest)
			if !o.ConfirmShallow {
				return result, fmt.Errorf("%w: pass --confirm-shallow to restore it anyway", ErrShallowNeedsConfirm)
			}
		}
	}

	if err := o.checkTargets(result, rep); err != nil {
		return result, err
	}
	if err := checkFreeSpace(dest, archive, o.Clone); err != nil {
		return result, err
	}
	if err := o.extract(ctx, archive, result, repo, rep); err != nil {
		return result, err
	}

	// A legacy archive carries no manifest, so git's own marker is the only signal
	// left. Stopping here still leaves the bare, which is the useful part.
	if gitx.IsShallow(result.BarePath) && !result.Shallow {
		result.Shallow = true
		reportShallow(rep, nil)
		if !o.ConfirmShallow {
			what := "pass --confirm-shallow to accept it"
			if o.Clone {
				what = "pass --confirm-shallow to also create a working clone"
			}
			return result, fmt.Errorf(
				"%w: the extracted repository is shallow; %s is usable, %s",
				ErrShallowNeedsConfirm, result.BarePath, what)
		}
	}

	if o.Verify {
		if err := o.verify(ctx, &result, rep); err != nil {
			return result, err
		}
	}

	// Everything from here on exists only to make a usable working copy, so without
	// one the restore is already done.
	if !o.Clone {
		rep.Report(RepoLog{Level: LevelInfo,
			Line: "no working clone was requested; pass --clone to create one"})
		return result, nil
	}

	if err := o.cloneWorkingCopy(ctx, &result, rep); err != nil {
		// The bare survives; only the partial working clone goes.
		_ = fsx.RemoveAllRetry(result.WorkPath, "")
		return result, err
	}
	result.Cloned = true

	if err := o.restoreLFS(ctx, &result, rep); err != nil {
		return result, err
	}
	o.wireRemotes(ctx, &result, rep)
	o.checkClean(ctx, &result)

	return result, nil
}

// checkTargets refuses to overwrite what is already there.
//
// Deliberately stricter than backup, which overwrites archives by design: a
// working copy may hold uncommitted work, and silently replacing that is a different
// class of mistake from replacing a backup that can be remade.
// Only what is about to be written is checked: without a clone an existing <repo>
// directory is none of this command's business, and removing it under --force would be
// data loss for no gain.
func (o RestoreOptions) checkTargets(result RestoreResult, rep Reporter) error {
	targets := []string{result.BarePath}
	if o.Clone {
		targets = append(targets, result.WorkPath)
	}
	for _, target := range targets {
		if _, err := os.Lstat(target); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if !o.Force {
			return fmt.Errorf("%w: %s; remove it or pass --force", ErrTargetExists, target)
		}
		rep.Report(RepoLog{Level: LevelWarn, Line: "replacing " + target})
		if err := fsx.RemoveAllRetry(target, ""); err != nil {
			return err
		}
	}
	return nil
}

func checkFreeSpace(dest string, archive *ziparc.Archive, clone bool) error {
	free, err := fsx.FreeSpace(dest)
	if err != nil {
		// Not knowing is no reason to refuse; the write itself will report the truth.
		return nil
	}
	// With a working clone roughly twice the archive contents end up on disk once the
	// working tree is materialised; without one, only the extracted bare.
	needed := uint64(archive.UncompressedSize())
	if clone {
		needed *= 2
	}
	if free < needed {
		return fmt.Errorf("not enough space in %s: %d bytes free, about %d needed",
			dest, free, needed)
	}
	return nil
}

// extract unpacks the archive into a temporary directory and renames it into place,
// so a failure never leaves a half-named repository behind.
func (o RestoreOptions) extract(
	ctx context.Context,
	archive *ziparc.Archive,
	result RestoreResult,
	repo string,
	rep Reporter,
) error {
	rep.Report(RepoPhase{Phase: PhaseExtract, Detail: filepath.Base(o.ArchivePath)})

	staging := result.BarePath + ".tmp-" + strconv.Itoa(os.Getpid())
	if err := fsx.RemoveAllRetry(staging, ""); err != nil {
		return err
	}

	// The archive root is renamed to <repo>.git during extraction. A legacy archive
	// holds "<repo>/" and a current one "<repo>.git/"; renaming here handles both
	// without the extract-then-rename dance the script needed.
	bareName := repo + layout.BareSuffix
	if err := archive.ExtractToDir(ctx, staging, bareName, func(done, total int64) {
		rep.Report(RepoBytes{Done: done, Total: total})
	}); err != nil {
		_ = fsx.RemoveAllRetry(staging, "")
		return err
	}

	extracted := filepath.Join(staging, bareName)
	if _, err := os.Stat(extracted); err != nil {
		_ = fsx.RemoveAllRetry(staging, "")
		return fmt.Errorf("archive did not contain %s: %w", bareName, err)
	}
	if err := os.Rename(extracted, result.BarePath); err != nil {
		_ = fsx.RemoveAllRetry(staging, "")
		return err
	}
	// Whatever else the archive held at its root, such as the manifest, is not part
	// of the repository.
	_ = fsx.RemoveAllRetry(staging, "")
	return nil
}

// verify checks that the extracted repository is internally consistent, and that it
// still holds what the manifest says was captured.
func (o RestoreOptions) verify(ctx context.Context, result *RestoreResult, rep Reporter) error {
	rep.Report(RepoPhase{Phase: PhaseVerify})

	if err := o.Git.Fsck(ctx, result.BarePath, nil); err != nil {
		return fmt.Errorf("the extracted repository failed git fsck: %w", err)
	}

	refs, err := o.Git.RefNames(ctx, result.BarePath)
	if err != nil {
		return err
	}
	if result.Manifest != nil && len(result.Manifest.Refs) > 0 && len(refs) != len(result.Manifest.Refs) {
		// Worth saying out loud rather than failing: a ref may legitimately have been
		// removed since, but a mismatch usually means truncation.
		result.Warnings = append(result.Warnings, fmt.Sprintf(
			"archive holds %d refs but its manifest recorded %d",
			len(refs), len(result.Manifest.Refs)))
	}
	result.Refs = len(refs)
	return nil
}

// cloneWorkingCopy makes the usable clone next to the bare.
func (o RestoreOptions) cloneWorkingCopy(ctx context.Context, result *RestoreResult, rep Reporter) error {
	rep.Report(RepoPhase{Phase: PhaseRestore, Detail: "cloning working copy"})

	sink := func(_ gitx.Stream, line string) {
		rep.Report(RepoLog{Level: LevelInfo, Line: line})
	}

	if !result.Shallow {
		return o.Git.Clone(ctx, result.BarePath, result.WorkPath, false, sink)
	}

	// A local clone hardlink-copies objects, which git refuses outright when the
	// source is shallow ("source repository is shallow, reject to clone"), and which
	// would not carry the shallow marker across even where it succeeds. Going through
	// the git transport negotiates the boundary correctly.
	err := o.Git.Clone(ctx, result.BarePath, result.WorkPath, true, sink)
	if err == nil {
		return nil
	}
	rep.Report(RepoLog{Level: LevelWarn,
		Line: "clone over file:// failed, retrying without hardlinks: " + shortReason(err)})

	if err := o.Git.CloneNoHardlinks(ctx, result.BarePath, result.WorkPath, sink); err != nil {
		return err
	}
	// Without the marker the working clone has a truncated object graph and no way to
	// know it, which git then reports as corruption.
	return copyFile(
		filepath.Join(result.BarePath, "shallow"),
		filepath.Join(result.WorkPath, ".git", "shallow"),
	)
}

// restoreLFS seeds the working clone's LFS cache from the archive and materialises the
// tracked files. This is what makes an archive self-sufficient: the objects come from
// the backup, not from an LFS server that may no longer exist.
func (o RestoreOptions) restoreLFS(ctx context.Context, result *RestoreResult, rep Reporter) error {
	rep.Report(RepoPhase{Phase: PhaseLFS, Detail: "initialising"})
	if err := o.Git.LFSInstallLocal(ctx, result.WorkPath); err != nil {
		result.Warnings = append(result.Warnings, "git lfs install failed: "+shortReason(err))
		return nil
	}

	source := filepath.Join(result.BarePath, "lfs", "objects")
	size, err := fsx.DirSize(source)
	if err != nil || size == 0 {
		// An old backup, or a repository that never used LFS. Either way the clone is
		// usable; LFS-tracked files simply remain pointer files.
		rep.Report(RepoLog{Level: LevelInfo,
			Line: "no LFS objects in the archive; any LFS-tracked files stay as pointer files"})
		return nil
	}

	rep.Report(RepoPhase{Phase: PhaseLFS, Detail: "seeding object cache"})
	target := filepath.Join(result.WorkPath, ".git", "lfs", "objects")
	if err := copyTree(source, target); err != nil {
		result.Warnings = append(result.Warnings, "could not seed the LFS cache: "+err.Error())
		return nil
	}
	result.LFSSeeded = true
	result.LFSBytes = size

	rep.Report(RepoPhase{Phase: PhaseLFS, Detail: "materialising files"})
	if err := o.Git.LFSCheckout(ctx, result.WorkPath, nil); err != nil {
		// Not fatal: the repository is complete, some large files are still stubs.
		result.Warnings = append(result.Warnings, fmt.Sprintf(
			"git lfs checkout reported errors (%s); inspect with: git -C %s lfs fsck",
			shortReason(err), result.WorkPath))
	}
	return nil
}

// wireRemotes points the working clone somewhere useful.
//
// Without this the restored clone's origin is a local directory, so nobody can ever
// push the work back. The manifest is what makes the real URL recoverable.
func (o RestoreOptions) wireRemotes(ctx context.Context, result *RestoreResult, rep Reporter) {
	sourceURL := ""
	if result.Manifest != nil {
		sourceURL = result.Manifest.Source.URL
	}

	if o.Origin == OriginSource && sourceURL != "" {
		if err := o.Git.SetRemoteURL(ctx, result.WorkPath, "origin", sourceURL); err != nil {
			result.Warnings = append(result.Warnings, "could not set origin: "+shortReason(err))
			return
		}
		result.OriginURL = sourceURL
		// Keep a way back to the bare, which holds every backed-up ref.
		if err := o.Git.AddRemote(ctx, result.WorkPath, "backup", result.BarePath); err != nil {
			rep.Report(RepoLog{Level: LevelInfo,
				Line: "could not add the backup remote: " + shortReason(err)})
		}
		return
	}

	result.OriginURL = result.BarePath
	if o.Origin == OriginSource && sourceURL == "" {
		result.Warnings = append(result.Warnings,
			"no manifest in this archive, so origin still points at the local bare repository")
	}
}

// checkClean reports a freshly cloned working tree that is already modified, which is
// the classic symptom of a line-ending or .gitattributes mismatch. It is detected
// rather than corrected: changing the user's core.autocrlf would make the restored
// tree differ from what their normal clone produces.
func (o RestoreOptions) checkClean(ctx context.Context, result *RestoreResult) {
	lines, err := o.Git.StatusPorcelain(ctx, result.WorkPath)
	if err != nil || len(lines) == 0 {
		return
	}
	result.DirtyFiles = lines
	result.Warnings = append(result.Warnings, fmt.Sprintf(
		"%d file(s) already differ in a fresh clone, which usually means a line-ending "+
			"or .gitattributes mismatch", len(lines)))
}

// readManifest returns the archive's manifest, or nil when it has none.
func readManifest(archive *ziparc.Archive, rep Reporter) *manifest.Manifest {
	data, err := archive.ReadFile(manifest.EntryName)
	if err != nil {
		// Normal for an archive written by the PowerShell scripts.
		rep.Report(RepoLog{Level: LevelInfo, Line: "no manifest in this archive; it predates clonezip"})
		return nil
	}
	man, err := manifest.Parse(data)
	if err != nil {
		rep.Report(RepoLog{Level: LevelWarn, Line: err.Error()})
		return nil
	}
	return man
}

// reportShallow explains what a truncated archive does and does not contain. This is
// deliberately prominent: restoring one and assuming it holds the full history is a
// mistake that only surfaces much later.
func reportShallow(rep Reporter, man *manifest.Manifest) {
	lines := []string{
		"SHALLOW ARCHIVE - history is truncated",
		"Commits before the shallow boundary are not in this archive and cannot be recovered from it.",
	}
	if man != nil {
		if man.Clone.Depth > 0 {
			lines = append(lines, fmt.Sprintf("Captured %s with --clone-depth %d, %d ref(s).",
				man.CreatedAt.Format("2006-01-02"), man.Clone.Depth, len(man.Refs)))
		}
		if man.Source.URL != "" {
			lines = append(lines,
				"To deepen it you need the original remote:",
				"  git -C <repo> remote add source "+man.Source.URL,
				"  git -C <repo> fetch --unshallow source")
		}
	}
	for _, line := range lines {
		rep.Report(RepoLog{Level: LevelWarn, Line: line})
	}
}

// copyTree copies a directory tree, used for the LFS object cache.
//
// The script used Copy-Item with a hardcoded backslash path; this works on both
// platforms and reports what went wrong.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)

		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		return copyFile(path, target)
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	// Read-only; the write handle's close error is returned below, which is the one
	// that could indicate lost data.
	defer func() { _ = in.Close() }()

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

// SummaryLines renders the closing report for a restore, so the caller does not have
// to know the shape of the result.
func (r RestoreResult) SummaryLines() []string {
	lines := []string{
		"Restore complete.",
		"  bare          " + r.BarePath,
	}
	if r.Cloned {
		lines = append(lines, "  working copy  "+r.WorkPath)
	} else {
		lines = append(lines, "  clone         not created (pass --clone)")
	}
	if r.Refs > 0 {
		lines = append(lines, "  refs          "+strconv.Itoa(r.Refs))
	}
	if r.LFSSeeded {
		lines = append(lines, "  lfs           seeded from the archive")
	}
	if r.OriginURL != "" {
		lines = append(lines, "  origin        "+r.OriginURL)
	}
	if r.Shallow {
		lines = append(lines, "  history       TRUNCATED - this is a shallow archive")
	}
	if len(r.Warnings) > 0 {
		lines = append(lines, "", "Warnings:")
		for _, w := range r.Warnings {
			lines = append(lines, "  "+w)
		}
	}
	return lines
}

// dirtySample is how many modified paths are worth showing before it becomes noise.
const dirtySample = 5

// DirtySample returns a short excerpt of the unexpectedly modified files.
func (r RestoreResult) DirtySample() []string {
	if len(r.DirtyFiles) <= dirtySample {
		return r.DirtyFiles
	}
	out := append([]string(nil), r.DirtyFiles[:dirtySample]...)
	return append(out, fmt.Sprintf("... and %d more", len(r.DirtyFiles)-dirtySample))
}
