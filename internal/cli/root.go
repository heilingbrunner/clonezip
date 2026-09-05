// Package cli wires the clonezip commands together and maps their errors onto
// process exit codes.
package cli

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
)

// Version is the clonezip version, reported by --version and recorded in every archive
// manifest.
//
// A variable rather than a constant, and assigned from main: the build injects the
// value with -ldflags "-X main.version=…", which rewrites a variable's data symbol at
// link time and therefore cannot target a constant. "dev" is what a plain `go build` or
// `go run` reports; make overrides it with VERSION from makefile.project.mk.
var Version = "dev"

// copyrightNotice is appended under the version in --version output.
const copyrightNotice = "Copyright (c) 2026 heilingbrunner — MIT License"

// errInterrupted is returned when the run was cancelled via Ctrl-C.
var errInterrupted = errors.New("interrupted")

// globalOptions holds the flags shared by every command.
type globalOptions struct {
	NoTUI   bool
	Verbose bool
	ASCII   bool
	Unicode bool
}

// Main runs clonezip with the given arguments and returns the process exit code.
func Main(args []string) int {
	// Nothing may write to stdout behind the TUI's back. Any library that
	// reaches for the standard logger is silenced here rather than corrupting
	// the rendered view.
	log.SetOutput(io.Discard)

	// One context for the whole process, threaded into bubbletea, the pipeline
	// and every child process, so Ctrl-C tears down git children instead of
	// orphaning them. SIGTERM matters just as much on Windows as on Linux: the
	// runtime maps closing the console window (or logoff/shutdown) to SIGTERM,
	// and without a handler registered for it Windows kills the process outright
	// with no grace period, letting an in-progress restore finish extracting
	// instead of being cancelled and cleaned up.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// started reports whether execution reached a command body. Cobra runs
	// every validation - unknown command, unparseable flag, argument count,
	// required flags, mutually exclusive groups - before it calls RunE, so this
	// stays false for every caller mistake.
	var started bool

	root, opts := newRootCmd()
	root.SetArgs(args)
	root.AddCommand(
		newBackupCmd(opts),
		newRestoreCmd(opts),
		newDoctorCmd(opts),
		newServiceCmd(opts),
		newMigrateCmd(opts),
	)
	markStarted(root, &started)

	err := root.ExecuteContext(ctx)

	switch {
	// A cancelled context surfaces as assorted wrapped errors depending on
	// where the run happened to be; normalise it to one sentinel.
	case err != nil && ctx.Err() != nil:
		err = errInterrupted

	// Cobra rejects an unknown command, an unparseable flag or the wrong
	// argument count before any command body runs, and reports those as plain
	// errors. Reaching a body is what separates "the caller made a mistake"
	// from "the work failed", so anything that fails earlier is a usage error.
	case err != nil && !started:
		err = &UsageError{Err: err}
	}
	return exitCode(err, os.Stderr)
}

// markStarted wraps every command body in the tree so that reaching one flips
// started. Cobra reports its own validation failures as plain errors, and this
// is what lets Main tell them apart from a genuine runtime failure.
func markStarted(cmd *cobra.Command, started *bool) {
	for _, child := range cmd.Commands() {
		markStarted(child, started)
	}
	run := cmd.RunE
	if run == nil {
		return
	}
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		*started = true
		return run(cmd, args)
	}
}

func newRootCmd() (*cobra.Command, *globalOptions) {
	opts := &globalOptions{}

	cmd := &cobra.Command{
		Use:   "clonezip",
		Short: "Back up Azure DevOps, GitHub, Codeberg and Bitbucket repositories as zipped bare clones",
		Long: "clonezip clones Git repositories (Azure DevOps, GitHub, Codeberg and Bitbucket) as bare\n" +
			"repos, fetches the Git-LFS objects referenced by every branch and tag tip\n" +
			"so each archive works offline, and zips them. It restores a single archive\n" +
			"back into a bare repo plus a ready-to-use working clone.",
		Version: Version,
		// Usage on every runtime error is noise, and errors are printed by
		// exitCode so the message and the exit status are decided together.
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	f := cmd.PersistentFlags()
	f.BoolVar(&opts.NoTUI, "no-tui", false, "plain line output instead of the interactive view")
	f.BoolVar(&opts.Verbose, "verbose", false, "include git output in the log stream")
	f.BoolVar(&opts.ASCII, "ascii", false, "force ASCII status words instead of unicode glyphs")
	f.BoolVar(&opts.Unicode, "unicode", false, "force unicode glyphs")

	cmd.MarkFlagsMutuallyExclusive("ascii", "unicode")
	cmd.SetVersionTemplate("{{.Name}} version {{.Version}}\n" + copyrightNotice + "\n")

	return cmd, opts
}
