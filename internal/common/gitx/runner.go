// Package gitx runs git. Every invocation goes through one wrapper that always
// inspects the exit status, so there is no code path that can ignore a failure -
// which is what the PowerShell script did for both git clone and 7-Zip, letting a
// failed clone be archived as though it had worked.
package gitx

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/heilingbrunner/clonezip/internal/common/procx"
)

// Stream identifies which of a child's output streams a line came from.
type Stream int

const (
	StdOut Stream = iota
	StdErr
)

// LineSink receives each line a git process writes, as it is written. It may be
// nil when the caller does not care.
type LineSink func(Stream, string)

// defaultTailLines is how much stderr is kept to explain a failure: enough to
// carry a git error plus the remote's message, not enough to matter for memory
// across concurrent jobs.
const defaultTailLines = 50

// ExitError reports a git command that exited non-zero. The tail turns an
// unhelpful "exit 128" into the remote's actual complaint.
type ExitError struct {
	Args []string
	Code int
	Tail []string
}

func (e *ExitError) Error() string {
	msg := fmt.Sprintf("git %s failed (exit %d)", strings.Join(e.Args, " "), e.Code)
	if reason := e.Reason(); reason != "" {
		msg += ": " + reason
	}
	return msg
}

// Reason is the most informative line of captured stderr, for a one-line report.
// git puts the fatal cause last and prefixes remote messages with "remote:", so
// those are preferred over progress noise.
func (e *ExitError) Reason() string {
	for i := len(e.Tail) - 1; i >= 0; i-- {
		line := strings.TrimSpace(e.Tail[i])
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "fatal:") || strings.HasPrefix(line, "error:") ||
			strings.HasPrefix(line, "remote:") {
			return line
		}
	}
	for i := len(e.Tail) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(e.Tail[i]); line != "" {
			return line
		}
	}
	return ""
}

// Runner executes a git command. It exists so the pipeline can be tested without
// a git binary and without a network.
type Runner interface {
	// Run executes git with args in dir, forwarding output lines to sink. It
	// returns the exit code, and an *ExitError when that code is non-zero.
	Run(ctx context.Context, dir string, args []string, sink LineSink) (int, error)
}

// ExecRunner runs the real git binary.
type ExecRunner struct {
	// Exe is the git executable; empty means "git" from PATH.
	Exe string

	// Env adds or overrides environment variables for the child.
	Env []string

	// TailLines overrides how many stderr lines are kept for ExitError.
	TailLines int
}

// NewExecRunner returns a runner for the git binary on PATH.
func NewExecRunner() *ExecRunner { return &ExecRunner{} }

func (r *ExecRunner) exe() string {
	if r.Exe != "" {
		return r.Exe
	}
	return "git"
}

// Run executes one git command.
//
// The child never inherits stdout or stderr: every line is captured and handed to
// the sink instead. That is what lets the interactive view render without a git
// subprocess scribbling over it, and what makes the run log complete.
func (r *ExecRunner) Run(ctx context.Context, dir string, args []string, sink LineSink) (int, error) {
	// No shell anywhere: repository names contain dots, spaces and ampersands,
	// and going through cmd.exe or a POSIX shell would make those a quoting
	// hazard for no benefit.
	cmd := exec.Command(r.exe(), args...)
	cmd.Dir = dir
	cmd.Env = r.environ()

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return -1, fmt.Errorf("git stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return -1, fmt.Errorf("git stderr pipe: %w", err)
	}

	tree, err := procx.Start(cmd)
	if err != nil {
		return -1, fmt.Errorf("start git: %w", err)
	}
	defer tree.Close()

	// Cancellation kills the whole tree, not just git: an orphaned git-lfs would
	// keep a handle on the staging directory and block cleanup.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			tree.Kill()
		case <-done:
		}
	}()

	tail := newRing(r.tailLines())
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		scan(stdout, StdOut, sink, nil)
	}()
	go func() {
		defer wg.Done()
		scan(stderr, StdErr, sink, tail)
	}()
	wg.Wait()

	waitErr := cmd.Wait()

	// Report cancellation as such, rather than as whatever exit status the kill
	// happened to produce.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return -1, ctxErr
	}

	code := 0
	if waitErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(waitErr, &exitErr) {
			return -1, fmt.Errorf("run git %s: %w", strings.Join(args, " "), waitErr)
		}
		code = exitErr.ExitCode()
	}
	if code != 0 {
		return code, &ExitError{Args: args, Code: code, Tail: tail.lines()}
	}
	return 0, nil
}

// environ builds the child environment.
func (r *ExecRunner) environ() []string {
	env := os.Environ()

	// GIT_TERMINAL_PROMPT and GCM_INTERACTIVE are the difference between a
	// stale-credential repository failing in a second and hanging an unattended
	// run over 300 repositories forever. LC_ALL keeps stderr parseable.
	env = append(env,
		"GIT_TERMINAL_PROMPT=0",
		"GCM_INTERACTIVE=never",
		"LC_ALL=C",
	)
	return append(env, r.Env...)
}

func (r *ExecRunner) tailLines() int {
	if r.TailLines > 0 {
		return r.TailLines
	}
	return defaultTailLines
}

// scan reads one stream, splitting on newlines and carriage returns, and forwards
// every line to sink and optionally to a ring buffer.
func scan(rc io.Reader, stream Stream, sink LineSink, tail *ring) {
	sc := bufio.NewScanner(rc)
	// Progress lines get long once a remote pads them; the default 64 KiB is
	// ample, but set it explicitly so a pathological line cannot abort the scan.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	sc.Split(splitLinesOrCR)

	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), " \t")
		if line == "" {
			continue
		}
		if tail != nil {
			tail.add(line)
		}
		if sink != nil {
			sink(stream, line)
		}
	}
	// A scan error here means the pipe broke, which cmd.Wait reports properly;
	// there is nothing to add by surfacing it twice.
}

// splitLinesOrCR splits on "\n", "\r\n" and a bare "\r".
//
// git writes progress with carriage returns and no newline, so a plain line
// scanner yields one enormous line at EOF and no progress at all until the clone
// has finished. Splitting on "\r" as well is what turns "Receiving objects: 45%"
// into something that can be shown while it is still true.
func splitLinesOrCR(data []byte, atEOF bool) (int, []byte, error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		// A trailing "\r" might be the first half of "\r\n"; wait for the next
		// read rather than emitting a spurious empty line.
		if data[i] == '\r' && i == len(data)-1 && !atEOF {
			return 0, nil, nil
		}
		advance := i + 1
		if data[i] == '\r' && i+1 < len(data) && data[i+1] == '\n' {
			advance = i + 2
		}
		return advance, data[:i], nil
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// ring keeps the last n lines. It is written from the stderr scanner goroutine and
// read after Wait, so it guards itself.
type ring struct {
	mu   sync.Mutex
	buf  []string
	next int
	full bool
}

func newRing(n int) *ring {
	if n < 1 {
		n = 1
	}
	return &ring{buf: make([]string, n)}
}

func (r *ring) add(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf[r.next] = line
	r.next = (r.next + 1) % len(r.buf)
	if r.next == 0 {
		r.full = true
	}
}

// lines returns the retained lines in the order they were written.
func (r *ring) lines() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.full {
		out := make([]string, r.next)
		copy(out, r.buf[:r.next])
		return out
	}
	out := make([]string, 0, len(r.buf))
	out = append(out, r.buf[r.next:]...)
	return append(out, r.buf[:r.next]...)
}
