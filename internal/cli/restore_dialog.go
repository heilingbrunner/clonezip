package cli

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"

	"github.com/heilingbrunner/clonezip/internal/common/layout"
	"github.com/heilingbrunner/clonezip/internal/common/manifest"
	"github.com/heilingbrunner/clonezip/internal/common/pipeline"
	"github.com/heilingbrunner/clonezip/internal/common/ziparc"
)

// Keys of the checkbox options in the form's "Options" multi-select, and the
// flag each one maps to on restoreOptions.
const (
	optClone          = "clone"
	optForce          = "force"
	optConfirmShallow = "confirm-shallow"
	optNoVerify       = "no-verify"
	optSkipCheck      = "skip-checks"
)

// restoreDialogAnswers holds what the form collected, independent of huh's
// own field types, so applying it back onto restoreOptions is a plain,
// testable step.
type restoreDialogAnswers struct {
	Dest    string
	Origin  string
	Options []string // subset of optClone, optForce, optConfirmShallow, optNoVerify, optSkipCheck
}

func (a restoreDialogAnswers) has(opt string) bool { return slices.Contains(a.Options, opt) }

// runRestoreInteractive renders a form seeded from opts's already-parsed
// flags, lets the user edit every value, then runs the restore in-process
// with the merged result. Called from newRestoreCmd's RunE when
// --interactive is set.
func runRestoreInteractive(ctx context.Context, opts *restoreOptions) error {
	if _, err := os.Stat(opts.ArchivePath); err != nil {
		return &UsageError{Err: fmt.Errorf("archive not found: %w", err)}
	}

	repo, _ := layout.RepoNameFromArchive(opts.ArchivePath)
	man := readManifestBestEffort(opts.ArchivePath)

	answers := restoreAnswersFromOptions(opts, man.Truncated())

	if err := buildRestoreForm(opts.ArchivePath, repo, man, &answers).RunWithContext(ctx); err != nil {
		if err == huh.ErrUserAborted {
			out := printer{w: os.Stdout}
			out.println("Restore cancelled.")
			return nil
		}
		return fmt.Errorf("restore dialog: %w", err)
	}

	answers.applyTo(opts)
	return runRestore(ctx, opts)
}

// restoreAnswersFromOptions seeds every form field from the flags already
// parsed onto opts, so --interactive prefills all of them, not just --dest.
// truncated additionally pre-checks "confirm truncated/shallow history" when
// the archive itself needs it, mirroring the old dialog's UX nudge - the
// user can still uncheck it before submitting.
func restoreAnswersFromOptions(opts *restoreOptions, truncated bool) restoreDialogAnswers {
	a := restoreDialogAnswers{
		Dest:   opts.Dest,
		Origin: opts.Origin,
	}
	if opts.Clone {
		a.Options = append(a.Options, optClone)
	}
	if opts.Force {
		a.Options = append(a.Options, optForce)
	}
	if opts.ConfirmShallow || truncated {
		a.Options = append(a.Options, optConfirmShallow)
	}
	if opts.NoVerify {
		a.Options = append(a.Options, optNoVerify)
	}
	if opts.SkipChecks {
		a.Options = append(a.Options, optSkipCheck)
	}
	return a
}

// applyTo writes the submitted form back onto opts. Origin is only ever
// meaningful together with Clone, and the form always shows a definite
// Origin choice regardless of whether the box is checked, so originSet is
// tied to the form's own Clone answer rather than carried over from the
// CLI's --origin flag: that keeps "--origin only applies with --clone" from
// ever spuriously tripping for the interactive path.
func (a restoreDialogAnswers) applyTo(opts *restoreOptions) {
	opts.Dest = a.Dest
	opts.Origin = a.Origin
	opts.Clone = a.has(optClone)
	opts.originSet = opts.Clone
	opts.Force = a.has(optForce)
	opts.ConfirmShallow = a.has(optConfirmShallow)
	opts.NoVerify = a.has(optNoVerify)
	opts.SkipChecks = a.has(optSkipCheck)
}

// readManifestBestEffort returns the archive's manifest for prefilling the form,
// or nil if it cannot be read - never fatal here, since runRestore reports the
// same problem authoritatively once it runs for real.
func readManifestBestEffort(archivePath string) *manifest.Manifest {
	archive, err := ziparc.Open(archivePath)
	if err != nil {
		return nil
	}
	defer func() { _ = archive.Close() }()

	data, err := archive.ReadFile(manifest.EntryName)
	if err != nil {
		return nil
	}
	man, err := manifest.Parse(data)
	if err != nil {
		return nil
	}
	return man
}

// dialogBorder is the outer box drawn around every dialog form. huh sizes a
// group's content to the full terminal width and then wraps it in this style
// without reserving room for it, so its horizontal frame size must be
// subtracted back out via dialogLayout - otherwise the box runs past the
// terminal's right edge and only the left border is ever visible.
var dialogBorder = lipgloss.NewStyle().
	Border(lipgloss.RoundedBorder()).
	BorderForeground(lipgloss.Color("212")).
	Padding(1, 2)

// dialogTheme renders the form as a bordered box with checkbox-style
// "[ ] "/"[x] " options, instead of huh's default button-style confirms.
func dialogTheme() *huh.Theme {
	t := huh.ThemeCharm()
	t.Form.Base = dialogBorder

	checked := lipgloss.NewStyle().Foreground(lipgloss.Color("212")).SetString("[x] ")
	unchecked := lipgloss.NewStyle().Foreground(lipgloss.Color("240")).SetString("[ ] ")
	t.Focused.SelectedPrefix = checked
	t.Focused.UnselectedPrefix = unchecked
	t.Blurred.SelectedPrefix = checked
	t.Blurred.UnselectedPrefix = unchecked

	return t
}

// dialogLayout shrinks the group width by dialogBorder's horizontal frame
// size before huh's default layout fills it, so the bordered box that then
// wraps the group fits inside the terminal instead of overflowing it.
type dialogLayout struct{}

func (dialogLayout) View(f *huh.Form) string { return huh.LayoutDefault.View(f) }

func (dialogLayout) GroupWidth(f *huh.Form, g *huh.Group, w int) int {
	w -= dialogBorder.GetHorizontalFrameSize()
	if w < 0 {
		w = 0
	}
	return huh.LayoutDefault.GroupWidth(f, g, w)
}

// escapeNoteText escapes the characters huh's Note field treats as inline
// markdown (backslash, underscore, asterisk, backtick) so that arbitrary
// values - archive paths, repo names, source URLs - display literally
// instead of being interpreted as formatting. Without this, huh's renderer
// treats '\' purely as an escape character and drops it, silently eating
// every backslash in a Windows path.
func escapeNoteText(s string) string {
	r := strings.NewReplacer(
		`\`, `\\`,
		`_`, `\_`,
		`*`, `\*`,
		"`", "\\`",
	)
	return r.Replace(s)
}

func buildRestoreForm(archivePath, repo string, man *manifest.Manifest, answers *restoreDialogAnswers) *huh.Form {
	description := "Archive: " + escapeNoteText(archivePath)
	if repo != "" {
		description += "\nRepository: " + escapeNoteText(repo)
	}
	if man != nil && man.Source.URL != "" {
		description += "\nSource: " + escapeNoteText(man.Source.URL)
	}

	optionsDescription := "Space to toggle, enter to continue."
	if man.Truncated() {
		optionsDescription = "This archive has truncated history - \"Confirm truncated/shallow history\" " +
			"must be checked to proceed.\n" + optionsDescription
	}

	form := huh.NewForm(huh.NewGroup(
		huh.NewNote().Title("Restore with clonezip").Description(description),

		huh.NewInput().
			Title("Destination directory").
			Description("Where <repo>.git (and <repo>, if cloning) will be created.").
			Value(&answers.Dest).
			Validate(func(s string) error {
				if s == "" {
					return fmt.Errorf("destination directory is required")
				}
				return nil
			}),

		huh.NewSelect[string]().
			Title("Remote origin (with --clone)").
			Options(
				huh.NewOption("Source URL", pipeline.OriginSource),
				huh.NewOption("Local bare path", pipeline.OriginBare),
			).
			Value(&answers.Origin),

		huh.NewMultiSelect[string]().
			Title("Options").
			Description(optionsDescription).
			Options(
				huh.NewOption("Clone a working copy", optClone),
				huh.NewOption("Force overwrite of an existing target", optForce),
				huh.NewOption("Confirm truncated/shallow history", optConfirmShallow),
				huh.NewOption("Skip verification (git fsck)", optNoVerify),
				huh.NewOption("Skip preflight checks", optSkipCheck),
			).
			Value(&answers.Options),
	))

	return form.WithTheme(dialogTheme()).WithLayout(dialogLayout{})
}
