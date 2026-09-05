package cli

import (
	"context"
	"os"

	"github.com/spf13/cobra"

	"github.com/heilingbrunner/clonezip/internal/common/doctorx"
)

// doctorOptions holds the flags of the doctor command.
type doctorOptions struct {
	global *globalOptions

	// Probe is a repo-list file or a single repository URL. Empty means the
	// auth check is skipped: it is the only check that cannot be answered
	// locally, so it needs somewhere to connect to.
	Probe  string
	Out    string
	JSON   bool
	Strict bool
}

func newDoctorCmd(global *globalOptions) *cobra.Command {
	opts := &doctorOptions{global: global}

	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check that git, git-lfs and the environment are ready",
		Long: "Reports whether everything clonezip needs is in place: git, git-lfs and its\n" +
			"filters, writable output with enough free space, and the platform\n" +
			"specifics that bite during long runs.\n\n" +
			"Every check is local except the authentication probe, which needs a URL\n" +
			"to connect to - pass --probe with a repo list or a single URL to include\n" +
			"it. 7-Zip is reported for information only; clonezip writes ZIP archives\n" +
			"itself and never shells out to it.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDoctor(cmd.Context(), opts)
		},
	}

	f := cmd.Flags()
	f.StringVar(&opts.Probe, "probe", "",
		"repo list or URL to test authentication against (one request per distinct host)")
	f.StringVar(&opts.Out, "out", ".", "directory to check for writability and free space")
	f.BoolVar(&opts.JSON, "json", false, "emit machine-readable results")
	f.BoolVar(&opts.Strict, "strict", false, "treat warnings as failures")

	return cmd
}

func runDoctor(ctx context.Context, opts *doctorOptions) error {
	report := doctorx.Run(ctx, Version, doctorx.Options{
		Out:    opts.Out,
		Probe:  opts.Probe,
		Strict: opts.Strict,
	})

	if opts.JSON {
		if err := doctorx.RenderJSON(os.Stdout, report); err != nil {
			return err
		}
	} else {
		doctorx.Render(os.Stdout, report)
	}

	// A failing check is the answer, not an error: doctor did its job either way, so
	// it reports through the exit code rather than through a message.
	if report.Failed() {
		return &FailureError{Count: report.Count(doctorx.StatusFail)}
	}
	return nil
}
