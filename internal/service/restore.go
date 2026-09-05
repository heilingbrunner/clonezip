package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/heilingbrunner/clonezip/internal/common/doctorx"
	"github.com/heilingbrunner/clonezip/internal/common/gitx"
	"github.com/heilingbrunner/clonezip/internal/common/pipeline"
)

// Errors TriggerRestore can return.
var (
	ErrRestoreAlreadyRunning = errors.New("a restore is already in progress")
	ErrNoArchive             = errors.New("no archive for that repository")
)

// RestoreRequest is one restore asked for through the dashboard. The archive
// itself is not named: the newest one belonging to Project/Repo within the
// group is what gets restored.
type RestoreRequest struct {
	Group   string
	Project string
	Repo    string

	// Dest is an absolute directory to restore into. Unlike a group's out
	// path it is deliberately not confined to the backup root - a restore's
	// whole point is to put a repository back somewhere else.
	Dest string

	Origin         string
	Clone          bool
	Force          bool
	ConfirmShallow bool
	NoVerify       bool
	SkipChecks     bool
}

// RestoreStatus is a snapshot of the most recent restore, in progress or
// finished. Only the latest one is tracked, and only in memory - the same
// deal as BatchStatus.
type RestoreStatus struct {
	Running bool

	Group   string
	Project string
	Repo    string
	Archive string
	Dest    string

	Started  time.Time
	Finished time.Time

	Phase   string
	Pct     int
	LogTail []LiveLogLine

	// Result is set once the restore finishes successfully.
	Result *RestoreOutcome

	// Err is the failure message, empty on success.
	Err string
}

// RestoreOutcome is what a finished restore produced.
type RestoreOutcome struct {
	BarePath  string
	WorkPath  string
	Cloned    bool
	Shallow   bool
	Refs      int
	LFSBytes  int64
	OriginURL string
	Warnings  []string
}

// RestoreRunner runs one restore at a time and keeps its progress readable
// while it runs, so the dashboard can poll it the way it polls a backup.
type RestoreRunner struct {
	git *gitx.Git

	// rootCtx is the service's context, so a shutdown cancels a restore in
	// flight rather than the browser request that started it doing so.
	rootCtx context.Context

	mu     sync.Mutex
	status RestoreStatus
	wg     sync.WaitGroup
}

// NewRestoreRunner builds a runner using git for verification and cloning.
func NewRestoreRunner(git *gitx.Git) *RestoreRunner {
	return &RestoreRunner{git: git, rootCtx: context.Background()}
}

// Start binds the runner to the service's context.
func (r *RestoreRunner) Start(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rootCtx = ctx
}

// Wait blocks until any restore in flight has finished.
func (r *RestoreRunner) Wait() { r.wg.Wait() }

// Status returns a snapshot of the latest restore.
func (r *RestoreRunner) Status() RestoreStatus {
	r.mu.Lock()
	defer r.mu.Unlock()

	s := r.status
	s.LogTail = append([]LiveLogLine(nil), r.status.LogTail...)
	if r.status.Result != nil {
		out := *r.status.Result
		out.Warnings = append([]string(nil), r.status.Result.Warnings...)
		s.Result = &out
	}
	return s
}

// Trigger starts restoring archive in the background and returns as soon as
// it is under way, reporting ErrRestoreAlreadyRunning if one already is.
func (r *RestoreRunner) Trigger(req RestoreRequest, archive ArchiveInfo) error {
	r.mu.Lock()
	if r.status.Running {
		r.mu.Unlock()
		return ErrRestoreAlreadyRunning
	}
	r.status = RestoreStatus{
		Running: true,
		Group:   req.Group,
		Project: archive.Project,
		Repo:    archive.Repo,
		Archive: archive.Path,
		Dest:    filepath.Clean(req.Dest),
		Started: time.Now(),
		Phase:   "starting",
	}
	ctx := r.rootCtx
	r.mu.Unlock()

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.run(ctx, req, archive)
	}()
	return nil
}

func (r *RestoreRunner) run(ctx context.Context, req RestoreRequest, archive ArchiveInfo) {
	defer r.finish()

	if !req.SkipChecks {
		// Restoring needs the LFS filters installed in a way backing up does
		// not: without them a checkout silently leaves pointer files behind.
		var checkOut bytes.Buffer
		if err := doctorx.Check(ctx, &checkOut,
			doctorx.Options{Git: r.git, Out: req.Dest}, doctorx.ChecksRestore); err != nil {
			msg := strings.TrimSpace(checkOut.String())
			if msg == "" {
				msg = err.Error()
			}
			r.fail(msg)
			return
		}
	}

	r.setPhase("restore", 0)
	result, err := pipeline.RunRestore(ctx, pipeline.RestoreOptions{
		ArchivePath:    archive.Path,
		Dest:           req.Dest,
		Origin:         req.Origin,
		Clone:          req.Clone,
		Force:          req.Force,
		ConfirmShallow: req.ConfirmShallow,
		Verify:         !req.NoVerify,
		Git:            r.git,
	}, &restoreReporter{runner: r})
	if err != nil {
		r.fail(dashboardMessage(err))
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.status.Phase = "done"
	r.status.Pct = 100
	r.status.Result = &RestoreOutcome{
		BarePath:  result.BarePath,
		WorkPath:  result.WorkPath,
		Cloned:    result.Cloned,
		Shallow:   result.Shallow,
		Refs:      result.Refs,
		LFSBytes:  result.LFSBytes,
		OriginURL: result.OriginURL,
		Warnings:  append([]string(nil), result.Warnings...),
	}
}

// dashboardMessage rewords the precondition failures whose text names a CLI
// flag, since the dashboard offers a checkbox instead and telling the user to
// pass --confirm-shallow leaves them looking for something that isn't there.
func dashboardMessage(err error) string {
	switch {
	case errors.Is(err, pipeline.ErrShallowNeedsConfirm):
		return "This archive has truncated history. Tick \"Confirm truncated/shallow history\" to restore it anyway."
	case errors.Is(err, pipeline.ErrTargetExists):
		return "The target already exists. Tick \"Force overwrite of an existing target\", or pick an empty destination directory."
	default:
		return err.Error()
	}
}

func (r *RestoreRunner) finish() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status.Running = false
	r.status.Finished = time.Now()
}

func (r *RestoreRunner) fail(msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status.Phase = "failed"
	r.status.Err = msg
}

func (r *RestoreRunner) setPhase(phase string, pct int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status.Phase = phase
	r.status.Pct = pct
}

func (r *RestoreRunner) appendLog(level, line string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.status.LogTail = append(r.status.LogTail, LiveLogLine{Level: level, Line: line})
	if len(r.status.LogTail) > logTailLimit {
		r.status.LogTail = r.status.LogTail[len(r.status.LogTail)-logTailLimit:]
	}
}

// restoreReporter is a pipeline.Reporter for a single-archive restore: there
// is only ever one "repo", so the per-repo index every event carries is
// ignored and everything folds into one status.
type restoreReporter struct {
	runner *RestoreRunner
}

// Report implements pipeline.Reporter.
func (rr *restoreReporter) Report(event pipeline.Event) {
	switch e := event.(type) {
	case pipeline.RepoPhase:
		phase := e.Phase.String()
		if e.Detail != "" {
			phase = fmt.Sprintf("%s (%s)", phase, e.Detail)
		}
		rr.runner.setPhase(phase, 0)

	case pipeline.RepoPercent:
		rr.runner.mu.Lock()
		rr.runner.status.Pct = e.Pct
		rr.runner.mu.Unlock()

	case pipeline.RepoLog:
		rr.runner.appendLog(levelString(e.Level), e.Line)
	}
}
