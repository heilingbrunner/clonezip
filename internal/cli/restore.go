package cli

import (
	"context"
	"errors"
	"os"

	"github.com/spf13/cobra"

	"github.com/heilingbrunner/clonezip/internal/cli/ui"
	"github.com/heilingbrunner/clonezip/internal/common/doctorx"
	"github.com/heilingbrunner/clonezip/internal/common/gitx"
	"github.com/heilingbrunner/clonezip/internal/common/pipeline"
	"github.com/heilingbrunner/clonezip/internal/common/ziparc"
)

// restoreOptions holds the flags of the restore command.
type restoreOptions struct {
	global *globalOptions

	ArchivePath    string
	Dest           string
	Origin         string
	Clone          bool
	Force          bool
	ConfirmShallow bool
	NoVerify       bool

	SkipChecks  bool
	Interactive bool

	// originSet distinguishes the default --origin from one the user typed, so asking
	// for an origin without a clone can be reported instead of silently ignored.
	originSet bool
}

func newRestoreCmd(global *globalOptions) *cobra.Command {
	opts := &restoreOptions{global: global}

	cmd := &cobra.Command{
		Use:   "restore <archive.git.zip>",
		Short: "Restore one archive into a bare repository, optionally with a working clone",
		Long: "Extracts an archive produced by backup into <repo>.git, which holds every\n" +
			"backed-up ref and is what you push to a new remote.\n\n" +
			"With --clone it also creates a working copy next to it, seeds the LFS\n" +
			"object cache from the bare so LFS-tracked files become real content, and\n" +
			"points origin back at the repository's original clone URL.\n\n" +
			"Accepts .git.zip and the legacy .git.7z archives written by the\n" +
			"PowerShell scripts, which were ZIP containers despite the extension.\n" +
			"Refuses to overwrite an existing <repo>.git, or an existing <repo> when\n" +
			"--clone is given.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.ArchivePath = args[0]
			opts.originSet = cmd.Flags().Changed("origin")
			if opts.Interactive {
				return runRestoreInteractive(cmd.Context(), opts)
			}
			return runRestore(cmd.Context(), opts)
		},
	}

	f := cmd.Flags()
	f.StringVar(&opts.Dest, "dest", ".", "directory to restore into")
	f.BoolVar(&opts.Clone, "clone", false,
		"also clone a working copy from the restored bare repository")
	f.StringVar(&opts.Origin, "origin", "source",
		"with --clone, what origin points at: source (the original clone URL) or bare (the local bare repo)")
	f.BoolVar(&opts.Force, "force", false, "replace an existing <repo>.git, or <repo> with --clone")
	f.BoolVar(&opts.ConfirmShallow, "confirm-shallow", false, "proceed with restoring a shallow/truncated archive")
	f.BoolVar(&opts.NoVerify, "no-verify", false, "skip git fsck and the manifest cross-check")
	f.BoolVar(&opts.SkipChecks, "skip-checks", false, "skip the git/lfs/disk checks")
	f.BoolVarP(&opts.Interactive, "interactive", "i", false,
		"prompt for the destination and flags with an interactive form before running")

	return cmd
}

func runRestore(ctx context.Context, opts *restoreOptions) error {
	switch opts.Origin {
	case pipeline.OriginSource, pipeline.OriginBare:
	default:
		return usagef("--origin must be %q or %q, got %q",
			pipeline.OriginSource, pipeline.OriginBare, opts.Origin)
	}

	// There is no origin to set without a working clone, and silently ignoring the flag
	// would leave the user believing they had configured something.
	if opts.originSet && !opts.Clone {
		return usagef("--origin only applies with --clone")
	}

	// The LFS filters matter here in a way they do not during backup: without them
	// git lfs checkout cannot turn pointer files into content, and the restore would
	// quietly produce stubs.
	if !opts.SkipChecks {
		err := doctorx.Check(ctx, os.Stdout, doctorx.Options{
			Out: opts.Dest,
		}, doctorx.ChecksRestore)
		if err != nil {
			return &UsageError{Err: err}
		}
	}

	reporter := ui.NewPlain(os.Stdout, opts.global.Verbose)

	result, err := pipeline.RunRestore(ctx, pipeline.RestoreOptions{
		ArchivePath:    opts.ArchivePath,
		Dest:           opts.Dest,
		Origin:         opts.Origin,
		Clone:          opts.Clone,
		Force:          opts.Force,
		ConfirmShallow: opts.ConfirmShallow,
		// Verifying one archive costs a few seconds and is the only chance to catch a
		// truncated backup before relying on it, so it is on unless refused.
		Verify: !opts.NoVerify,
		Git:    gitx.New(gitx.NewExecRunner()),
	}, reporter)

	if err != nil {
		// These all mean "do something and retry" rather than "the work went wrong",
		// so they get their own exit code.
		var formatErr *ziparc.FormatError
		switch {
		case errors.Is(err, pipeline.ErrTargetExists),
			errors.Is(err, pipeline.ErrShallowNeedsConfirm),
			errors.Is(err, pipeline.ErrNotAnArchive),
			errors.As(err, &formatErr):
			return &PreconditionError{Err: err}
		}
		return err
	}

	out := printer{w: os.Stdout}
	for _, line := range result.SummaryLines() {
		out.println(line)
	}
	if sample := result.DirtySample(); len(sample) > 0 {
		out.println("\nAlready modified in the fresh clone:")
		for _, line := range sample {
			out.printf("  %s\n", line)
		}
	}
	return nil
}
