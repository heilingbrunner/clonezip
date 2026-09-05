package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"time"

	"github.com/charmbracelet/huh"
	"go.yaml.in/yaml/v3"

	"github.com/heilingbrunner/clonezip/internal/service"
)

// serviceInitDialogAnswers holds what the form collected, independent of
// huh's own field types, so applying it back onto a service.Config is a
// plain, testable step. The numeric/duration fields stay strings here - they
// are parsed only for validation inside the form; applyTo parses them again
// for real. Options reuses optFailFast/optSkipCheck from the backup and
// restore dialogs: they name the same two settings.
type serviceInitDialogAnswers struct {
	Title        string
	DateFormat   string
	Listen       string
	Out          string
	CloneDepth   string
	Jobs         string
	Retries      string
	Timeout      string
	HistoryLimit string
	Options      []string // subset of optFailFast, optSkipCheck
}

func (a serviceInitDialogAnswers) has(opt string) bool { return slices.Contains(a.Options, opt) }

// runServiceInitInteractive renders a form seeded from the embedded starter
// template, then writes the edited config to path. Called from
// newServiceInitCmd's RunE when --interactive is set.
//
// The template's example groups are carried through untouched: init only ever
// produces a starting point, and a config with no groups will not load.
func runServiceInitInteractive(ctx context.Context, w io.Writer, path string) error {
	exists, err := serviceConfigExists(w, path)
	if err != nil || exists {
		return err
	}

	var cfg service.Config
	if err := yaml.Unmarshal(serviceConfigTemplate, &cfg); err != nil {
		return fmt.Errorf("parse service config template: %w", err)
	}

	answers := serviceInitAnswersFromConfig(cfg)

	if err := buildServiceInitForm(&answers).RunWithContext(ctx); err != nil {
		if err == huh.ErrUserAborted {
			out := printer{w: os.Stdout}
			out.println("Service config cancelled.")
			return nil
		}
		return fmt.Errorf("service init dialog: %w", err)
	}

	if err := answers.applyTo(&cfg); err != nil {
		return err
	}
	if err := cfg.Save(path); err != nil {
		return err
	}

	_, err = fmt.Fprintf(w, "clonezip: wrote %s\n", path)
	return err
}

// serviceInitAnswersFromConfig seeds every form field from the template, so
// the prefilled values and the ones a plain "service init" would write stay
// the same set - there is no second copy of the defaults to keep in step.
func serviceInitAnswersFromConfig(cfg service.Config) serviceInitDialogAnswers {
	d := cfg.Defaults
	a := serviceInitDialogAnswers{
		Title:        cfg.Title,
		DateFormat:   cfg.DateFormat,
		Listen:       cfg.Listen,
		Out:          cfg.Out,
		CloneDepth:   strconv.Itoa(d.CloneDepth),
		Jobs:         strconv.Itoa(d.Jobs),
		Retries:      strconv.Itoa(d.Retries),
		Timeout:      d.Timeout.String(),
		HistoryLimit: strconv.Itoa(d.HistoryLimit),
	}
	if d.FailFast {
		a.Options = append(a.Options, optFailFast)
	}
	if d.SkipChecks {
		a.Options = append(a.Options, optSkipCheck)
	}
	return a
}

// applyTo writes the submitted form back onto cfg. The numeric/duration
// fields were already validated by the form's own Validate funcs, so these
// reparses cannot fail in practice; the error is still surfaced defensively
// rather than ignored.
//
// cfg.Groups and the retired cfg.Defaults.Out are deliberately left alone:
// the form does not cover groups, and writing anything into Defaults.Out
// would make the file this very command produces fail to load.
func (a serviceInitDialogAnswers) applyTo(cfg *service.Config) error {
	depth, err := strconv.Atoi(a.CloneDepth)
	if err != nil {
		return fmt.Errorf("clone depth: %w", err)
	}
	jobs, err := strconv.Atoi(a.Jobs)
	if err != nil {
		return fmt.Errorf("jobs: %w", err)
	}
	retries, err := strconv.Atoi(a.Retries)
	if err != nil {
		return fmt.Errorf("retries: %w", err)
	}
	timeout, err := time.ParseDuration(a.Timeout)
	if err != nil {
		return fmt.Errorf("timeout: %w", err)
	}
	limit, err := strconv.Atoi(a.HistoryLimit)
	if err != nil {
		return fmt.Errorf("history limit: %w", err)
	}

	cfg.Title = a.Title
	cfg.DateFormat = a.DateFormat
	cfg.Listen = a.Listen
	cfg.Out = a.Out
	cfg.Defaults.CloneDepth = depth
	cfg.Defaults.Jobs = jobs
	cfg.Defaults.Retries = retries
	cfg.Defaults.Timeout = timeout
	cfg.Defaults.HistoryLimit = limit
	cfg.Defaults.FailFast = a.has(optFailFast)
	cfg.Defaults.SkipChecks = a.has(optSkipCheck)
	return nil
}

func buildServiceInitForm(answers *serviceInitDialogAnswers) *huh.Form {
	form := huh.NewForm(huh.NewGroup(
		huh.NewNote().Title("New clonezip service config"),

		huh.NewInput().
			Title("Dashboard title").
			Description("Shown in the web page header; useful for telling instances apart.").
			Value(&answers.Title),

		huh.NewInput().
			Title("Date format").
			Description("A locale tag the dashboard renders run timestamps with, e.g. en-US, de-DE.").
			Value(&answers.DateFormat).
			Validate(validateLocale("date format")),

		huh.NewInput().
			Title("Listen address").
			Description("Where the dashboard is served, e.g. :8091 or 127.0.0.1:8091.").
			Value(&answers.Listen).
			Validate(validateListenAddr("listen address")),

		huh.NewInput().
			Title("Backup root").
			Description("Every group writes under here, relative to the config file.").
			Value(&answers.Out).
			Validate(validateRequired("backup root")),

		huh.NewNote().Title("Defaults for every group"),

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
			Title("Retries per repository").
			Description("On transient network errors.").
			Value(&answers.Retries).
			Validate(validateNonNegativeInt("retries")),

		huh.NewInput().
			Title("Per-repository timeout").
			Description("A Go duration, e.g. 30m, 1h15m.").
			Value(&answers.Timeout).
			Validate(validatePositiveDuration("timeout")),

		huh.NewInput().
			Title("History limit").
			Description("Past runs kept per group, in the dashboard and as log files.").
			Value(&answers.HistoryLimit).
			Validate(validatePositiveInt("history limit")),

		huh.NewMultiSelect[string]().
			Title("Options").
			Description("Space to toggle, enter to continue.").
			Options(
				huh.NewOption("Fail fast (stop on the first failed repository)", optFailFast),
				huh.NewOption("Skip preflight checks", optSkipCheck),
			).
			Value(&answers.Options),
	))

	return form.WithTheme(dialogTheme()).WithLayout(dialogLayout{})
}
