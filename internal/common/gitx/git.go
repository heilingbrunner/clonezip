package gitx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// maxRefArgsLen bounds the joined length of the ref arguments passed to one
// "git lfs fetch" call.
//
// The PowerShell script splatted every ref onto a single command line. Windows
// caps a command line at 32767 characters, so a repository with enough tags fails
// with a confusing error. 8000 leaves generous room for the executable path and
// the fixed arguments.
const maxRefArgsLen = 8000

// Git is the set of git operations clonezip needs.
type Git struct {
	R Runner
}

// New returns a Git driven by the given runner.
func New(r Runner) *Git { return &Git{R: r} }

// globalArgs precede every subcommand.
//
// core.longpaths is passed rather than relied upon: it is unset by default, and a
// deep object path inside a nested run directory is exactly where MAX_PATH bites.
// git ignores it on Linux, so it is unconditional.
func globalArgs() []string {
	return []string{"-c", "core.longpaths=true"}
}

func (g *Git) run(ctx context.Context, dir string, sink LineSink, args ...string) error {
	_, err := g.R.Run(ctx, dir, append(globalArgs(), args...), sink)
	return err
}

// CloneBare clones url into dir as a bare repository.
//
// A depth of 0 means a full clone. A positive depth also passes
// --no-single-branch, which is mandatory: --depth implies --single-branch, and a
// backup that silently contained only the default branch would be data loss
// dressed up as success.
func (g *Git) CloneBare(ctx context.Context, url, dir string, depth int, sink LineSink) error {
	args := []string{"clone", "--bare", "--progress"}
	if depth > 0 {
		args = append(args, fmt.Sprintf("--depth=%d", depth), "--no-single-branch")
	}
	args = append(args, url, dir)
	// Run with no working directory: the target does not exist yet.
	return g.run(ctx, "", sink, args...)
}

// Clone makes a working clone of a local bare repository.
//
// noLocal forces the git transport instead of the default hardlink copy. That is
// required when the source is shallow: a local clone refuses with "source
// repository is shallow, reject to clone", and even where it succeeds it does not
// propagate the shallow marker, leaving a working copy with a truncated object
// graph that looks corrupt.
func (g *Git) Clone(ctx context.Context, src, dst string, noLocal bool, sink LineSink) error {
	args := []string{"clone", "--progress"}
	if noLocal {
		args = append(args, "--no-local", LocalFileURL(src))
	} else {
		args = append(args, src)
	}
	args = append(args, dst)
	return g.run(ctx, "", sink, args...)
}

// CloneNoHardlinks is the fallback when --no-local is refused: it copies objects
// instead of hardlinking them, and the caller then carries the shallow marker
// across by hand.
func (g *Git) CloneNoHardlinks(ctx context.Context, src, dst string, sink LineSink) error {
	return g.run(ctx, "", sink, "clone", "--progress", "--no-hardlinks", src, dst)
}

// LocalFileURL renders a local path as a file:// URL: absolute, forward slashes,
// and a leading slash before a Windows drive letter.
func LocalFileURL(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	slashed := filepath.ToSlash(abs)
	if !strings.HasPrefix(slashed, "/") {
		slashed = "/" + slashed
	}
	return "file://" + slashed
}

// RefNames lists the branch and tag refs of a repository. These are the tips whose
// LFS objects the backup captures.
func (g *Git) RefNames(ctx context.Context, dir string) ([]string, error) {
	var refs []string
	sink := func(stream Stream, line string) {
		if stream == StdOut && line != "" {
			refs = append(refs, line)
		}
	}
	// The script wrote --format='%(refname)', but PowerShell strips those quotes
	// before git sees them, so the format is passed unquoted here.
	err := g.run(ctx, dir, sink, "for-each-ref", "--format=%(refname)", "refs/heads", "refs/tags")
	if err != nil {
		return nil, err
	}
	return refs, nil
}

// LFSFetch downloads the LFS objects referenced by the given refs.
//
// Only the named tips are fetched, never --all: fetching all of LFS history would
// pull every past revision of every large file and defeat the point of the design,
// which is an archive that restores the current content offline while staying a
// sensible size.
//
// Refs go out in batches, because one command line cannot hold hundreds of them.
func (g *Git) LFSFetch(ctx context.Context, dir string, refs []string, sink LineSink) error {
	if len(refs) == 0 {
		return nil
	}
	var errs []error
	for _, batch := range chunkRefs(refs, maxRefArgsLen) {
		args := append([]string{"lfs", "fetch", "origin"}, batch...)
		if err := g.run(ctx, dir, sink, args...); err != nil {
			if ctx.Err() != nil {
				return err
			}
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// chunkRefs splits refs into batches whose joined length stays under limit. A
// single ref longer than the limit still gets its own batch: dropping it silently
// would be worse than a command line git may reject.
func chunkRefs(refs []string, limit int) [][]string {
	var (
		batches [][]string
		current []string
		length  int
	)
	for _, ref := range refs {
		cost := len(ref) + 1
		if len(current) > 0 && length+cost > limit {
			batches = append(batches, current)
			current, length = nil, 0
		}
		current = append(current, ref)
		length += cost
	}
	if len(current) > 0 {
		batches = append(batches, current)
	}
	return batches
}

// LFSInstallLocal enables the LFS filters for one repository only, leaving the
// user's global configuration alone.
func (g *Git) LFSInstallLocal(ctx context.Context, dir string) error {
	return g.run(ctx, dir, nil, "lfs", "install", "--local")
}

// LFSCheckout replaces LFS pointer files in the working tree with their content,
// using the object cache seeded from the archive.
func (g *Git) LFSCheckout(ctx context.Context, dir string, sink LineSink) error {
	return g.run(ctx, dir, sink, "lfs", "checkout")
}

// SetRemoteURL repoints an existing remote. Used to strip credentials from the
// archived config, and to restore origin to the real Azure DevOps URL.
func (g *Git) SetRemoteURL(ctx context.Context, dir, remote, url string) error {
	return g.run(ctx, dir, nil, "remote", "set-url", remote, url)
}

// AddRemote adds a remote, so a restored clone can keep a pointer to the bare it
// came from as well as to its origin.
func (g *Git) AddRemote(ctx context.Context, dir, name, url string) error {
	return g.run(ctx, dir, nil, "remote", "add", name, url)
}

// Fsck verifies that the object graph is complete. Connectivity only, because a
// full check would read every byte of every pack for little extra assurance.
func (g *Git) Fsck(ctx context.Context, dir string, sink LineSink) error {
	return g.run(ctx, dir, sink, "fsck", "--connectivity-only")
}

// StatusPorcelain returns the machine-readable status lines of a working tree. A
// fresh clone should report nothing; anything else usually means a line-ending or
// .gitattributes surprise worth reporting rather than silently correcting.
func (g *Git) StatusPorcelain(ctx context.Context, dir string) ([]string, error) {
	var lines []string
	sink := func(stream Stream, line string) {
		if stream == StdOut && line != "" {
			lines = append(lines, line)
		}
	}
	if err := g.run(ctx, dir, sink, "status", "--porcelain"); err != nil {
		return nil, err
	}
	return lines, nil
}

// LSRemote asks a remote for its branches without cloning anything. This is the
// authentication probe: it turns "300 repositories failing over two hours" into
// one failure in a few seconds.
func (g *Git) LSRemote(ctx context.Context, url string) error {
	return g.run(ctx, "", nil, "ls-remote", "--heads", url)
}

// Version returns the git version string, for example "2.55.0.windows.3".
func (g *Git) Version(ctx context.Context) (string, error) {
	out, err := g.capture(ctx, "version")
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(out, "git version "), nil
}

// LFSVersion returns the git-lfs version, for example "3.7.1".
//
// It asks through git rather than looking for a git-lfs binary, because that is
// what validates the wiring: an installed binary git cannot invoke is useless.
func (g *Git) LFSVersion(ctx context.Context) (string, error) {
	out, err := g.capture(ctx, "lfs", "version")
	if err != nil {
		return "", err
	}
	// Reported as "git-lfs/3.7.1 (GitHub; windows amd64; go 1.25.1; git b84b338)".
	version := strings.TrimPrefix(out, "git-lfs/")
	if i := strings.IndexAny(version, " ("); i > 0 {
		version = version[:i]
	}
	return version, nil
}

// ConfigGet reads one configuration value. A missing key is not an error: git
// exits 1, and the empty result is the answer.
func (g *Git) ConfigGet(ctx context.Context, key string) (string, error) {
	out, err := g.capture(ctx, "config", "--get", key)
	if err != nil {
		var exitErr *ExitError
		if errors.As(err, &exitErr) && exitErr.Code == 1 {
			return "", nil
		}
		return "", err
	}
	return out, nil
}

// ConfigGetAll reads every value of a multi-valued key, such as credential.helper.
func (g *Git) ConfigGetAll(ctx context.Context, key string) ([]string, error) {
	var lines []string
	sink := func(stream Stream, line string) {
		if stream == StdOut && line != "" {
			lines = append(lines, line)
		}
	}
	if err := g.run(ctx, "", sink, "config", "--get-all", key); err != nil {
		var exitErr *ExitError
		if errors.As(err, &exitErr) && exitErr.Code == 1 {
			return nil, nil
		}
		return nil, err
	}
	return lines, nil
}

// capture runs a command and returns its first line of standard output.
func (g *Git) capture(ctx context.Context, args ...string) (string, error) {
	var first string
	found := false
	sink := func(stream Stream, line string) {
		if stream == StdOut && !found && line != "" {
			first, found = line, true
		}
	}
	if err := g.run(ctx, "", sink, args...); err != nil {
		return "", err
	}
	return strings.TrimSpace(first), nil
}

// IsShallow reports whether a repository has truncated history.
//
// Read from disk rather than asked of git, because it has to be answered about a
// freshly extracted archive before anything is cloned from it. git writes this
// file itself, so no marker of our own is needed.
func IsShallow(gitDir string) bool {
	info, err := os.Stat(filepath.Join(gitDir, "shallow"))
	return err == nil && !info.IsDir()
}

// HeadRef returns the symbolic ref HEAD points at, for example "refs/heads/main".
// Reading the file directly works on a bare repository that has just been
// extracted and may be shallow.
func HeadRef(gitDir string) string {
	data, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(data))
	if ref, ok := strings.CutPrefix(line, "ref:"); ok {
		return strings.TrimSpace(ref)
	}
	// A detached HEAD holds a bare object id, which is not a ref name.
	return ""
}
