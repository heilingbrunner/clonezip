package cli

import (
	"context"
	"fmt"
	"net"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
)

// Keys of the checkbox options in the form's "Options" multi-select, and the
// flag each one maps to on backupOptions. optSkipCheck is shared with
// restore_dialog.go: both commands expose the same --skip-checks flag.
const (
	optDryRun   = "dry-run"
	optFailFast = "fail-fast"
)

// backupDialogAnswers holds what the form collected, independent of huh's own
// field types, so applying it back onto backupOptions is a plain, testable
// step. The numeric/duration fields stay strings here - they are parsed only
// for validation inside the form; applyTo parses them again for real.
type backupDialogAnswers struct {
	ListPath   string
	Out        string
	Stage      string
	CloneDepth string
	Jobs       string
	Timeout    string
	Retries    string
	Options    []string // subset of optDryRun, optFailFast, optSkipCheck
}

func (a backupDialogAnswers) has(opt string) bool { return slices.Contains(a.Options, opt) }

// runBackupInteractive renders a form seeded from opts's already-parsed
// flags, lets the user edit every value, then runs the backup in-process
// with the merged result. Called from newBackupCmd's RunE when --interactive
// is set.
func runBackupInteractive(ctx context.Context, opts *backupOptions) error {
	if _, err := os.Stat(opts.ListPath); err != nil {
		return &UsageError{Err: fmt.Errorf("repo list not found: %w", err)}
	}

	answers := backupAnswersFromOptions(opts)

	if err := buildBackupForm(&answers).RunWithContext(ctx); err != nil {
		if err == huh.ErrUserAborted {
			out := printer{w: os.Stdout}
			out.println("Backup cancelled.")
			return nil
		}
		return fmt.Errorf("backup dialog: %w", err)
	}

	if err := answers.applyTo(opts); err != nil {
		return err
	}
	return runBackup(ctx, opts)
}

// backupAnswersFromOptions seeds every form field from the flags already
// parsed onto opts, so --interactive prefills all of them, not just --out.
func backupAnswersFromOptions(opts *backupOptions) backupDialogAnswers {
	a := backupDialogAnswers{
		ListPath:   opts.ListPath,
		Out:        opts.Out,
		Stage:      opts.Stage,
		CloneDepth: strconv.Itoa(opts.CloneDepth),
		Jobs:       strconv.Itoa(opts.Jobs),
		Timeout:    opts.Timeout.String(),
		Retries:    strconv.Itoa(opts.Retries),
	}
	if opts.DryRun {
		a.Options = append(a.Options, optDryRun)
	}
	if opts.FailFast {
		a.Options = append(a.Options, optFailFast)
	}
	if opts.SkipChecks {
		a.Options = append(a.Options, optSkipCheck)
	}
	return a
}

// applyTo writes the submitted form back onto opts. The numeric/duration
// fields were already validated by the form's own Validate funcs, so these
// reparses cannot fail in practice; the error is still surfaced defensively
// rather than ignored.
func (a backupDialogAnswers) applyTo(opts *backupOptions) error {
	depth, err := strconv.Atoi(a.CloneDepth)
	if err != nil {
		return fmt.Errorf("clone depth: %w", err)
	}
	jobs, err := strconv.Atoi(a.Jobs)
	if err != nil {
		return fmt.Errorf("jobs: %w", err)
	}
	timeout, err := time.ParseDuration(a.Timeout)
	if err != nil {
		return fmt.Errorf("timeout: %w", err)
	}
	retries, err := strconv.Atoi(a.Retries)
	if err != nil {
		return fmt.Errorf("retries: %w", err)
	}

	opts.ListPath = a.ListPath
	opts.Out = a.Out
	opts.Stage = a.Stage
	opts.CloneDepth = depth
	opts.Jobs = jobs
	opts.Timeout = timeout
	opts.Retries = retries
	opts.DryRun = a.has(optDryRun)
	opts.FailFast = a.has(optFailFast)
	opts.SkipChecks = a.has(optSkipCheck)
	return nil
}

func buildBackupForm(answers *backupDialogAnswers) *huh.Form {
	form := huh.NewForm(huh.NewGroup(
		huh.NewNote().Title("Backup with clonezip"),

		huh.NewInput().
			Title("Repo list (YAML)").
			Description("The list of repository URLs (Azure DevOps, GitHub, Codeberg, Bitbucket) to back up.").
			Value(&answers.ListPath).
			Validate(requiredPath("repo list")),

		huh.NewInput().
			Title("Output directory").
			Description("Blank uses the default: ./<yyyyMMdd-hhmmss>-<repolist name>").
			Value(&answers.Out),

		huh.NewInput().
			Title("Staging directory").
			Description("Blank uses the default: <output directory>/.stage").
			Value(&answers.Stage),

		huh.NewInput().
			Title("Clone depth").
			Description("0 = full clone; anything else truncates history and is lossy.").
			Value(&answers.CloneDepth).
			Validate(validateNonNegativeInt("clone depth")),

		huh.NewInput().
			Title("Concurrent jobs").
			Description("Repositories to process at the same time.").
			Value(&answers.Jobs).
			Validate(validatePositiveInt("jobs")),

		huh.NewInput().
			Title("Per-repository timeout").
			Description("A Go duration, e.g. 30m, 1h15m.").
			Value(&answers.Timeout).
			Validate(validatePositiveDuration("timeout")),

		huh.NewInput().
			Title("Retries per repository").
			Description("On transient network errors.").
			Value(&answers.Retries).
			Validate(validateNonNegativeInt("retries")),

		huh.NewMultiSelect[string]().
			Title("Options").
			Description("Space to toggle, enter to continue.").
			Options(
				huh.NewOption("Dry run (resolve and validate only, nothing cloned)", optDryRun),
				huh.NewOption("Fail fast (stop on the first failed repository)", optFailFast),
				huh.NewOption("Skip preflight checks", optSkipCheck),
			).
			Value(&answers.Options),
	))

	return form.WithTheme(dialogTheme()).WithLayout(dialogLayout{})
}

func requiredPath(label string) func(string) error {
	return func(s string) error {
		if s == "" {
			return fmt.Errorf("%s is required", label)
		}
		if _, err := os.Stat(s); err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
		return nil
	}
}

func validateNonNegativeInt(label string) func(string) error {
	return func(s string) error {
		n, err := strconv.Atoi(s)
		if err != nil {
			return fmt.Errorf("%s must be a whole number", label)
		}
		if n < 0 {
			return fmt.Errorf("%s cannot be negative", label)
		}
		return nil
	}
}

func validatePositiveInt(label string) func(string) error {
	return func(s string) error {
		n, err := strconv.Atoi(s)
		if err != nil {
			return fmt.Errorf("%s must be a whole number", label)
		}
		if n < 1 {
			return fmt.Errorf("%s must be at least 1", label)
		}
		return nil
	}
}

func validatePositiveDuration(label string) func(string) error {
	return func(s string) error {
		d, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("%s must be a duration like 30m or 1h15m", label)
		}
		if d <= 0 {
			return fmt.Errorf("%s must be positive", label)
		}
		return nil
	}
}

// bcp47Tag matches the shape of a language tag (language plus optional
// subtags); the browser's Intl is the real arbiter of what resolves. Kept in
// step with the same check the web API applies to a dashboard settings save.
var bcp47Tag = regexp.MustCompile(`^[A-Za-z]{2,8}(-[A-Za-z0-9]{1,8})*$`)

func validateLocale(label string) func(string) error {
	return func(s string) error {
		if s == "" {
			return fmt.Errorf("%s is required", label)
		}
		if !bcp47Tag.MatchString(s) {
			return fmt.Errorf("%s must be a locale tag such as en-US or de-DE", label)
		}
		return nil
	}
}

func validateListenAddr(label string) func(string) error {
	return func(s string) error {
		if s == "" {
			return fmt.Errorf("%s is required", label)
		}
		if _, _, err := net.SplitHostPort(s); err != nil {
			return fmt.Errorf("%s must be a host:port address such as :8091", label)
		}
		return nil
	}
}

func validateRequired(label string) func(string) error {
	return func(s string) error {
		if strings.TrimSpace(s) == "" {
			return fmt.Errorf("%s is required", label)
		}
		return nil
	}
}
