package gitx

import (
	"context"
	"strings"
	"sync"
	"time"
)

// Response is what a FakeRunner replies to one matched command.
type Response struct {
	Code   int
	Stdout []string
	Stderr []string

	// Delay simulates a slow command, for testing timeouts and concurrency.
	Delay time.Duration
}

// Call records one invocation, so a test can assert what git was actually asked
// to do - including that a failing step stopped the ones that follow it.
type Call struct {
	Dir  string
	Args []string
}

// Joined renders the arguments the way FakeRunner matches them.
func (c Call) Joined() string { return strings.Join(c.Args, " ") }

// FakeRunner is a Runner that replays canned responses.
//
// It lives outside a _test.go file on purpose: the pipeline tests need it, and
// exporting it here is what lets the whole clone-and-archive sequence be tested
// with no git binary, no network and no Azure DevOps account.
type FakeRunner struct {
	// Responses is keyed by a substring of the joined arguments. The longest
	// matching key wins, so "clone --bare" can be given a different answer from
	// "clone".
	Responses map[string]Response

	// Default answers anything unmatched. Its zero value means success with no
	// output, which keeps a test focused on the commands it cares about.
	Default Response

	mu    sync.Mutex
	calls []Call
}

// NewFakeRunner returns a FakeRunner that succeeds silently at everything.
func NewFakeRunner() *FakeRunner {
	return &FakeRunner{Responses: map[string]Response{}}
}

// Respond registers a response for commands whose joined arguments contain match.
func (f *FakeRunner) Respond(match string, r Response) *FakeRunner {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Responses == nil {
		f.Responses = map[string]Response{}
	}
	f.Responses[match] = r
	return f
}

// Fail registers a failing response, as git would report it on stderr.
func (f *FakeRunner) Fail(match string, code int, stderr ...string) *FakeRunner {
	return f.Respond(match, Response{Code: code, Stderr: stderr})
}

// Run implements Runner.
func (f *FakeRunner) Run(ctx context.Context, dir string, args []string, sink LineSink) (int, error) {
	f.mu.Lock()
	f.calls = append(f.calls, Call{Dir: dir, Args: args})
	f.mu.Unlock()

	resp := f.lookup(strings.Join(args, " "))

	if resp.Delay > 0 {
		select {
		case <-time.After(resp.Delay):
		case <-ctx.Done():
			return -1, ctx.Err()
		}
	}
	if err := ctx.Err(); err != nil {
		return -1, err
	}

	if sink != nil {
		for _, line := range resp.Stdout {
			sink(StdOut, line)
		}
		for _, line := range resp.Stderr {
			sink(StdErr, line)
		}
	}

	if resp.Code != 0 {
		return resp.Code, &ExitError{Args: args, Code: resp.Code, Tail: resp.Stderr}
	}
	return 0, nil
}

// lookup finds the most specific registered response for a command.
func (f *FakeRunner) lookup(joined string) Response {
	f.mu.Lock()
	defer f.mu.Unlock()

	best, bestLen := f.Default, -1
	for match, resp := range f.Responses {
		if strings.Contains(joined, match) && len(match) > bestLen {
			best, bestLen = resp, len(match)
		}
	}
	return best
}

// Calls returns the invocations so far, in order.
func (f *FakeRunner) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Call, len(f.calls))
	copy(out, f.calls)
	return out
}

// CallsMatching returns the invocations whose joined arguments contain match.
func (f *FakeRunner) CallsMatching(match string) []Call {
	var out []Call
	for _, call := range f.Calls() {
		if strings.Contains(call.Joined(), match) {
			out = append(out, call)
		}
	}
	return out
}

// Reset clears the recorded calls, keeping the registered responses.
func (f *FakeRunner) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}
