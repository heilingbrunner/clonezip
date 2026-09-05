// Package pipeline performs the work: cloning repositories into archives, and
// restoring one archive back into a usable repository.
//
// It never writes to stdout. Progress is emitted as events to a Reporter, which is
// what lets the same pipeline drive an interactive view, a plain log, or a test that
// asserts on the exact sequence. This package must not import internal/ui or
// bubbletea; that constraint is what keeps it testable.
package pipeline

import (
	"time"

	"github.com/heilingbrunner/clonezip/internal/common/repolist"
)

// Phase is the step a repository is currently in.
type Phase int

const (
	PhaseClone Phase = iota
	PhaseRefs
	PhaseLFS
	PhaseArchive
	PhaseCleanup
	PhaseExtract
	PhaseVerify
	PhaseRestore
)

func (p Phase) String() string {
	switch p {
	case PhaseClone:
		return "clone"
	case PhaseRefs:
		return "refs"
	case PhaseLFS:
		return "lfs"
	case PhaseArchive:
		return "archive"
	case PhaseCleanup:
		return "cleanup"
	case PhaseExtract:
		return "extract"
	case PhaseVerify:
		return "verify"
	case PhaseRestore:
		return "restore"
	}
	return "unknown"
}

// Level ranks a log line coming out of a child process.
type Level int

const (
	LevelInfo Level = iota
	LevelWarn
	LevelError
)

// Event is one progress notification. It is deliberately an empty interface so the
// same values can be sent straight to bubbletea as messages, with no translation
// layer in between.
type Event any

// Reporter receives events. Implementations must be safe to call from several
// goroutines at once, because --jobs above one means several workers report
// concurrently.
type Reporter interface {
	Report(Event)
}

// ReporterFunc adapts a function to Reporter.
type ReporterFunc func(Event)

func (f ReporterFunc) Report(e Event) {
	if f != nil {
		f(e)
	}
}

// Discard drops every event, for tests that do not care.
var Discard Reporter = ReporterFunc(func(Event) {})

// Events emitted during a run.
type (
	// RunStarted is sent once, before any work.
	RunStarted struct {
		Total int
		Out   string
		Log   string
		Jobs  int
		Depth int
	}

	// RepoStarted announces a repository entering the pipeline.
	RepoStarted struct {
		Index   int
		Project string
		Repo    string
		URL     string
	}

	// RepoPhase reports a repository moving to its next step.
	RepoPhase struct {
		Index  int
		Phase  Phase
		Detail string
	}

	// RepoPercent carries a percentage parsed from git's own progress output.
	RepoPercent struct {
		Index int
		Pct   int
		Label string
	}

	// RepoBytes reports byte progress, used while writing an archive where the
	// total is known up front.
	RepoBytes struct {
		Index int
		Done  int64
		Total int64
	}

	// RepoLog carries one line of child output. Info lines are transient detail;
	// warnings and errors are kept in the scrollback.
	RepoLog struct {
		Index int
		Level Level
		Line  string
	}

	// RepoRetry reports that a transient failure is being retried.
	RepoRetry struct {
		Index   int
		Attempt int
		Of      int
		Reason  string
		Wait    time.Duration
	}

	// RepoDone reports a finished repository, successful or not.
	RepoDone struct {
		Result Result
	}

	// RunDone is sent once, last.
	RunDone struct {
		Summary Summary
	}
)

// Status is how one repository ended up.
type Status int

const (
	// StatusOK means the archive was written with nothing to report.
	StatusOK Status = iota

	// StatusWarn means the archive was written but something is worth knowing,
	// almost always a failed LFS fetch. The archive is a complete git repository
	// either way; only LFS-tracked content would be missing.
	StatusWarn

	// StatusFailed means no archive was produced.
	StatusFailed

	// StatusSkipped means the repository was not attempted.
	StatusSkipped
)

func (s Status) String() string {
	switch s {
	case StatusOK:
		return "ok"
	case StatusWarn:
		return "warn"
	case StatusFailed:
		return "failed"
	case StatusSkipped:
		return "skipped"
	}
	return "unknown"
}

// Result is the outcome for one repository.
type Result struct {
	Index   int
	Project string
	Repo    string
	URL     string // credential-free, safe to print and to store

	// Archive is the path written, empty when nothing was.
	Archive string

	Status   Status
	Bytes    int64
	Refs     int
	LFSBytes int64

	// Empty marks a repository that cloned successfully but has no refs, which is
	// what an unused Azure DevOps repository looks like. It is a success.
	Empty bool

	// Shallow records that history was truncated by --clone-depth.
	Shallow bool

	Dur      time.Duration
	Warnings []string

	// Err carries the full failure, including the git argv and the captured stderr
	// tail, for the run log and the per-failure file.
	Err error

	// Reason is the same failure in one readable line, for the console. The full
	// error is far too long to print next to three hundred repositories.
	Reason string
}

// Slug identifies the result as project/repo, or just repo when the entry has
// no project (github.com, codeberg.org). The project is included for Azure
// (and the workspace for Bitbucket) because repository names repeat across them.
func (r Result) Slug() string {
	if r.Project == "" {
		return r.Repo
	}
	return r.Project + "/" + r.Repo
}

// Summary is the outcome of a whole run.
type Summary struct {
	Out     string
	Log     string
	Started time.Time
	Elapsed time.Duration

	Results []Result

	// Issues are what reading the repo list turned up: label lines, duplicates and
	// anything rejected.
	Issues []repolist.Issue

	// StuckDirs counts staging directories that could not be deleted, so litter is
	// reported rather than left silently behind.
	StuckDirs int
}

// Count returns how many results carry the given status.
func (s Summary) Count(status Status) int {
	n := 0
	for _, r := range s.Results {
		if r.Status == status {
			n++
		}
	}
	return n
}

// Failed returns the results that produced no archive.
func (s Summary) Failed() []Result {
	var out []Result
	for _, r := range s.Results {
		if r.Status == StatusFailed {
			out = append(out, r)
		}
	}
	return out
}

// Warned returns the results that produced an archive with something to report.
func (s Summary) Warned() []Result {
	var out []Result
	for _, r := range s.Results {
		if r.Status == StatusWarn {
			out = append(out, r)
		}
	}
	return out
}

// TotalBytes is how much was written across the run.
func (s Summary) TotalBytes() int64 {
	var total int64
	for _, r := range s.Results {
		total += r.Bytes
	}
	return total
}

// Slowest returns the result that took longest, which is usually the one worth
// investigating after a long run.
func (s Summary) Slowest() (Result, bool) {
	return s.pick(func(a, b Result) bool { return a.Dur > b.Dur })
}

// Largest returns the biggest archive written.
func (s Summary) Largest() (Result, bool) {
	return s.pick(func(a, b Result) bool { return a.Bytes > b.Bytes })
}

func (s Summary) pick(better func(a, b Result) bool) (Result, bool) {
	var best Result
	found := false
	for _, r := range s.Results {
		if !found || better(r, best) {
			best, found = r, true
		}
	}
	return best, found
}

// IssueCount returns how many parse issues carry the given code.
func (s Summary) IssueCount(code string) int {
	n := 0
	for _, issue := range s.Issues {
		if issue.Code == code {
			n++
		}
	}
	return n
}

// ExitCode is the process status for this run. A warning is still success, because
// an archive was produced; a repository that produced nothing is a failure the
// caller has to be able to detect - which the PowerShell script made impossible by
// exiting 0 regardless.
func (s Summary) ExitCode() int {
	if len(s.Failed()) > 0 {
		return 1
	}
	return 0
}
