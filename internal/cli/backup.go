package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/heilingbrunner/clonezip/internal/cli/ui"
	"github.com/heilingbrunner/clonezip/internal/common/doctorx"
	"github.com/heilingbrunner/clonezip/internal/common/gitx"
	"github.com/heilingbrunner/clonezip/internal/common/layout"
	"github.com/heilingbrunner/clonezip/internal/common/pipeline"
	"github.com/heilingbrunner/clonezip/internal/common/repolist"
)

// backupOptions holds the flags of the backup command.
type backupOptions struct {
	global *globalOptions

	ListPath   string
	Out        string
	Stage      string
	CloneDepth int
	Jobs       int
	Timeout    time.Duration
	Retries    int
	DryRun     bool
	FailFast   bool

	SkipChecks  bool
	Interactive bool
}

func newBackupCmd(global *globalOptions) *cobra.Command {
	opts := &backupOptions{global: global}

	cmd := &cobra.Command{
		Use:   "backup <repolist.yaml>",
		Short: "Clone every repository in a list and zip each as a bare archive",
		Long: "Reads a YAML file of repository URLs (Azure DevOps, GitHub, Codeberg, Bitbucket),\n" +
			"clones each one as a bare repository, fetches the Git-LFS objects\n" +
			"referenced by every branch and tag tip so the archive works without the\n" +
			"LFS server, and writes <out>/<project>/<repo>.git.zip. Bitbucket URLs use\n" +
			"the workspace as the project. GitHub and Codeberg URLs have no project, so\n" +
			"their archive is written straight to <out>/<repo>.git.zip.\n\n" +
			"Lines that are not URLs are treated as decorative labels and skipped.\n" +
			"An existing archive is overwritten.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.ListPath = args[0]
			if opts.Interactive {
				return runBackupInteractive(cmd.Context(), opts)
			}
			return runBackup(cmd.Context(), opts)
		},
	}

	f := cmd.Flags()
	f.IntVar(&opts.CloneDepth, "clone-depth", 0,
		"truncate history to N commits per ref (0 = full clone); a depth-limited backup is lossy")
	f.IntVar(&opts.Jobs, "jobs", 1, "repositories to process concurrently")
	f.StringVar(&opts.Out, "out", "",
		"output directory (default ./<yyyyMMdd-hhmmss>-<repolist name>)")
	f.StringVar(&opts.Stage, "stage", "", "staging directory for clones (default <out>/.stage)")
	f.DurationVar(&opts.Timeout, "timeout", 30*time.Minute, "per-repository timeout")
	f.IntVar(&opts.Retries, "retries", 2, "retries per repository on transient network errors")
	f.BoolVar(&opts.DryRun, "dry-run", false, "resolve and validate every entry without cloning")
	f.BoolVar(&opts.FailFast, "fail-fast", false, "stop the run on the first failed repository")
	f.BoolVar(&opts.SkipChecks, "skip-checks", false, "skip the git/lfs/disk/auth checks")
	f.BoolVarP(&opts.Interactive, "interactive", "i", false,
		"prompt for the options with an interactive form before running")

	return cmd
}

func (o *backupOptions) validate() error {
	switch {
	case o.Jobs < 1:
		return usagef("--jobs must be at least 1, got %d", o.Jobs)
	case o.CloneDepth < 0:
		return usagef("--clone-depth cannot be negative, got %d", o.CloneDepth)
	case o.Retries < 0:
		return usagef("--retries cannot be negative, got %d", o.Retries)
	case o.Timeout <= 0:
		return usagef("--timeout must be positive, got %s", o.Timeout)
	}
	return nil
}

func runBackup(ctx context.Context, opts *backupOptions) error {
	if err := opts.validate(); err != nil {
		return err
	}

	list, err := repolist.ParseFile(opts.ListPath)
	if err != nil {
		return usagef("read repo list: %w", err)
	}

	// A collision between two entries would silently lose one of them, so the
	// list has to be corrected before anything is cloned.
	if list.HasErrors() {
		problems := printer{w: os.Stderr}
		problems.printf("clonezip: %s cannot be used as it stands:\n", opts.ListPath)
		for _, issue := range list.Errors() {
			problems.printf("  %s\n", issue)
		}
		return &UsageError{Err: fmt.Errorf("%d problem(s) in the repo list", len(list.Errors()))}
	}
	if len(list.Entries) == 0 {
		return usagef("%s contains no repository URLs", opts.ListPath)
	}

	plan, err := layout.NewPlan(opts.ListPath, ".", opts.Out, opts.Stage, time.Now)
	if err != nil {
		return err
	}

	if opts.DryRun {
		printDryRun(os.Stdout, plan, list, opts)
		return nil
	}

	// Checked before anything is cloned. The authentication probe is what turns a
	// whole afternoon of silent failures into one failure in a few seconds, so the
	// list is passed to it: one request per distinct host.
	if !opts.SkipChecks {
		err := doctorx.Check(ctx, os.Stdout, doctorx.Options{
			Out:   plan.Out,
			Probe: opts.ListPath,
		}, doctorx.ChecksBackup)
		if err != nil {
			return &UsageError{Err: err}
		}
	}

	git := gitx.New(gitx.NewExecRunner())

	// Resolved once, for the provenance recorded in every archive. A failure here is
	// not worth stopping for: the versions are informational, and a genuinely missing
	// git is what the startup check is for.
	gitVersion, _ := git.Version(ctx)
	lfsVersion, _ := git.LFSVersion(ctx)

	var summary pipeline.Summary
	runErr := ui.Run(ctx,
		ui.SelectMode(os.Stdout, opts.global.NoTUI),
		os.Stdout,
		opts.global.Verbose,
		ui.PickGlyphs(opts.global.ASCII, opts.global.Unicode),
		func(runCtx context.Context, reporter pipeline.Reporter) error {
			var err error
			summary, err = pipeline.RunBackup(runCtx, pipeline.BackupOptions{
				Plan:        plan,
				List:        list,
				Git:         git,
				Depth:       opts.CloneDepth,
				Jobs:        opts.Jobs,
				Retries:     opts.Retries,
				Timeout:     opts.Timeout,
				FailFast:    opts.FailFast,
				ToolVersion: Version,
				GitVersion:  gitVersion,
				LFSVersion:  lfsVersion,
			}, reporter)
			return err
		})

	// The summary is printed whatever happened, including after a Ctrl-C: knowing
	// which of 300 repositories got done is the whole point of running it.
	ui.RenderSummary(os.Stdout, summary)

	if runErr != nil {
		return runErr
	}
	if n := len(summary.Failed()); n > 0 {
		return &FailureError{Count: n}
	}
	return nil
}

// printDryRun reports exactly what a run would do, without touching the network
// or the filesystem. This is what surfaces a duplicate entry or a name that
// cannot become a path, before a multi-hour run is started.
func printDryRun(w *os.File, plan layout.Plan, list repolist.List, opts *backupOptions) {
	out := printer{w: w}
	out.printf("clonezip backup  %s  (dry run)\n", filepath.Base(plan.ListPath))
	out.printf("out    %s\n", plan.Out)
	out.printf("stage  %s\n", plan.Stage)
	if opts.CloneDepth > 0 {
		out.printf("depth  %d  (history is truncated; a depth-limited backup is lossy)\n", opts.CloneDepth)
	}
	out.println()

	width := 0
	for _, e := range list.Entries {
		if n := len(e.Slug()); n > width {
			width = n
		}
	}
	for i, e := range list.Entries {
		archive, err := filepath.Rel(plan.Out, plan.ArchivePath(e.Project, e.Repo))
		if err != nil {
			archive = plan.ArchivePath(e.Project, e.Repo)
		}
		out.printf("  %04d  %-*s  -> %s\n", i, width, e.Slug(), archive)
	}

	out.printf("\n%d to clone", len(list.Entries))
	if n := list.Count(repolist.CodeDuplicate); n > 0 {
		out.printf(", %d duplicate(s) dropped", n)
	}
	if n := list.Count(repolist.CodeInsecure); n > 0 {
		out.printf(", %d over plain http", n)
	}
	out.println()

	// Notices are worth seeing in a dry run even though they do not stop it.
	for _, issue := range list.Issues {
		if issue.Code == repolist.CodeDuplicate || issue.Severity == repolist.SeverityWarning {
			out.printf("  %s\n", issue)
		}
	}
}
