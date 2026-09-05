package gitx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCloneBareArgs(t *testing.T) {
	tests := []struct {
		name     string
		depth    int
		wantHas  []string
		wantMiss []string
	}{
		{
			name:     "full clone",
			depth:    0,
			wantHas:  []string{"clone", "--bare", "--progress", "core.longpaths=true"},
			wantMiss: []string{"--depth", "--no-single-branch"},
		},
		{
			// --depth implies --single-branch, which would silently reduce the
			// backup to the default branch. Passing --no-single-branch is what
			// keeps every branch tip in the archive.
			name:    "shallow clone keeps every branch",
			depth:   5,
			wantHas: []string{"--depth=5", "--no-single-branch"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := NewFakeRunner()
			g := New(fake)

			if err := g.CloneBare(context.Background(),
				"https://dev.azure.com/org/Proj/_git/Repo", "stage/0001", tc.depth, nil); err != nil {
				t.Fatalf("CloneBare: %v", err)
			}

			calls := fake.Calls()
			if len(calls) != 1 {
				t.Fatalf("got %d calls, want 1", len(calls))
			}
			joined := calls[0].Joined()
			for _, want := range tc.wantHas {
				if !strings.Contains(joined, want) {
					t.Errorf("args %q missing %q", joined, want)
				}
			}
			for _, miss := range tc.wantMiss {
				if strings.Contains(joined, miss) {
					t.Errorf("args %q should not contain %q", joined, miss)
				}
			}
		})
	}
}

func TestRefNamesPassesFormatUnquoted(t *testing.T) {
	fake := NewFakeRunner().Respond("for-each-ref", Response{
		Stdout: []string{"refs/heads/main", "refs/heads/feature", "refs/tags/v1.0"},
	})

	refs, err := New(fake).RefNames(context.Background(), "bare")
	if err != nil {
		t.Fatalf("RefNames: %v", err)
	}
	if len(refs) != 3 {
		t.Fatalf("got %d refs, want 3: %v", len(refs), refs)
	}

	// PowerShell stripped the quotes in --format='%(refname)' before git saw
	// them; passing them literally here would make git emit them verbatim.
	joined := fake.Calls()[0].Joined()
	if !strings.Contains(joined, "--format=%(refname)") {
		t.Errorf("args %q do not carry the expected format", joined)
	}
	if strings.Contains(joined, "'") {
		t.Errorf("args %q contain a quote character, which git would take literally", joined)
	}
	for _, want := range []string{"refs/heads", "refs/tags"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args %q missing %q", joined, want)
		}
	}
}

func TestChunkRefsRespectsTheLimit(t *testing.T) {
	refs := make([]string, 500)
	for i := range refs {
		refs[i] = fmt.Sprintf("refs/tags/release-2026-07-27-build-%04d", i)
	}

	batches := chunkRefs(refs, maxRefArgsLen)
	if len(batches) < 2 {
		t.Fatalf("500 long refs produced %d batch(es); the point is to split them", len(batches))
	}

	var total int
	for i, batch := range batches {
		total += len(batch)
		if n := len(strings.Join(batch, " ")); n > maxRefArgsLen {
			t.Errorf("batch %d is %d characters, over the %d limit", i, n, maxRefArgsLen)
		}
	}
	if total != len(refs) {
		t.Errorf("batches hold %d refs, want all %d: none may be dropped", total, len(refs))
	}
}

func TestChunkRefsKeepsAnOversizedRef(t *testing.T) {
	// Dropping a ref silently would be worse than handing git a long command
	// line and letting it complain.
	huge := "refs/tags/" + strings.Repeat("x", maxRefArgsLen*2)
	batches := chunkRefs([]string{"refs/heads/main", huge}, maxRefArgsLen)

	var seen int
	for _, batch := range batches {
		seen += len(batch)
	}
	if seen != 2 {
		t.Errorf("got %d refs across batches, want both kept", seen)
	}
}

func TestLFSFetchBatchesAndNeverUsesAll(t *testing.T) {
	refs := make([]string, 400)
	for i := range refs {
		refs[i] = fmt.Sprintf("refs/tags/a-reasonably-long-tag-name-number-%04d", i)
	}

	fake := NewFakeRunner()
	if err := New(fake).LFSFetch(context.Background(), "bare", refs, nil); err != nil {
		t.Fatalf("LFSFetch: %v", err)
	}

	calls := fake.CallsMatching("lfs fetch")
	if len(calls) < 2 {
		t.Fatalf("got %d lfs fetch calls, want the refs split across several", len(calls))
	}
	for _, call := range calls {
		joined := call.Joined()
		// --all would pull every past revision of every large file and defeat
		// the tip-only design the archive size depends on.
		if strings.Contains(joined, "--all") {
			t.Errorf("lfs fetch used --all, which pulls the whole LFS history: %q", joined)
		}
		if !strings.Contains(joined, "origin") {
			t.Errorf("lfs fetch did not name the remote: %q", joined)
		}
	}
}

func TestLFSFetchWithNoRefsDoesNothing(t *testing.T) {
	// An empty Azure DevOps repository clones fine and has no refs at all.
	fake := NewFakeRunner()
	if err := New(fake).LFSFetch(context.Background(), "bare", nil, nil); err != nil {
		t.Fatalf("LFSFetch with no refs: %v", err)
	}
	if n := len(fake.Calls()); n != 0 {
		t.Errorf("made %d call(s) for an empty ref list, want none", n)
	}
}

func TestLFSFetchReportsFailure(t *testing.T) {
	fake := NewFakeRunner().Fail("lfs fetch", 2, "LFS: Service Unavailable")

	err := New(fake).LFSFetch(context.Background(), "bare", []string{"refs/heads/main"}, nil)
	if err == nil {
		t.Fatal("LFSFetch returned nil for a failing fetch")
	}
	// The caller downgrades this to a warning and still writes the archive; what
	// matters here is that the failure is not swallowed.
	if !strings.Contains(err.Error(), "Service Unavailable") {
		t.Errorf("error %q does not carry the reason from stderr", err)
	}
}

func TestCloneUsesFileURLWhenLocalIsRefused(t *testing.T) {
	fake := NewFakeRunner()
	g := New(fake)

	// A shallow bare repository cannot be cloned with the default hardlink copy:
	// git refuses with "source repository is shallow, reject to clone".
	if err := g.Clone(context.Background(), filepath.Join("out", "Repo.git"), "Repo", true, nil); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	joined := fake.Calls()[0].Joined()
	if !strings.Contains(joined, "--no-local") {
		t.Errorf("args %q missing --no-local", joined)
	}
	if !strings.Contains(joined, "file://") {
		t.Errorf("args %q should address the source as a file:// URL", joined)
	}

	fake.Reset()
	if err := g.Clone(context.Background(), filepath.Join("out", "Repo.git"), "Repo", false, nil); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if joined := fake.Calls()[0].Joined(); strings.Contains(joined, "--no-local") {
		t.Errorf("a normal clone should not pass --no-local: %q", joined)
	}
}

func TestLocalFileURL(t *testing.T) {
	got := LocalFileURL(filepath.Join("out", "Repo.git"))
	if !strings.HasPrefix(got, "file:///") {
		t.Errorf("LocalFileURL = %q, want it to start with file:///", got)
	}
	if strings.Contains(got, `\`) {
		t.Errorf("LocalFileURL = %q, want forward slashes only", got)
	}
}

func TestVersionParsing(t *testing.T) {
	fake := NewFakeRunner().
		Respond("version", Response{Stdout: []string{"git version 2.55.0.windows.3"}}).
		Respond("lfs version", Response{
			Stdout: []string{"git-lfs/3.7.1 (GitHub; windows amd64; go 1.25.1; git b84b3384)"},
		})
	g := New(fake)

	if got, err := g.Version(context.Background()); err != nil || got != "2.55.0.windows.3" {
		t.Errorf("Version = %q, %v; want 2.55.0.windows.3", got, err)
	}
	if got, err := g.LFSVersion(context.Background()); err != nil || got != "3.7.1" {
		t.Errorf("LFSVersion = %q, %v; want 3.7.1", got, err)
	}
}

func TestConfigGetTreatsMissingKeyAsEmpty(t *testing.T) {
	// git exits 1 for an unset key. That is an answer, not a failure.
	fake := NewFakeRunner().Fail("config --get", 1)

	got, err := New(fake).ConfigGet(context.Background(), "core.longpaths")
	if err != nil {
		t.Fatalf("ConfigGet on an unset key returned %v, want no error", err)
	}
	if got != "" {
		t.Errorf("ConfigGet = %q, want empty", got)
	}
}

func TestConfigGetPropagatesRealFailures(t *testing.T) {
	fake := NewFakeRunner().Fail("config --get", 128, "fatal: not in a git directory")
	if _, err := New(fake).ConfigGet(context.Background(), "core.longpaths"); err == nil {
		t.Fatal("ConfigGet swallowed a genuine failure")
	}
}

func TestSplitLinesOrCR(t *testing.T) {
	// git writes progress with carriage returns and no newline. Without splitting
	// on "\r" the whole clone arrives as one line at EOF, and there is no
	// progress to show while it is still running.
	input := "Cloning into bare repository...\n" +
		"Receiving objects:   1% (1/100)\r" +
		"Receiving objects:  45% (45/100)\r" +
		"Receiving objects: 100% (100/100), done.\r\n" +
		"final line without terminator"

	got := scanAll(t, input)
	want := []string{
		"Cloning into bare repository...",
		"Receiving objects:   1% (1/100)",
		"Receiving objects:  45% (45/100)",
		"Receiving objects: 100% (100/100), done.",
		"final line without terminator",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d:\n got  %q\n want %q", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestSplitLinesOrCRDoesNotSplitCRLFIntoTwo(t *testing.T) {
	// A "\r" arriving as the last byte of a read might be the first half of
	// "\r\n"; emitting immediately would produce a spurious empty line.
	if got := scanAll(t, "one\r\ntwo\r\n"); len(got) != 2 {
		t.Errorf("got %d lines from two CRLF-terminated lines: %q", len(got), got)
	}
}

// scanAll runs the scanner over a string and returns the lines it produced.
func scanAll(t *testing.T, input string) []string {
	t.Helper()
	var got []string
	scan(strings.NewReader(input), StdErr, func(_ Stream, line string) {
		got = append(got, line)
	}, nil)
	return got
}

func TestExitErrorReasonPrefersTheFatalLine(t *testing.T) {
	tests := []struct {
		name string
		tail []string
		want string
	}{
		{
			// The whole point: "exit 128" alone is undiagnosable two hours later.
			name: "remote message",
			tail: []string{
				"Cloning into bare repository 'stage/0001'...",
				"remote: TF401019: The Git repository with name or identifier X does not exist",
				"fatal: repository not found",
			},
			want: "fatal: repository not found",
		},
		{
			name: "progress noise only",
			tail: []string{"Receiving objects:  10% (1/10)", "Receiving objects: 100% (10/10)"},
			want: "Receiving objects: 100% (10/10)",
		},
		{name: "empty", tail: nil, want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := &ExitError{Args: []string{"clone"}, Code: 128, Tail: tc.tail}
			if got := err.Reason(); got != tc.want {
				t.Errorf("Reason = %q, want %q", got, tc.want)
			}
			if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Error() = %q, want it to include the reason", err.Error())
			}
		})
	}
}

func TestRingKeepsTheLastLines(t *testing.T) {
	r := newRing(3)
	for i := 1; i <= 5; i++ {
		r.add(fmt.Sprintf("line %d", i))
	}
	got := r.lines()
	want := []string{"line 3", "line 4", "line 5"}
	if len(got) != len(want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("index %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestRingBelowCapacityKeepsOrder(t *testing.T) {
	r := newRing(10)
	r.add("first")
	r.add("second")
	if got := r.lines(); len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Errorf("lines = %q, want [first second]", got)
	}
}

func TestIsShallowAndHeadRef(t *testing.T) {
	bare := t.TempDir()

	if IsShallow(bare) {
		t.Error("a repository with no shallow file was reported as shallow")
	}
	if got := HeadRef(bare); got != "" {
		t.Errorf("HeadRef with no HEAD file = %q, want empty", got)
	}

	if err := os.WriteFile(filepath.Join(bare, "shallow"), []byte("deadbeef\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !IsShallow(bare) {
		t.Error("a repository with a shallow file was not reported as shallow")
	}

	if err := os.WriteFile(filepath.Join(bare, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := HeadRef(bare); got != "refs/heads/main" {
		t.Errorf("HeadRef = %q, want refs/heads/main", got)
	}

	// A detached HEAD holds an object id, which is not a ref name.
	if err := os.WriteFile(filepath.Join(bare, "HEAD"),
		[]byte("9f3c1ab000000000000000000000000000000000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := HeadRef(bare); got != "" {
		t.Errorf("HeadRef for a detached HEAD = %q, want empty", got)
	}
}

func TestFakeRunnerStopsOnCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	fake := NewFakeRunner()
	if _, err := fake.Run(ctx, "", []string{"clone"}, nil); err == nil {
		t.Fatal("a cancelled context produced no error")
	}
}

// requireGit skips a test when git is unavailable. clonezip cannot work without git,
// so exercising the real runner is worth doing wherever it is present.
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
}

func TestExecRunnerCapturesOutputAndSucceeds(t *testing.T) {
	requireGit(t)

	var lines []string
	code, err := NewExecRunner().Run(context.Background(), "", []string{"version"},
		func(stream Stream, line string) {
			if stream == StdOut {
				lines = append(lines, line)
			}
		})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if len(lines) == 0 || !strings.Contains(lines[0], "git version") {
		t.Errorf("captured stdout = %q, want a git version line", lines)
	}
}

func TestExecRunnerReportsExitCodeAndTail(t *testing.T) {
	requireGit(t)

	// An empty directory is not a repository, so this fails with a message worth
	// carrying back to the caller.
	code, err := NewExecRunner().Run(context.Background(), t.TempDir(),
		[]string{"rev-parse", "--verify", "HEAD"}, nil)
	if err == nil {
		t.Fatal("running rev-parse outside a repository succeeded")
	}
	if code == 0 {
		t.Error("exit code = 0 for a failing command")
	}

	var exitErr *ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("error is %T, want *ExitError", err)
	}
	if exitErr.Code != code {
		t.Errorf("ExitError.Code = %d, but Run returned %d", exitErr.Code, code)
	}
	// Without the tail this is an undiagnosable "exit 128".
	if len(exitErr.Tail) == 0 {
		t.Error("ExitError carries no stderr tail")
	}
	if exitErr.Reason() == "" {
		t.Error("ExitError.Reason is empty, so the failure would report no cause")
	}
}

func TestExecRunnerDisablesCredentialPrompts(t *testing.T) {
	// Without GIT_TERMINAL_PROMPT=0 one repository with stale credentials hangs
	// an unattended run over hundreds of repositories forever.
	env := NewExecRunner().environ()

	want := map[string]bool{
		"GIT_TERMINAL_PROMPT=0": false,
		"GCM_INTERACTIVE=never": false,
		"LC_ALL=C":              false,
	}
	for _, entry := range env {
		if _, ok := want[entry]; ok {
			want[entry] = true
		}
	}
	for entry, found := range want {
		if !found {
			t.Errorf("child environment is missing %s", entry)
		}
	}
}

func TestExecRunnerHonoursCancellation(t *testing.T) {
	requireGit(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := NewExecRunner().Run(ctx, "", []string{"version"}, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
}

func TestExecRunnerReportsAMissingExecutable(t *testing.T) {
	runner := &ExecRunner{Exe: "clonezip-no-such-executable"}
	if _, err := runner.Run(context.Background(), "", []string{"version"}, nil); err == nil {
		t.Fatal("running a non-existent executable succeeded")
	}
}

func TestFakeRunnerPrefersTheMostSpecificMatch(t *testing.T) {
	fake := NewFakeRunner().
		Respond("clone", Response{Code: 1}).
		Respond("clone --bare", Response{Code: 0})

	if code, err := fake.Run(context.Background(), "", []string{"clone", "--bare", "url", "dir"}, nil); code != 0 || err != nil {
		t.Errorf("bare clone matched the shorter key: code %d, err %v", code, err)
	}
	if code, _ := fake.Run(context.Background(), "", []string{"clone", "src", "dst"}, nil); code != 1 {
		t.Errorf("plain clone code = %d, want 1", code)
	}
}
