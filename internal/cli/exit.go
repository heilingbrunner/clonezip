package cli

import (
	"errors"
	"fmt"
	"io"
)

// Exit codes. The PowerShell scripts this tool replaces used a bare `exit`,
// which returns 0 and makes every failure invisible to callers. Each code below
// is distinct so a scheduled task can tell "nothing was attempted" apart from
// "some repos failed".
const (
	ExitOK           = 0   // completed, zero failures (warnings allowed)
	ExitFailures     = 1   // completed, at least one repo failed
	ExitUsage        = 2   // usage, list validation or startup check failure - nothing attempted
	ExitPrecondition = 3   // precondition (not a zip, target dir exists)
	ExitInterrupted  = 130 // aborted by Ctrl-C, shell convention
)

// printer writes user-facing text.
//
// A failed write to stdout or stderr has nowhere left to be reported, so the error is
// discarded deliberately, in one place, rather than at every call site.
type printer struct{ w io.Writer }

func (p printer) printf(format string, args ...any) {
	_, _ = fmt.Fprintf(p.w, format, args...)
}

func (p printer) println(args ...any) {
	_, _ = fmt.Fprintln(p.w, args...)
}

// UsageError marks an error as a caller mistake rather than a runtime failure.
type UsageError struct{ Err error }

func (e *UsageError) Error() string { return e.Err.Error() }
func (e *UsageError) Unwrap() error { return e.Err }

func usagef(format string, a ...any) error {
	return &UsageError{Err: fmt.Errorf(format, a...)}
}

// PreconditionError marks a failure detected before any work was attempted that
// the user must resolve (an existing target directory, a non-zip archive).
type PreconditionError struct{ Err error }

func (e *PreconditionError) Error() string { return e.Err.Error() }
func (e *PreconditionError) Unwrap() error { return e.Err }

// FailureError reports that the run itself completed but some units failed.
// It carries no message of its own: the summary has already been rendered.
type FailureError struct{ Count int }

func (e *FailureError) Error() string {
	return fmt.Sprintf("%d unit(s) failed", e.Count)
}

// ConfigInvalidError reports that a service config failed validation. It
// carries no message of its own: printConfigErrors has already listed every
// problem before this is returned.
type ConfigInvalidError struct{}

func (e *ConfigInvalidError) Error() string { return "invalid config" }

// exitCode maps an error returned by a command to a process exit code, and
// writes a message for the cases that have not already reported themselves.
func exitCode(err error, stderr io.Writer) int {
	if err == nil {
		return ExitOK
	}

	out := printer{w: stderr}

	if errors.Is(err, errInterrupted) {
		out.println("aborted")
		return ExitInterrupted
	}

	// The summary already listed every failure; do not repeat it here.
	if _, ok := errors.AsType[*FailureError](err); ok {
		return ExitFailures
	}

	// printConfigErrors already listed every problem; do not repeat it here.
	if _, ok := errors.AsType[*ConfigInvalidError](err); ok {
		return ExitUsage
	}

	if usage, ok := errors.AsType[*UsageError](err); ok {
		out.printf("clonezip: %v\n", usage.Err)
		return ExitUsage
	}

	if precondition, ok := errors.AsType[*PreconditionError](err); ok {
		out.printf("clonezip: %v\n", precondition.Err)
		return ExitPrecondition
	}

	out.printf("clonezip: %v\n", err)
	return ExitFailures
}
