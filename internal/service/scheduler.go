package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/heilingbrunner/clonezip/internal/common/doctorx"
	"github.com/heilingbrunner/clonezip/internal/common/fsx"
	"github.com/heilingbrunner/clonezip/internal/common/gitx"
	"github.com/heilingbrunner/clonezip/internal/common/layout"
	"github.com/heilingbrunner/clonezip/internal/common/pipeline"
	"github.com/heilingbrunner/clonezip/internal/common/repolist"
)

// Errors TriggerNow and TriggerAllNow can return.
var (
	ErrUnknownGroup        = errors.New("unknown group")
	ErrAlreadyRunning      = errors.New("group already running")
	ErrBatchAlreadyRunning = errors.New("run-all already in progress")
)

// BatchResult records one group's outcome within a "run all" batch.
type BatchResult struct {
	Name   string
	Status string // "ok", "failed", "skipped"
	Reason string
}

// BatchStatus is a snapshot of the most recent "run all" batch, whether
// still in progress or finished. Only the latest batch is ever tracked - a
// new TriggerAllNow overwrites it. It lives in memory only: a closed browser
// tab does not affect it, since the batch keeps running server-side, but a
// service restart does lose it, the same as a run in progress does.
type BatchStatus struct {
	Running  bool
	Names    []string
	Current  string
	Started  time.Time
	Finished time.Time
	Results  []BatchResult
}

// Scheduler drives every enabled group's cron schedule and lets one be
// triggered on demand, outside its schedule.
type Scheduler struct {
	store   *Store
	git     *gitx.Git
	version string

	configDir string

	// mu guards groups, order and entryIDs below, since AddOrUpdateGroup and
	// RemoveGroup can now be called from an HTTP handler goroutine at the same
	// time cron's own goroutine is reading them to fire a group. It does not
	// need to protect a run in progress: fire/runGroup take a ResolvedGroup by
	// value, so an in-flight run already holds its own snapshot, independent
	// of whatever s.groups[name] changes to afterwards.
	mu       sync.RWMutex
	groups   map[string]ResolvedGroup
	order    []string
	cron     *cron.Cron
	entryIDs map[string]cron.EntryID

	// rootCtx is the service's own long-lived context, captured in Start. A
	// manually triggered run must use it rather than the HTTP request's
	// context: the request context is cancelled the instant the handler
	// returns, which happens immediately after a trigger is accepted.
	rootCtx context.Context

	wg sync.WaitGroup

	// batchMu guards batch, the most recent "run all" batch's status. It is
	// separate from mu since runBatch holds it only briefly to update
	// progress, never across a blocking runGroup call.
	batchMu sync.Mutex
	batch   BatchStatus
}

// NewScheduler builds a Scheduler from a validated config. version is
// recorded in every archive's manifest, the same as the backup command's own
// --version.
func NewScheduler(cfg Config, store *Store, git *gitx.Git, version string) *Scheduler {
	resolved := cfg.ResolvedGroups()

	s := &Scheduler{
		store:     store,
		git:       git,
		version:   version,
		configDir: cfg.Dir,
		groups:    make(map[string]ResolvedGroup, len(resolved)),
		entryIDs:  make(map[string]cron.EntryID, len(resolved)),
		cron:      cron.New(),
	}
	for _, g := range resolved {
		s.groups[g.Name] = g
		s.order = append(s.order, g.Name)
	}
	return s
}

// Start registers every enabled group with the cron scheduler and starts it.
// ctx is the parent context for every run this scheduler launches, whether
// cron-fired or manually triggered; cancelling it (Ctrl-C) cancels any run in
// progress.
func (s *Scheduler) Start(ctx context.Context) error {
	s.mu.Lock()
	s.rootCtx = ctx
	for _, name := range s.order {
		g := s.groups[name]
		if !g.Enabled {
			continue
		}
		id, err := s.cron.AddFunc(g.Schedule, func() { s.fire(ctx, g) })
		if err != nil {
			s.mu.Unlock()
			return fmt.Errorf("group %q: schedule: %w", g.Name, err)
		}
		s.entryIDs[g.Name] = id
	}
	s.mu.Unlock()

	s.cron.Start()
	s.refreshNextRuns()
	return nil
}

// AddOrUpdateGroup registers a new group or replaces an existing one's
// definition, taking effect immediately: any existing cron entry for the
// name is removed first, then a fresh one is added from g's current
// schedule/enabled state. A run already in progress under the old
// definition is unaffected - it holds its own ResolvedGroup snapshot, taken
// when it started.
func (s *Scheduler) AddOrUpdateGroup(g ResolvedGroup) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if id, ok := s.entryIDs[g.Name]; ok {
		s.cron.Remove(id)
		delete(s.entryIDs, g.Name)
	}
	if _, existed := s.groups[g.Name]; !existed {
		s.order = append(s.order, g.Name)
	}
	s.groups[g.Name] = g

	if !g.Enabled || s.rootCtx == nil {
		s.store.SetNextRun(g.Name, time.Time{})
		return nil
	}

	id, err := s.cron.AddFunc(g.Schedule, func() { s.fire(s.rootCtx, g) })
	if err != nil {
		return fmt.Errorf("group %q: schedule: %w", g.Name, err)
	}
	s.entryIDs[g.Name] = id
	s.store.SetNextRun(g.Name, s.cron.Entry(id).Next)
	return nil
}

// RemoveGroup unregisters name's cron entry, if any, and drops it from the
// scheduler. It does not check whether a run is currently in progress - the
// caller (ConfigStore) is the single place that decides whether removal is
// allowed while running, since that policy also has to cover Store.
func (s *Scheduler) RemoveGroup(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if id, ok := s.entryIDs[name]; ok {
		s.cron.Remove(id)
		delete(s.entryIDs, name)
	}
	delete(s.groups, name)
	for i, n := range s.order {
		if n == name {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
}

// Stop stops scheduling new runs and waits for every run this scheduler
// launched - cron-fired or manually triggered - to finish.
func (s *Scheduler) Stop() {
	s.cron.Stop()
	s.wg.Wait()
}

func (s *Scheduler) refreshNextRuns() {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for name, id := range s.entryIDs {
		s.store.SetNextRun(name, s.cron.Entry(id).Next)
	}
}

func (s *Scheduler) fire(ctx context.Context, g ResolvedGroup) {
	s.wg.Add(1)
	defer s.wg.Done()
	defer s.refreshNextRuns()

	runID := newRunID()
	if !s.store.MarkRunning(g.Name, runID) {
		// A manual trigger is already in flight for this group; this tick is
		// skipped rather than queued, the same as an overlapping fire would be.
		return
	}
	s.runGroup(ctx, g, runID, TriggerCron)
}

// Group returns one group's resolved, effective settings.
func (s *Scheduler) Group(name string) (ResolvedGroup, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	g, ok := s.groups[name]
	return g, ok
}

// TriggerNow starts an immediate out-of-schedule run of name, unless one is
// already running. The run uses the service's own root context, not the
// caller's - it must keep running after an HTTP handler returns.
func (s *Scheduler) TriggerNow(name string) (runID string, err error) {
	s.mu.RLock()
	g, ok := s.groups[name]
	s.mu.RUnlock()
	if !ok {
		return "", ErrUnknownGroup
	}

	runID = newRunID()
	if !s.store.MarkRunning(name, runID) {
		return "", ErrAlreadyRunning
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.runGroup(s.rootCtx, g, runID, TriggerManual)
	}()
	return runID, nil
}

// TriggerAllNow starts a sequential, out-of-schedule run of every group, one
// after another in configured order, unless a batch is already in progress.
// Like TriggerNow, the batch runs under the service's own root context so it
// keeps going after the HTTP handler that started it returns - and, unlike a
// single TriggerNow, it keeps going even if the browser that started it is
// closed, since the sequencing itself lives here rather than in the client.
func (s *Scheduler) TriggerAllNow() (BatchStatus, error) {
	s.mu.RLock()
	names := append([]string(nil), s.order...)
	s.mu.RUnlock()

	s.batchMu.Lock()
	if s.batch.Running {
		cur := s.batch
		s.batchMu.Unlock()
		return cur, ErrBatchAlreadyRunning
	}
	s.batch = BatchStatus{Running: true, Names: names, Started: time.Now()}
	cur := s.batch
	s.batchMu.Unlock()

	s.wg.Add(1)
	go s.runBatch(names)
	return cur, nil
}

// runBatch runs every named group in order, waiting for each to finish
// before starting the next - the sequential counterpart to TriggerNow, which
// only ever starts one group and returns immediately. A group already
// running independently (e.g. its own "Run now" was clicked mid-batch) is
// recorded as skipped rather than waited on, so one busy group cannot stall
// the rest of the batch.
func (s *Scheduler) runBatch(names []string) {
	defer s.wg.Done()
	defer s.finishBatch()

	for _, name := range names {
		if s.rootCtx.Err() != nil {
			s.recordBatchResult(name, "skipped", "service is shutting down")
			continue
		}

		s.mu.RLock()
		g, ok := s.groups[name]
		s.mu.RUnlock()
		if !ok {
			s.recordBatchResult(name, "skipped", "group no longer exists")
			continue
		}

		s.setBatchCurrent(name)
		runID := newRunID()
		if !s.store.MarkRunning(name, runID) {
			s.recordBatchResult(name, "skipped", "already running")
			continue
		}

		rec := s.runGroup(s.rootCtx, g, runID, TriggerManual)
		if rec.Err != "" || rec.Failed > 0 {
			s.recordBatchResult(name, "failed", rec.Err)
		} else {
			s.recordBatchResult(name, "ok", "")
		}
	}
}

func (s *Scheduler) setBatchCurrent(name string) {
	s.batchMu.Lock()
	defer s.batchMu.Unlock()
	s.batch.Current = name
}

func (s *Scheduler) recordBatchResult(name, status, reason string) {
	s.batchMu.Lock()
	defer s.batchMu.Unlock()
	s.batch.Results = append(s.batch.Results, BatchResult{Name: name, Status: status, Reason: reason})
	s.batch.Current = ""
}

func (s *Scheduler) finishBatch() {
	s.batchMu.Lock()
	defer s.batchMu.Unlock()
	s.batch.Running = false
	s.batch.Current = ""
	s.batch.Finished = time.Now()
}

// BatchStatus returns a snapshot of the most recent "run all" batch: still
// in progress, finished, or (before any batch has ever run) the zero value.
func (s *Scheduler) BatchStatus() BatchStatus {
	s.batchMu.Lock()
	defer s.batchMu.Unlock()

	b := s.batch
	b.Names = append([]string(nil), s.batch.Names...)
	b.Results = append([]BatchResult(nil), s.batch.Results...)
	return b
}

// runGroup is the headless equivalent of the backup command's runBackup:
// parse the repo list, resolve a layout.Plan, run the pre-flight checks
// unless skipped, then hand off to pipeline.RunBackup with a Reporter that
// updates the Store instead of a TUI.
func (s *Scheduler) runGroup(ctx context.Context, g ResolvedGroup, runID string, trigger RunTrigger) (rec RunRecord) {
	rec = RunRecord{ID: runID, Trigger: trigger, Started: time.Now()}
	defer func() {
		rec.Ended = time.Now()
		if rec.LogPath != "" {
			// Best-effort: a run whose history entry cannot be written just
			// does not survive a restart, it does not make the run itself
			// fail.
			_ = appendHistoryRecord(g.Out, rec, g.HistoryLimit)
			pruneHistoryFiles(g.Out, g.HistoryLimit)
		}
		s.store.MarkFinished(g.Name, rec)
	}()

	list := repolist.ParseEntries(g.Repos)
	rec.RepoCount = len(list.Entries)
	if list.HasErrors() {
		rec.Err = fmt.Sprintf("%d problem(s) in the repo list", len(list.Errors()))
		return
	}
	if len(list.Entries) == 0 {
		rec.Err = "repo list contains no repository URLs"
		return
	}

	plan, err := layout.NewPlan(g.Name, s.configDir, g.Out, g.Stage, time.Now)
	if err != nil {
		rec.Err = err.Error()
		return
	}

	if !g.SkipChecks {
		var checkOut bytes.Buffer
		err := doctorx.Check(ctx, &checkOut,
			doctorx.Options{Git: s.git, Out: plan.Out, ProbeList: &list}, doctorx.ChecksBackup)
		if err != nil {
			rec.Err = strings.TrimSpace(checkOut.String())
			if rec.Err == "" {
				rec.Err = err.Error()
			}
			rec.Out = plan.Out
			return
		}
	}

	gitVersion, _ := s.git.Version(ctx)
	lfsVersion, _ := s.git.LFSVersion(ctx)
	rec.GitVersion, rec.LFSVersion = gitVersion, lfsVersion

	summary, runErr := pipeline.RunBackup(ctx, pipeline.BackupOptions{
		Plan:        plan,
		List:        list,
		Git:         s.git,
		Depth:       g.CloneDepth,
		Jobs:        g.Jobs,
		Retries:     g.Retries,
		Timeout:     g.Timeout,
		FailFast:    g.FailFast,
		ToolVersion: s.version,
		GitVersion:  gitVersion,
		LFSVersion:  lfsVersion,
	}, newStatusReporter(s.store, g.Name))

	rec.OK = summary.Count(pipeline.StatusOK)
	rec.Warn = summary.Count(pipeline.StatusWarn)
	rec.Failed = summary.Count(pipeline.StatusFailed)
	rec.Skipped = summary.Count(pipeline.StatusSkipped)
	rec.TotalBytes = summary.TotalBytes()
	rec.StuckDirs = summary.StuckDirs
	rec.Repos = repoOutcomes(summary)
	rec.Out = summary.Out
	rec.LogPath = summary.Log
	// Best-effort: a run that produced no output, or an Out this process
	// cannot walk, just leaves DirBytes at zero rather than failing the run.
	rec.DirBytes, _ = fsx.DirSize(summary.Out)
	if runErr != nil {
		rec.Err = runErr.Error()
	}
	return
}

// repoOutcomes collects the failed and warned repositories from a run, not
// the OK/skipped ones, so a run's history entry stays a bounded size
// regardless of how many repositories the list held.
func repoOutcomes(summary pipeline.Summary) []RepoOutcome {
	failed, warned := summary.Failed(), summary.Warned()
	out := make([]RepoOutcome, 0, len(failed)+len(warned))
	for _, r := range failed {
		out = append(out, RepoOutcome{Project: r.Project, Repo: r.Repo, Status: r.Status.String(), Reason: r.Reason, Bytes: r.Bytes})
	}
	for _, r := range warned {
		reason := r.Reason
		if reason == "" && len(r.Warnings) > 0 {
			reason = r.Warnings[0]
		}
		out = append(out, RepoOutcome{Project: r.Project, Repo: r.Repo, Status: r.Status.String(), Reason: reason, Bytes: r.Bytes})
	}
	return out
}

// newRunID identifies one run of one group. Uniqueness only has to hold
// within a single group's history, so a microsecond-resolution timestamp is
// enough.
func newRunID() string {
	return time.Now().Format("20060102-150405.000000")
}
