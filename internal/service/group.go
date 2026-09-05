package service

import "time"

// RunTrigger is what caused a run to start.
type RunTrigger int

const (
	TriggerCron RunTrigger = iota
	TriggerManual
)

func (t RunTrigger) String() string {
	if t == TriggerManual {
		return "manual"
	}
	return "cron"
}

// RunRecord is the outcome of one group run.
type RunRecord struct {
	ID      string
	Trigger RunTrigger
	Started time.Time
	Ended   time.Time

	OK, Warn, Failed, Skipped int
	TotalBytes                int64

	// DirBytes is the actual on-disk size of Out (archives, logs, failure
	// dumps included), from fsx.DirSize - unlike TotalBytes, which only sums
	// the archive bytes tracked while writing them.
	DirBytes int64

	// RepoCount is how many repositories the run's repo list held.
	RepoCount int

	// GitVersion and LFSVersion are what ran this backup, the same versions
	// recorded in every archive's manifest.
	GitVersion string
	LFSVersion string

	// StuckDirs counts staging directories that could not be deleted.
	StuckDirs int

	// Repos holds the failed and warned repositories, not the OK/skipped
	// ones, so a run's history entry stays a bounded size regardless of how
	// many repositories the list held.
	Repos []RepoOutcome

	// Out and LogPath point at pipeline.Summary's run directory and JSONL log.
	// This record itself is additionally appended, as one JSON line, to
	// Out's clonezip-history.log (see history.go's
	// appendHistoryRecord/loadHistoryFromDisk) - separate from LogPath's
	// verbose git progress output - so a service restart rebuilds a group's
	// in-memory history from that file rather than starting empty.
	Out     string
	LogPath string

	// Err is the top-level RunBackup error, if any. A per-repository failure is
	// not an error here; it is counted in Failed.
	Err string
}

// RepoOutcome is one repository's failure or warning within a run.
type RepoOutcome struct {
	Project, Repo, Status, Reason string
	Bytes                         int64
}

// RepoLive is one repository's progress within an in-flight run.
type RepoLive struct {
	Index   int
	Project string
	Repo    string
	Phase   string
	Pct     int
	// Status is empty while the repository is still being worked on, and set
	// to "ok"/"warn"/"failed"/"skipped" once it finishes.
	Status string

	// Started is when this repository entered the pipeline, so the dashboard
	// can show elapsed time and flag one running far longer than its peers.
	Started time.Time

	// Detail is phase detail, e.g. "12 refs" during the LFS phase.
	Detail string

	// BytesDone/BytesTotal track archive-writing progress. Both are zero
	// outside the archive phase.
	BytesDone, BytesTotal int64

	// LastLine is the most recent child-process output line for this
	// repository, a transient "what is it doing right now" hint - not a
	// persisted record, that is what the run's own log file is for.
	LastLine string

	// RetryAttempt is nonzero while a retried clone is in flight.
	RetryAttempt, RetryOf int
	RetryReason           string
	RetryWait             time.Duration
}

// LiveProgress is the in-flight state of a running group.
type LiveProgress struct {
	Total int
	Done  int
	Repos []RepoLive

	// Out, Log, Jobs and Depth describe the run in progress: where it is
	// writing, and how it was started.
	Out, Log    string
	Jobs, Depth int
	Started     time.Time

	// LogTail is the most recent output lines across every repository in the
	// run, newest last - a combined console view, capped by logTailLimit.
	// Each repository's own RepoLive.LastLine already carries its most
	// recent line individually; this is the merged, run-wide view.
	LogTail []LiveLogLine
}

// LiveLogLine is one line in a run's combined log tail.
type LiveLogLine struct {
	Repo  string // "project/repo", empty for a line with no associated repository
	Level string // "info", "warn" or "error"
	Line  string
}

// logTailLimit bounds LiveProgress.LogTail, the same bounded-cache approach
// historyLimit uses for a group's run history.
const logTailLimit = 200

// GroupStatus is a point-in-time snapshot of one group, for the dashboard.
type GroupStatus struct {
	Name         string
	Enabled      bool
	Schedule     string
	Running      bool
	CurrentRunID string
	LastRun      *RunRecord
	NextRun      time.Time
	History      []RunRecord // newest first
}
