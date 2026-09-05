package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/heilingbrunner/clonezip/internal/common/gitx"
	"github.com/heilingbrunner/clonezip/internal/common/layout"
	"github.com/heilingbrunner/clonezip/internal/common/repolist"
	"github.com/heilingbrunner/clonezip/internal/common/ziparc"
)

// fakeArchiver stands in for the real zip writer. It records what it was asked to do
// and creates a placeholder, so the pipeline can be exercised without compressing
// anything.
type fakeArchiver struct {
	mu    sync.Mutex
	calls []archiveCall

	// err, when set, makes every call fail.
	err error
}

type archiveCall struct {
	SrcDir   string
	RootName string
	Dest     string
	Extra    map[string][]byte
}

func (a *fakeArchiver) CreateFromDir(_ context.Context, srcDir, rootName, dest string,
	extra map[string][]byte, progress ziparc.Progress,
) (int64, error) {
	a.mu.Lock()
	a.calls = append(a.calls, archiveCall{SrcDir: srcDir, RootName: rootName, Dest: dest, Extra: extra})
	a.mu.Unlock()

	if a.err != nil {
		return 0, a.err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return 0, err
	}
	body := []byte("fake archive of " + rootName)
	if err := os.WriteFile(dest, body, 0o644); err != nil {
		return 0, err
	}
	if progress != nil {
		progress(int64(len(body)), int64(len(body)))
	}
	return int64(len(body)), nil
}

func (a *fakeArchiver) Calls() []archiveCall {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]archiveCall(nil), a.calls...)
}

// recordingReporter keeps every event so a test can assert the sequence.
type recordingReporter struct {
	mu     sync.Mutex
	events []Event
}

func (r *recordingReporter) Report(e Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recordingReporter) Events() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Event(nil), r.events...)
}

// phasesFor returns the phases reported for one repository, in order.
func (r *recordingReporter) phasesFor(index int) []Phase {
	var out []Phase
	for _, event := range r.Events() {
		if e, ok := event.(RepoPhase); ok && e.Index == index {
			out = append(out, e.Phase)
		}
	}
	return out
}

func (r *recordingReporter) has(match func(Event) bool) bool {
	for _, event := range r.Events() {
		if match(event) {
			return true
		}
	}
	return false
}

// testList builds a parsed list from synthetic URLs.
func testList(t *testing.T, urls ...string) repolist.List {
	t.Helper()
	var yamlList strings.Builder
	yamlList.WriteString("repos:\n")
	for _, u := range urls {
		yamlList.WriteString("  - " + u + "\n")
	}
	list, err := repolist.Parse([]byte(yamlList.String()))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(list.Entries) != len(urls) {
		t.Fatalf("built %d entries from %d URLs: %v", len(list.Entries), len(urls), list.Issues)
	}
	return list
}

func testPlan(t *testing.T) layout.Plan {
	t.Helper()
	stamp := time.Date(2026, 7, 27, 16, 42, 11, 0, time.UTC)
	plan, err := layout.NewPlan("list.txt", t.TempDir(), "", "", func() time.Time { return stamp })
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}
	return plan
}

// run wires a pipeline over a fake git and a fake archiver.
func run(t *testing.T, list repolist.List, fake *gitx.FakeRunner, arc *fakeArchiver,
	adjust func(*BackupOptions),
) (Summary, *recordingReporter) {
	t.Helper()

	opts := BackupOptions{
		Plan:        testPlan(t),
		List:        list,
		Git:         gitx.New(fake),
		Arc:         arc,
		Jobs:        1,
		ToolVersion: "test",
	}
	if adjust != nil {
		adjust(&opts)
	}

	rep := &recordingReporter{}
	summary, err := RunBackup(context.Background(), opts, rep)
	if err != nil {
		t.Fatalf("RunBackup: %v", err)
	}
	return summary, rep
}

// stageDirsUnder lists what is left in the staging root.
func stageDirsUnder(t *testing.T, stage string) []string {
	t.Helper()
	entries, err := os.ReadDir(stage)
	if err != nil {
		return nil // removed entirely, which is the expected end state
	}
	var out []string
	for _, entry := range entries {
		out = append(out, entry.Name())
	}
	return out
}

func TestRunBackupHappyPath(t *testing.T) {
	list := testList(t, "https://dev.azure.com/org/Inosoft/_git/VisiWin7-Licenses")
	fake := gitx.NewFakeRunner().Respond("for-each-ref", gitx.Response{
		Stdout: []string{"refs/heads/main", "refs/tags/v1"},
	})
	arc := &fakeArchiver{}

	summary, rep := run(t, list, fake, arc, nil)

	if got := summary.Count(StatusOK); got != 1 {
		t.Fatalf("ok = %d, want 1; results: %+v", got, summary.Results)
	}
	if summary.ExitCode() != 0 {
		t.Errorf("ExitCode = %d, want 0", summary.ExitCode())
	}

	result := summary.Results[0]
	if result.Refs != 2 {
		t.Errorf("refs = %d, want 2", result.Refs)
	}
	if result.Empty {
		t.Error("a repository with refs was reported as empty")
	}
	if result.Bytes == 0 {
		t.Error("no archive size recorded")
	}

	// The phases have to happen in this order, or the archive would be made before
	// the LFS objects it is supposed to contain.
	want := []Phase{PhaseClone, PhaseRefs, PhaseLFS, PhaseArchive, PhaseCleanup}
	got := rep.phasesFor(0)
	if len(got) != len(want) {
		t.Fatalf("phases = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("phase %d = %v, want %v", i, got[i], want[i])
		}
	}

	// The archive is named for the repository even though staging is keyed by index.
	calls := arc.Calls()
	if len(calls) != 1 {
		t.Fatalf("archiver called %d times, want 1", len(calls))
	}
	if calls[0].RootName != "VisiWin7-Licenses.git" {
		t.Errorf("archive root = %q, want VisiWin7-Licenses.git", calls[0].RootName)
	}
	if _, ok := calls[0].Extra["clonezip-manifest.json"]; !ok {
		t.Error("no manifest was passed to the archiver")
	}
}

func TestRunBackupIsolatesAFailure(t *testing.T) {
	// The whole point of the rewrite: the script never checked git's exit status, so a
	// failed clone was archived as though it had worked and the run reported success.
	// Here one repository fails and the others must still be archived.
	list := testList(t,
		"https://dev.azure.com/org/Proj/_git/First",
		"https://dev.azure.com/org/Proj/_git/Broken",
		"https://dev.azure.com/org/Proj/_git/Third",
	)
	fake := gitx.NewFakeRunner().
		Respond("for-each-ref", gitx.Response{Stdout: []string{"refs/heads/main"}}).
		Fail("_git/Broken", 128, "remote: TF401019: The Git repository does not exist",
			"fatal: repository not found")
	arc := &fakeArchiver{}

	summary, _ := run(t, list, fake, arc, nil)

	if got := summary.Count(StatusOK); got != 2 {
		t.Errorf("ok = %d, want 2", got)
	}
	if got := summary.Count(StatusFailed); got != 1 {
		t.Errorf("failed = %d, want 1", got)
	}
	if summary.ExitCode() != 1 {
		t.Errorf("ExitCode = %d, want 1 so a scheduled run can detect the failure", summary.ExitCode())
	}

	failed := summary.Failed()
	if len(failed) != 1 || failed[0].Repo != "Broken" {
		t.Fatalf("unexpected failures: %+v", failed)
	}
	// The remote's own message has to survive, or the failure is undiagnosable.
	if !strings.Contains(failed[0].Reason, "repository not found") {
		t.Errorf("Reason = %q, want the remote's message", failed[0].Reason)
	}
	if failed[0].Archive != "" {
		t.Error("a failed repository recorded an archive path")
	}

	// Only the two that succeeded were archived.
	if got := len(arc.Calls()); got != 2 {
		t.Errorf("archiver called %d times, want 2", got)
	}
}

func TestRunBackupTreatsLFSFailureAsAWarning(t *testing.T) {
	// Deliberate: the archive is still a complete git repository, only LFS-tracked
	// content would be missing. It must be produced, and the reason recorded.
	list := testList(t, "https://dev.azure.com/org/Proj/_git/HasLFS")
	fake := gitx.NewFakeRunner().
		Respond("for-each-ref", gitx.Response{Stdout: []string{"refs/heads/main"}}).
		Fail("lfs fetch", 2, "LFS: Service Unavailable")
	arc := &fakeArchiver{}

	summary, rep := run(t, list, fake, arc, nil)

	if got := summary.Count(StatusWarn); got != 1 {
		t.Fatalf("warn = %d, want 1; results: %+v", got, summary.Results)
	}
	if summary.ExitCode() != 0 {
		t.Errorf("ExitCode = %d, want 0: a warning still produced an archive", summary.ExitCode())
	}

	result := summary.Results[0]
	if result.Archive == "" {
		t.Error("no archive was produced despite the LFS failure being non-fatal")
	}
	if len(result.Warnings) == 0 || !strings.Contains(result.Warnings[0], "Service Unavailable") {
		t.Errorf("warnings = %v, want the LFS reason", result.Warnings)
	}
	if len(arc.Calls()) != 1 {
		t.Fatal("the archive was not written")
	}

	// The manifest has to record that LFS was attempted and did not succeed.
	manifestJSON := string(arc.Calls()[0].Extra["clonezip-manifest.json"])
	if !strings.Contains(manifestJSON, `"attempted": true`) {
		t.Error("manifest does not record that the LFS fetch was attempted")
	}
	if !strings.Contains(manifestJSON, `"ok": false`) {
		t.Error("manifest does not record that the LFS fetch failed")
	}

	if !rep.has(func(e Event) bool {
		log, ok := e.(RepoLog)
		return ok && log.Level == LevelWarn && strings.Contains(log.Line, "LFS fetch failed")
	}) {
		t.Error("no warning was reported for the failed LFS fetch")
	}
}

func TestRunBackupHandlesAnEmptyRepository(t *testing.T) {
	// An unused Azure DevOps repository clones fine and has no refs. That is a
	// success, and there is nothing for LFS to fetch.
	list := testList(t, "https://dev.azure.com/org/Proj/_git/NeverUsed")
	fake := gitx.NewFakeRunner() // for-each-ref returns nothing
	arc := &fakeArchiver{}

	summary, rep := run(t, list, fake, arc, nil)

	result := summary.Results[0]
	if result.Status != StatusOK {
		t.Errorf("status = %v, want ok", result.Status)
	}
	if !result.Empty || result.Refs != 0 {
		t.Errorf("empty = %v, refs = %d; want true and 0", result.Empty, result.Refs)
	}
	if len(arc.Calls()) != 1 {
		t.Error("an empty repository should still be archived")
	}
	// Fetching LFS for no refs would be a pointless subprocess.
	if len(fake.CallsMatching("lfs fetch")) != 0 {
		t.Error("lfs fetch ran for a repository with no refs")
	}
	for _, phase := range rep.phasesFor(0) {
		if phase == PhaseLFS {
			t.Error("the LFS phase was reported for a repository with no refs")
		}
	}
}

func TestRunBackupCleansStagingOnSuccessAndFailure(t *testing.T) {
	list := testList(t,
		"https://dev.azure.com/org/Proj/_git/Good",
		"https://dev.azure.com/org/Proj/_git/Bad",
	)
	fake := gitx.NewFakeRunner().
		Respond("for-each-ref", gitx.Response{Stdout: []string{"refs/heads/main"}}).
		Fail("_git/Bad", 128, "fatal: repository not found")
	arc := &fakeArchiver{}

	summary, _ := run(t, list, fake, arc, nil)

	// Left-behind clones would fill the disk over a 300-repository run.
	if left := stageDirsUnder(t, filepath.Join(summary.Out, ".stage")); len(left) > 0 {
		t.Errorf("staging directories left behind: %v", left)
	}
	if summary.StuckDirs != 0 {
		t.Errorf("StuckDirs = %d, want 0", summary.StuckDirs)
	}
}

func TestRunBackupStagesByIndexSoParallelIsSafe(t *testing.T) {
	// 25 repository basenames repeat across projects in the real lists. The PowerShell
	// script staged everything at temp/<repo>, which only worked because it ran
	// strictly sequentially.
	list := testList(t,
		"https://dev.azure.com/org/Inosoft/_git/VisiWinPowerToys",
		"https://dev.azure.com/org/Tools/_git/VisiWinPowerToys",
	)
	fake := gitx.NewFakeRunner().Respond("for-each-ref", gitx.Response{
		Stdout: []string{"refs/heads/main"},
	})
	arc := &fakeArchiver{}

	summary, _ := run(t, list, fake, arc, func(o *BackupOptions) { o.Jobs = 2 })

	if got := summary.Count(StatusOK); got != 2 {
		t.Fatalf("ok = %d, want 2; results: %+v", got, summary.Results)
	}

	calls := arc.Calls()
	if len(calls) != 2 {
		t.Fatalf("archiver called %d times, want 2", len(calls))
	}
	if calls[0].SrcDir == calls[1].SrcDir {
		t.Errorf("both repositories staged in the same directory: %q", calls[0].SrcDir)
	}
	if calls[0].Dest == calls[1].Dest {
		t.Errorf("both repositories wrote the same archive: %q", calls[0].Dest)
	}
}

func TestRunBackupReportsAnArchiveFailure(t *testing.T) {
	list := testList(t, "https://dev.azure.com/org/Proj/_git/Repo")
	fake := gitx.NewFakeRunner().Respond("for-each-ref", gitx.Response{
		Stdout: []string{"refs/heads/main"},
	})
	arc := &fakeArchiver{err: errors.New("disk full")}

	summary, _ := run(t, list, fake, arc, nil)

	// The script never checked 7-Zip's exit code either, so a failed compression
	// looked exactly like a successful backup.
	if got := summary.Count(StatusFailed); got != 1 {
		t.Fatalf("failed = %d, want 1", got)
	}
	if !strings.Contains(summary.Failed()[0].Reason, "disk full") {
		t.Errorf("Reason = %q, want it to mention the archiver error", summary.Failed()[0].Reason)
	}
}

func TestRunBackupStripsCredentialsFromTheArchivedRemote(t *testing.T) {
	// A token in the list may reach git and nothing else. Left in the bare repo's
	// config it would travel with the archive to wherever the archive goes.
	list := testList(t, "https://user:s3cr3t@dev.azure.com/org/Proj/_git/Repo")
	fake := gitx.NewFakeRunner().Respond("for-each-ref", gitx.Response{
		Stdout: []string{"refs/heads/main"},
	})
	arc := &fakeArchiver{}

	summary, _ := run(t, list, fake, arc, nil)

	rewrites := fake.CallsMatching("remote set-url")
	if len(rewrites) != 1 {
		t.Fatalf("origin was rewritten %d times, want 1", len(rewrites))
	}
	if strings.Contains(rewrites[0].Joined(), "s3cr3t") {
		t.Errorf("the rewritten origin still carries the token: %q", rewrites[0].Joined())
	}

	if url := summary.Results[0].URL; strings.Contains(url, "s3cr3t") {
		t.Errorf("the recorded URL leaks the token: %q", url)
	}
	if manifestJSON := string(arc.Calls()[0].Extra["clonezip-manifest.json"]); strings.Contains(manifestJSON, "s3cr3t") {
		t.Error("the manifest leaks the token")
	}
}

func TestRunBackupFailFastStopsEarly(t *testing.T) {
	list := testList(t,
		"https://dev.azure.com/org/Proj/_git/Bad",
		"https://dev.azure.com/org/Proj/_git/Second",
		"https://dev.azure.com/org/Proj/_git/Third",
	)
	fake := gitx.NewFakeRunner().
		Respond("for-each-ref", gitx.Response{Stdout: []string{"refs/heads/main"}}).
		Fail("_git/Bad", 128, "fatal: repository not found")
	arc := &fakeArchiver{}

	summary, _ := run(t, list, fake, arc, func(o *BackupOptions) { o.FailFast = true })

	if got := summary.Count(StatusFailed); got != 1 {
		t.Errorf("failed = %d, want 1", got)
	}
	// The rest are accounted for rather than silently missing, so the counts still add
	// up to the size of the list.
	if got := len(summary.Results); got != 3 {
		t.Errorf("results = %d, want one per list entry", got)
	}
	if got := summary.Count(StatusSkipped); got != 2 {
		t.Errorf("skipped = %d, want 2", got)
	}
}

func TestRunBackupWritesARunLog(t *testing.T) {
	list := testList(t, "https://dev.azure.com/org/Proj/_git/Repo")
	fake := gitx.NewFakeRunner().Respond("for-each-ref", gitx.Response{
		Stdout: []string{"refs/heads/main"},
	})

	summary, _ := run(t, list, fake, &fakeArchiver{}, nil)

	// Without this there is no way to diagnose a repository that failed two hours into
	// a run.
	data, err := os.ReadFile(summary.Log)
	if err != nil {
		t.Fatalf("run log: %v", err)
	}
	body := string(data)
	for _, want := range []string{`"msg":"run started"`, `"msg":"repository done"`, `"msg":"run finished"`} {
		if !strings.Contains(body, want) {
			t.Errorf("run log does not contain %s", want)
		}
	}
	if !strings.Contains(body, `"elapsed":"`) {
		t.Error("run finished log does not contain a readable elapsed duration")
	}
	if strings.Contains(body, `"elapsed":0`) {
		t.Error("run finished log contains elapsed as raw nanoseconds")
	}
}

func TestRunBackupRetriesOnlyTransientFailures(t *testing.T) {
	tests := []struct {
		name        string
		stderr      string
		wantRetries bool
	}{
		{"service unavailable", "error: RPC failed; HTTP 503", true},
		{"connection reset", "fatal: unable to access: Connection was reset", true},
		{"early eof", "fatal: early EOF", true},
		// Retrying these would multiply the wait before the same answer, which over
		// hundreds of repositories is minutes wasted for nothing.
		{"authentication", "fatal: Authentication failed for 'https://dev.azure.com'", false},
		{"missing repository", "remote: TF401019: The Git repository does not exist", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			list := testList(t, "https://dev.azure.com/org/Proj/_git/Repo")
			fake := gitx.NewFakeRunner().Fail("clone --bare", 128, tc.stderr)

			summary, rep := run(t, list, fake, &fakeArchiver{}, func(o *BackupOptions) {
				o.Retries = 1
			})

			if summary.Count(StatusFailed) != 1 {
				t.Fatalf("expected the repository to fail: %+v", summary.Results)
			}

			attempts := len(fake.CallsMatching("clone --bare"))
			retried := rep.has(func(e Event) bool { _, ok := e.(RepoRetry); return ok })

			switch {
			case tc.wantRetries && attempts != 2:
				t.Errorf("clone attempted %d times, want 2 for a transient failure", attempts)
			case !tc.wantRetries && attempts != 1:
				t.Errorf("clone attempted %d times, want 1 for a permanent failure", attempts)
			}
			if retried != tc.wantRetries {
				t.Errorf("retry reported = %v, want %v", retried, tc.wantRetries)
			}
		})
	}
}

func TestRunBackupPassesDepthThrough(t *testing.T) {
	list := testList(t, "https://dev.azure.com/org/Proj/_git/Repo")
	fake := gitx.NewFakeRunner().Respond("for-each-ref", gitx.Response{
		Stdout: []string{"refs/heads/main"},
	})

	summary, _ := run(t, list, fake, &fakeArchiver{}, func(o *BackupOptions) { o.Depth = 5 })

	joined := fake.CallsMatching("clone --bare")[0].Joined()
	if !strings.Contains(joined, "--depth=5") {
		t.Errorf("args %q missing the depth", joined)
	}
	// Without this, --depth silently reduces the backup to the default branch.
	if !strings.Contains(joined, "--no-single-branch") {
		t.Errorf("args %q missing --no-single-branch", joined)
	}
	if !summary.Results[0].Shallow {
		t.Error("the result does not record that history was truncated")
	}
}

func TestSummaryExitCodeMatrix(t *testing.T) {
	tests := []struct {
		name     string
		statuses []Status
		want     int
	}{
		{"empty", nil, 0},
		{"all ok", []Status{StatusOK, StatusOK}, 0},
		{"warnings only", []Status{StatusOK, StatusWarn}, 0},
		{"one failure", []Status{StatusOK, StatusFailed}, 1},
		{"skipped only", []Status{StatusSkipped}, 0},
		{"failure and skips", []Status{StatusFailed, StatusSkipped}, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var summary Summary
			for _, status := range tc.statuses {
				summary.Results = append(summary.Results, Result{Status: status})
			}
			if got := summary.ExitCode(); got != tc.want {
				t.Errorf("ExitCode = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestRunBackupStopsOnCancellation(t *testing.T) {
	list := testList(t,
		"https://dev.azure.com/org/Proj/_git/One",
		"https://dev.azure.com/org/Proj/_git/Two",
	)
	fake := gitx.NewFakeRunner().Respond("clone --bare", gitx.Response{Delay: time.Hour})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	summary, err := RunBackup(ctx, BackupOptions{
		Plan:        testPlan(t),
		List:        list,
		Git:         gitx.New(fake),
		Arc:         &fakeArchiver{},
		Jobs:        1,
		ToolVersion: "test",
	}, Discard)

	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
	// The summary still has to be usable: knowing how far a cancelled run got is the
	// whole reason it is returned rather than discarded.
	if len(summary.Results) != 2 {
		t.Errorf("results = %d, want one per list entry even when cancelled", len(summary.Results))
	}
}
