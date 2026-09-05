package service

import (
	"time"

	"github.com/heilingbrunner/clonezip/internal/common/pipeline"
)

// statusReporter is a pipeline.Reporter that updates a Store instead of
// rendering a TUI or a plain log - the headless analogue of internal/ui.Plain.
type statusReporter struct {
	store *Store
	group string
}

func newStatusReporter(store *Store, group string) *statusReporter {
	return &statusReporter{store: store, group: group}
}

// Report implements pipeline.Reporter.
func (r *statusReporter) Report(event pipeline.Event) {
	switch e := event.(type) {
	case pipeline.RunStarted:
		r.store.StartLive(r.group, LiveProgress{
			Total: e.Total, Out: e.Out, Log: e.Log, Jobs: e.Jobs, Depth: e.Depth,
			Started: time.Now(),
		})

	case pipeline.RepoStarted:
		r.store.UpdateLive(r.group, e.Index, func(live *RepoLive) {
			live.Index = e.Index
			live.Project = e.Project
			live.Repo = e.Repo
			live.Started = time.Now()
		})

	case pipeline.RepoPhase:
		r.store.UpdateLive(r.group, e.Index, func(live *RepoLive) {
			live.Phase = e.Phase.String()
			live.Detail = e.Detail
			// A repository moving to a new phase leaves the previous phase's
			// transient state behind, the same reset the TUI does.
			live.BytesDone, live.BytesTotal = 0, 0
			live.RetryAttempt, live.RetryOf, live.RetryReason, live.RetryWait = 0, 0, "", 0
		})

	case pipeline.RepoPercent:
		r.store.UpdateLive(r.group, e.Index, func(live *RepoLive) {
			live.Pct = e.Pct
			live.Detail = e.Label
		})

	case pipeline.RepoBytes:
		r.store.UpdateLive(r.group, e.Index, func(live *RepoLive) {
			live.BytesDone, live.BytesTotal = e.Done, e.Total
		})

	case pipeline.RepoRetry:
		r.store.UpdateLive(r.group, e.Index, func(live *RepoLive) {
			live.RetryAttempt, live.RetryOf = e.Attempt, e.Of
			live.RetryReason, live.RetryWait = e.Reason, e.Wait
		})

	case pipeline.RepoLog:
		r.store.UpdateLive(r.group, e.Index, func(live *RepoLive) {
			live.LastLine = e.Line
		})
		r.store.AppendLog(r.group, e.Index, levelString(e.Level), e.Line)

	case pipeline.RepoDone:
		r.store.UpdateLive(r.group, e.Result.Index, func(live *RepoLive) {
			live.Status = e.Result.Status.String()
			live.Pct = 100
			// Keep the archive's final size after the repo finishes, so a
			// live "bytes so far" total can sum completed repos too, not
			// just the one currently mid-archive.
			live.BytesDone, live.BytesTotal = e.Result.Bytes, e.Result.Bytes
		})
		r.store.MarkRepoDone(r.group)
	}
}

func levelString(l pipeline.Level) string {
	switch l {
	case pipeline.LevelWarn:
		return "warn"
	case pipeline.LevelError:
		return "error"
	default:
		return "info"
	}
}
