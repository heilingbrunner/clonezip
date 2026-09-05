package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/heilingbrunner/clonezip/internal/common/fsx"
	"github.com/heilingbrunner/clonezip/internal/common/gitx"
	"github.com/heilingbrunner/clonezip/internal/common/layout"
	"github.com/heilingbrunner/clonezip/internal/common/manifest"
	"github.com/heilingbrunner/clonezip/internal/common/repolist"
	"github.com/heilingbrunner/clonezip/internal/common/ziparc"
)

// Archiver is the archive writer the pipeline uses. It is an interface so tests can
// run the whole sequence without producing real archives.
type Archiver interface {
	CreateFromDir(ctx context.Context, srcDir, rootName, dest string,
		extra map[string][]byte, progress ziparc.Progress) (int64, error)
}

// ZipArchiver writes real archives.
type ZipArchiver struct{}

func (ZipArchiver) CreateFromDir(ctx context.Context, srcDir, rootName, dest string,
	extra map[string][]byte, progress ziparc.Progress,
) (int64, error) {
	return ziparc.CreateFromDir(ctx, srcDir, rootName, dest, extra, progress)
}

// BackupOptions configures a backup run.
type BackupOptions struct {
	Plan layout.Plan
	List repolist.List

	Git *gitx.Git
	Arc Archiver

	Depth   int
	Jobs    int
	Retries int
	Timeout time.Duration

	FailFast bool

	ToolVersion string

	// GitVersion and LFSVersion are recorded in every manifest. They are resolved
	// once per run rather than per repository, since they cannot change mid-run.
	GitVersion string
	LFSVersion string

	// Now is injected so tests are deterministic.
	Now func() time.Time
}

func (o *BackupOptions) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// job is one unit of work, carrying the list index that makes its staging
// directory unique.
type job struct {
	Index int
	Entry repolist.Entry
}

// RunBackup clones every repository in the list and archives each one.
//
// A repository that fails does not stop the run. That is the central difference
// from the script this replaces, which never checked git's exit status and so
// archived failed clones as though they had worked - and, having no summary, gave no
// way to find out afterwards.
func RunBackup(ctx context.Context, o BackupOptions, rep Reporter) (Summary, error) {
	started := o.now()
	summary := Summary{
		Out:     o.Plan.Out,
		Log:     o.Plan.LogPath(),
		Started: started,
		Issues:  o.List.Issues,
	}

	if o.Arc == nil {
		o.Arc = ZipArchiver{}
	}
	if o.Jobs < 1 {
		o.Jobs = 1
	}

	if err := os.MkdirAll(o.Plan.Out, 0o755); err != nil {
		return summary, fmt.Errorf("create run directory: %w", err)
	}
	logger, closeLog, err := openLog(o.Plan.LogPath())
	if err != nil {
		return summary, err
	}
	defer closeLog()

	logger.Info("run started",
		slog.Int("repositories", len(o.List.Entries)),
		slog.String("out", o.Plan.Out),
		slog.Int("jobs", o.Jobs),
		slog.Int("depth", o.Depth),
	)
	rep.Report(RunStarted{
		Total: len(o.List.Entries),
		Out:   o.Plan.Out,
		Log:   o.Plan.LogPath(),
		Jobs:  o.Jobs,
		Depth: o.Depth,
	})

	// Results come back in list order regardless of the order they finished in, so
	// the summary reads the same whatever --jobs was set to.
	summary.Results = runJobs(ctx, &o, rep, logger)
	summary.Elapsed = o.now().Sub(started)

	summary.StuckDirs = fsx.EmptyTrash(o.Plan.TrashDir())
	// The staging root is only litter once every worker has finished with it.
	if err := fsx.RemoveAllRetry(o.Plan.Stage, ""); err != nil {
		summary.StuckDirs++
		logger.Warn("staging directory could not be removed", slog.String("error", err.Error()))
	}

	logger.Info("run finished",
		slog.Int("ok", summary.Count(StatusOK)),
		slog.Int("warn", summary.Count(StatusWarn)),
		slog.Int("failed", summary.Count(StatusFailed)),
		slog.Int64("bytes", summary.TotalBytes()),
		slog.String("elapsed", summary.Elapsed.String()),
	)
	rep.Report(RunDone{Summary: summary})

	if err := ctx.Err(); err != nil {
		return summary, err
	}
	return summary, nil
}

// runJobs feeds the entries through a bounded worker pool and collects results in
// list order.
func runJobs(ctx context.Context, o *BackupOptions, rep Reporter, logger *slog.Logger) []Result {
	results := make([]Result, len(o.List.Entries))

	jobs := make(chan job)
	var wg sync.WaitGroup

	// A separate cancel for --fail-fast, so stopping early stays distinguishable
	// from a Ctrl-C on the parent context.
	runCtx, stop := context.WithCancel(ctx)
	defer stop()

	for range o.Jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				result := runOne(runCtx, o, rep, logger, j)
				results[j.Index] = result
				rep.Report(RepoDone{Result: result})

				if o.FailFast && result.Status == StatusFailed {
					stop()
					return
				}
			}
		}()
	}

feed:
	for i, entry := range o.List.Entries {
		select {
		case jobs <- job{Index: i, Entry: entry}:
		case <-runCtx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()

	// Entries never reached keep their zero value; mark them so the summary counts
	// add up to the size of the list.
	for i, entry := range o.List.Entries {
		if results[i].Repo == "" {
			results[i] = Result{
				Index:   i,
				Project: entry.Project,
				Repo:    entry.Repo,
				URL:     entry.Source(),
				Status:  StatusSkipped,
			}
		}
	}
	return results
}

// runOne processes a single repository, retrying a transient failure.
func runOne(ctx context.Context, o *BackupOptions, rep Reporter, logger *slog.Logger, j job) Result {
	entry := j.Entry
	rep.Report(RepoStarted{
		Index: j.Index, Project: entry.Project, Repo: entry.Repo, URL: entry.Display(),
	})

	attempts := o.Retries + 1
	var result Result

	for attempt := 1; attempt <= attempts; attempt++ {
		result = cloneOne(ctx, o, rep, logger, j)

		if result.Status != StatusFailed || ctx.Err() != nil {
			break
		}
		// Only a transport-shaped failure is worth repeating. Retrying an
		// authentication failure just multiplies the wait before the same answer.
		if attempt == attempts || !isTransient(result.Err) {
			break
		}

		wait := retryBackoff(attempt)
		rep.Report(RepoRetry{
			Index: j.Index, Attempt: attempt, Of: attempts,
			Reason: shortReason(result.Err), Wait: wait,
		})
		logger.Warn("retrying",
			slog.String("repo", entry.Slug()),
			slog.Int("attempt", attempt),
			slog.String("reason", shortReason(result.Err)),
		)
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return result
		}
	}

	logResult(logger, result)
	if result.Status == StatusFailed {
		writeFailureLog(o.Plan, result)
	}
	return result
}

// cloneOne is one attempt at one repository: clone, fetch LFS, archive, clean up.
//
// The result is a named return on purpose. The deferred cleanup below can add a
// warning, and with an unnamed return that append would land on a copy the caller never
// sees - silently losing the one signal that a staging directory was left behind.
func cloneOne(ctx context.Context, o *BackupOptions, rep Reporter, logger *slog.Logger, j job) (result Result) {
	entry := j.Entry
	started := o.now()

	result = Result{
		Index:   j.Index,
		Project: entry.Project,
		Repo:    entry.Repo,
		URL:     entry.Source(),
		Shallow: o.Depth > 0,
	}
	finish := func() Result {
		result.Dur = o.now().Sub(started)
		return result
	}
	fail := func(phase Phase, err error) Result {
		result.Status = StatusFailed
		result.Err = fmt.Errorf("%s: %w", phase, err)
		result.Reason = fmt.Sprintf("%s: %s", phase, shortReason(err))
		return finish()
	}

	// A per-repository deadline, so one unresponsive remote cannot stall a run of
	// three hundred.
	if o.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.Timeout)
		defer cancel()
	}

	stage := o.Plan.StageDir(j.Index, entry.Project, entry.Repo)
	sink := newSink(rep, logger, j.Index, entry.Slug())

	// Always leave the staging directory behind us, on success or failure.
	defer func() {
		rep.Report(RepoPhase{Index: j.Index, Phase: PhaseCleanup})
		if err := fsx.RemoveAllRetry(stage, o.Plan.TrashDir()); err != nil {
			// Litter, not a failure: abandoning completed work over an undeletable
			// directory would be far worse.
			result.Warnings = append(result.Warnings, "staging directory left behind: "+err.Error())
			logger.Warn("staging directory left behind",
				slog.String("repo", entry.Slug()), slog.String("error", err.Error()))
		}
	}()

	if err := fsx.RemoveAllRetry(stage, o.Plan.TrashDir()); err != nil {
		return fail(PhaseCleanup, err)
	}
	if err := os.MkdirAll(filepath.Dir(stage), 0o755); err != nil {
		return fail(PhaseCleanup, err)
	}

	// Clone.
	rep.Report(RepoPhase{Index: j.Index, Phase: PhaseClone})
	cloneStart := o.now()
	if err := o.Git.CloneBare(ctx, entry.CloneURL(), stage, o.Depth, sink); err != nil {
		return fail(PhaseClone, err)
	}
	cloneMs := o.now().Sub(cloneStart).Milliseconds()

	// Strip any credentials the list carried, so no token is baked into the
	// archived config where it would travel with the archive.
	if entry.CloneURL() != entry.Source() {
		if err := o.Git.SetRemoteURL(ctx, stage, "origin", entry.Source()); err != nil {
			result.Warnings = append(result.Warnings, "could not rewrite origin: "+err.Error())
		}
	}

	// Refs.
	rep.Report(RepoPhase{Index: j.Index, Phase: PhaseRefs})
	refs, err := o.Git.RefNames(ctx, stage)
	if err != nil {
		return fail(PhaseRefs, err)
	}
	result.Refs = len(refs)
	// An unused Azure DevOps repository clones fine and has no refs at all. That is
	// a success, and there is nothing for LFS to fetch.
	result.Empty = len(refs) == 0

	// LFS, for the tips only.
	lfsInfo := manifest.LFS{Scope: manifest.LFSScopeTips}
	var lfsMs int64
	if len(refs) > 0 {
		rep.Report(RepoPhase{
			Index: j.Index, Phase: PhaseLFS,
			Detail: strconv.Itoa(len(refs)) + " refs",
		})
		lfsStart := o.now()
		lfsInfo.Attempted = true

		if err := o.Git.LFSFetch(ctx, stage, refs, sink); err != nil {
			if ctx.Err() != nil {
				return fail(PhaseLFS, err)
			}
			// Not fatal, and deliberately so: the archive is still a complete git
			// repository, only LFS-tracked content would be missing. Recording it
			// is what stops that being a silent surprise years later.
			lfsInfo.Warning = shortReason(err)
			result.Warnings = append(result.Warnings, "LFS fetch failed: "+shortReason(err))
			rep.Report(RepoLog{
				Index: j.Index, Level: LevelWarn,
				Line: "LFS fetch failed, archiving without LFS payload: " + shortReason(err),
			})
		} else {
			lfsInfo.OK = true
		}
		lfsMs = o.now().Sub(lfsStart).Milliseconds()

		if bytes, err := fsx.DirSize(filepath.Join(stage, "lfs", "objects")); err == nil {
			lfsInfo.TotalBytes = bytes
			result.LFSBytes = bytes
		}
	}

	// Archive.
	rep.Report(RepoPhase{Index: j.Index, Phase: PhaseArchive})
	archivePath := o.Plan.ArchivePath(entry.Project, entry.Repo)
	bareDir := entry.Repo + layout.BareSuffix

	man := o.buildManifest(entry, refs, bareDir, stage, lfsInfo, cloneMs, lfsMs)
	manJSON, err := man.JSON()
	if err != nil {
		return fail(PhaseArchive, err)
	}

	size, err := o.Arc.CreateFromDir(ctx, stage, bareDir, archivePath,
		map[string][]byte{manifest.EntryName: manJSON},
		func(done, total int64) {
			rep.Report(RepoBytes{Index: j.Index, Done: done, Total: total})
		})
	if err != nil {
		return fail(PhaseArchive, err)
	}

	result.Archive = archivePath
	result.Bytes = size
	if len(result.Warnings) > 0 {
		result.Status = StatusWarn
	} else {
		result.Status = StatusOK
	}
	return finish()
}

// buildManifest records what this archive is and where it came from.
//
// The zip phase is not timed here, because the manifest has to be serialised before
// it can be written into the archive it describes.
func (o *BackupOptions) buildManifest(
	entry repolist.Entry,
	refs []string,
	bareDir, stage string,
	lfs manifest.LFS,
	cloneMs, lfsMs int64,
) *manifest.Manifest {
	man := manifest.New(o.ToolVersion, o.now())
	man.Environment.Git = o.GitVersion
	man.Environment.GitLFS = o.LFSVersion

	man.Source = manifest.Source{
		URL:                 entry.Source(),
		CredentialsRedacted: entry.CloneURL() != entry.Source(),
		Host:                entry.URL.Hostname(),
		Organization:        entry.Org,
		Project:             entry.Project,
		Repo:                entry.Repo,
		Provider:            entry.Provider,
		HeadRef:             gitx.HeadRef(stage),
		ListFile:            filepath.Base(o.Plan.ListPath),
		ListLine:            entry.LineNo,
	}
	man.Clone = manifest.Clone{
		Bare:    true,
		Depth:   o.Depth,
		Shallow: gitx.IsShallow(stage),
	}
	man.Archive = manifest.Archive{BareDir: bareDir}
	if size, err := fsx.DirSize(stage); err == nil {
		man.Archive.UncompressedBytes = size
	}
	for _, ref := range refs {
		man.Refs = append(man.Refs, manifest.Ref{Name: ref})
	}
	man.LFS = lfs
	man.Timings = manifest.Timings{CloneMs: cloneMs, LFSMs: lfsMs}
	return man
}

// progressPattern matches the progress lines git writes with a carriage return.
//
// The "remote:" prefix is optional because the server reports its own counting and
// compressing that way; without allowing it, every one of those lines would be
// mistaken for a message and printed instead of driving the progress display.
var progressPattern = regexp.MustCompile(
	`^(?:remote: )?(Counting|Compressing|Receiving|Resolving|Enumerating) (?:objects|deltas):\s+(\d+)%`)

// newSink turns child output into events and log records. Nothing here writes to
// stdout: that is what keeps the rendered view intact.
func newSink(rep Reporter, logger *slog.Logger, index int, slug string) gitx.LineSink {
	return func(_ gitx.Stream, line string) {
		if m := progressPattern.FindStringSubmatch(line); m != nil {
			if pct, err := strconv.Atoi(m[2]); err == nil {
				rep.Report(RepoPercent{Index: index, Pct: pct, Label: strings.ToLower(m[1])})
				return
			}
		}

		// "remote:" is not a warning. The server prefixes its ordinary progress
		// chatter that way, and treating it as noteworthy would print a dozen lines
		// per repository - hundreds across a run - for repositories that succeeded.
		// A genuine remote complaint arrives either as "fatal:"/"error:" or in the
		// stderr tail carried by the failure itself.
		level := LevelInfo
		switch {
		case strings.HasPrefix(line, "fatal:"), strings.HasPrefix(line, "error:"):
			level = LevelError
		case strings.HasPrefix(line, "warning:"):
			level = LevelWarn
		}

		rep.Report(RepoLog{Index: index, Level: level, Line: line})
		if level != LevelInfo {
			logger.Warn("git", slog.String("repo", slug), slog.String("line", line))
		}
	}
}

// Retry policy. Only a failure that looks like transport trouble is repeated;
// anything about credentials or a missing repository will answer the same way.
var (
	transientSignals = []string{
		"429", "503", "502", "504",
		"rpc failed", "connection", "early eof", "the remote end hung up",
		"timed out", "timeout", "could not resolve host", "operation was aborted",
		"unexpected disconnect", "gnutls_handshake", "ssl_read",
	}
	permanentSignals = []string{
		"authentication failed", "tf401019", "tf401027",
		"repository not found", "does not exist", "403", "401",
		"permission denied", "invalid username or password",
		"could not read username", "terminal prompts disabled",
		"support for password authentication",
	}
)

func isTransient(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())

	var exitErr *gitx.ExitError
	if errors.As(err, &exitErr) {
		msg += " " + strings.ToLower(strings.Join(exitErr.Tail, " "))
	}

	// A permanent signal wins: an authentication failure often also mentions the
	// connection, and retrying that wastes minutes for nothing.
	for _, signal := range permanentSignals {
		if strings.Contains(msg, signal) {
			return false
		}
	}
	for _, signal := range transientSignals {
		if strings.Contains(msg, signal) {
			return true
		}
	}
	return false
}

func retryBackoff(attempt int) time.Duration {
	switch attempt {
	case 1:
		return 2 * time.Second
	case 2:
		return 8 * time.Second
	default:
		return 20 * time.Second
	}
}

// shortReason renders an error as one line, preferring the remote's own message.
func shortReason(err error) string {
	if err == nil {
		return ""
	}
	var exitErr *gitx.ExitError
	if errors.As(err, &exitErr) {
		if reason := exitErr.Reason(); reason != "" {
			return fmt.Sprintf("exit %d: %s", exitErr.Code, reason)
		}
		return fmt.Sprintf("exit %d", exitErr.Code)
	}
	return err.Error()
}

// openLog creates the run log: one JSON object per line, so a failure two hours into
// a run can still be diagnosed afterwards.
func openLog(path string) (*slog.Logger, func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, nil, fmt.Errorf("create run log: %w", err)
	}
	logger := slog.New(slog.NewJSONHandler(f, &slog.HandlerOptions{Level: slog.LevelInfo}))
	return logger, func() { _ = f.Close() }, nil
}

func logResult(logger *slog.Logger, r Result) {
	attrs := []any{
		slog.String("repo", r.Slug()),
		slog.String("status", r.Status.String()),
		slog.Int64("ms", r.Dur.Milliseconds()),
		slog.Int64("bytes", r.Bytes),
		slog.Int("refs", r.Refs),
	}
	if r.Err != nil {
		attrs = append(attrs, slog.String("error", r.Err.Error()))
	}
	if len(r.Warnings) > 0 {
		attrs = append(attrs, slog.String("warnings", strings.Join(r.Warnings, "; ")))
	}
	if r.Status == StatusFailed {
		logger.Error("repository failed", attrs...)
		return
	}
	logger.Info("repository done", attrs...)
}

// writeFailureLog keeps the captured git output for a failed repository, so the full
// context survives beyond the one line the summary can show.
func writeFailureLog(plan layout.Plan, r Result) {
	var exitErr *gitx.ExitError
	if !errors.As(r.Err, &exitErr) || len(exitErr.Tail) == 0 {
		return
	}
	if err := os.MkdirAll(plan.FailuresDir(), 0o755); err != nil {
		return
	}
	body := fmt.Sprintf("%s\ngit %s\nexit %d\n\n%s\n",
		r.Slug(), strings.Join(exitErr.Args, " "), exitErr.Code,
		strings.Join(exitErr.Tail, "\n"))
	_ = os.WriteFile(plan.FailureLogPath(r.Project, r.Repo), []byte(body), 0o644)
}
