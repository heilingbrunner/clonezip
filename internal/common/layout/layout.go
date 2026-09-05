// Package layout derives every path a backup run uses, up front and without
// touching the filesystem, so a dry run can show exactly what would be written
// and tests can assert the shape of a run without performing one.
package layout

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/heilingbrunner/clonezip/internal/common/safepath"
)

// Naming of the pieces a run produces.
const (
	// ArchiveSuffix is appended to the repository name. The archives the
	// PowerShell script wrote were named ".git.7z" but were plain ZIP
	// containers, because it called 7-Zip with -tzip. New archives say what
	// they are; restore still accepts the old name.
	ArchiveSuffix = ".git.zip"

	// LegacyArchiveSuffix is the name used by the PowerShell script.
	LegacyArchiveSuffix = ".git.7z"

	// BareSuffix is appended when restoring, giving <repo>.git next to <repo>.
	BareSuffix = ".git"

	// StageDirName holds the in-progress clones, removed when the run ends.
	StageDirName = ".stage"

	// FailuresDirName collects the captured stderr of failed repositories.
	FailuresDirName = "_failures"

	// outDirDateLayout stamps the run directory: 20260727-164211-azuredevops-contoso.
	outDirDateLayout = "20060102-150405"

	// logFileDateLayout stamps the run log: clonezip-20260727-164211.log.
	logFileDateLayout = "20060102-150405"

	// maxSlugLen bounds each half of a staging directory name. The name is
	// already unique through its index, so this only keeps paths short.
	maxSlugLen = 40
)

// Clock supplies the current time. It is injected rather than called directly so
// that the run directory name is deterministic under test.
type Clock func() time.Time

// Plan is the set of resolved directories for one run. All paths are absolute:
// Go only applies the Windows \\?\ long-path prefix to absolute paths, so making
// every path absolute here is what gives the whole program long-path support.
type Plan struct {
	// Out is the run directory, named by timestamp and repo list, so a run never
	// writes into a directory belonging to another run.
	Out string

	// Stage holds in-progress bare clones.
	Stage string

	// ListPath is the repo list this run was built from.
	ListPath string

	// Started is the time the run directory name was derived from.
	Started time.Time
}

// NewPlan resolves the directories for a run over listPath.
//
// listPath is normally a real repo list file, but a caller with no backing
// file (the service scheduler, whose repos are inline in its config) may
// pass any opaque identifier instead - it is only ever used for naming
// (RunDirName, and the manifest's ListFile provenance field), never opened.
//
// baseDir is where a defaulted run directory is created, normally the working
// directory. outOverride and stageOverride correspond to --out and --stage and
// win when non-empty.
func NewPlan(listPath, baseDir, outOverride, stageOverride string, now Clock) (Plan, error) {
	if now == nil {
		return Plan{}, fmt.Errorf("layout: no clock supplied")
	}
	started := now()

	listAbs, err := filepath.Abs(listPath)
	if err != nil {
		return Plan{}, fmt.Errorf("resolve repo list path: %w", err)
	}

	out := outOverride
	if out == "" {
		base, err := filepath.Abs(baseDir)
		if err != nil {
			return Plan{}, fmt.Errorf("resolve base directory: %w", err)
		}
		out = filepath.Join(base, RunDirName(listPath, started))
	}
	outAbs, err := filepath.Abs(out)
	if err != nil {
		return Plan{}, fmt.Errorf("resolve output directory: %w", err)
	}

	stage := stageOverride
	if stage == "" {
		// Default inside the run directory, so staging lands on the same volume
		// as the output rather than on whatever drive holds the temp directory,
		// which is usually the smaller one.
		stage = filepath.Join(outAbs, StageDirName)
	}
	stageAbs, err := filepath.Abs(stage)
	if err != nil {
		return Plan{}, fmt.Errorf("resolve staging directory: %w", err)
	}

	return Plan{
		Out:      outAbs,
		Stage:    stageAbs,
		ListPath: listAbs,
		Started:  started,
	}, nil
}

// RunDirName is the directory name for a run: the timestamp followed by the repo
// list name, for example "20260727-164211-azuredevops-contoso".
func RunDirName(listPath string, started time.Time) string {
	return started.Format(outDirDateLayout) + "-" + ListBaseName(listPath)
}

// ListBaseName is the repo list filename without directory or extension.
func ListBaseName(listPath string) string {
	base := filepath.Base(listPath)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// ProjectDir is the directory collecting one project's archives (a Bitbucket
// workspace counts as a project). An empty project (github.com, codeberg.org -
// no project layer) puts the archive straight in the run's output directory.
func (p Plan) ProjectDir(project string) string {
	if project == "" {
		return p.Out
	}
	return filepath.Join(p.Out, project)
}

// ArchivePath is where one repository's archive is written. Nesting by project
// is what keeps the repository names that repeat across Azure projects apart.
func (p Plan) ArchivePath(project, repo string) string {
	return filepath.Join(p.ProjectDir(project), repo+ArchiveSuffix)
}

// PartPath is the temporary name an archive is written under. It is renamed onto
// the final name only after a successful close, so an archive that exists is
// always a complete archive.
func (p Plan) PartPath(project, repo string) string {
	return p.ArchivePath(project, repo) + ".part"
}

// StageDir is where one repository is cloned before being archived.
//
// The zero-padded index makes the name unique by construction, whatever the
// project and repository are called. That matters twice over: 25 repository
// basenames repeat across projects in the committed lists, and with --jobs above
// one, two workers sharing a staging directory would corrupt each other. The
// PowerShell script staged everything at temp/<repo>, which was safe only
// because it ran strictly sequentially and deleted each directory as it went.
//
// Keeping the name short also preserves headroom against MAX_PATH on Windows,
// where git itself does not honour \\?\ paths.
func (p Plan) StageDir(index int, project, repo string) string {
	name := fmt.Sprintf("%04d-%s", index, slug(repo))
	if project != "" {
		name = fmt.Sprintf("%04d-%s-%s", index, slug(project), slug(repo))
	}
	return filepath.Join(p.Stage, name)
}

// TrashDir holds staging directories that could not be deleted, so a locked file
// leaves litter to sweep up later instead of failing the run.
func (p Plan) TrashDir() string {
	return filepath.Join(p.Stage, ".trash")
}

// FailuresDir collects the captured stderr of failed repositories.
func (p Plan) FailuresDir() string {
	return filepath.Join(p.Out, FailuresDirName)
}

// FailureLogPath is where one repository's captured stderr is written.
func (p Plan) FailureLogPath(project, repo string) string {
	name := slug(repo)
	if project != "" {
		name = slug(project) + "-" + slug(repo)
	}
	return filepath.Join(p.FailuresDir(), name+".stderr.txt")
}

// LogPath is the run's JSONL log. It carries a timestamp so that re-running into
// the same run directory, which overwrites archives by design, still leaves the
// earlier run's log intact.
func (p Plan) LogPath() string {
	return filepath.Join(p.Out, "clonezip-"+p.Started.Format(logFileDateLayout)+".log")
}

// slug makes a string safe and short enough for a directory name. Unlike
// safepath.SanitizeComponent, which validates a name that has to survive intact,
// this deliberately rewrites: the result only ever forms part of a staging
// directory name that nobody needs to recognise.
func slug(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := b.String()
	if utf8.RuneCountInString(out) > maxSlugLen {
		out = string([]rune(out)[:maxSlugLen])
	}
	// A trailing dot or space is stripped by Windows, and a stem matching a
	// reserved device name is unusable; fall back rather than build such a path.
	if _, err := safepath.SanitizeComponent(out); err != nil {
		return "x"
	}
	return out
}

// RepoNameFromArchive derives the repository name from an archive filename,
// accepting both the current ".git.zip" and the legacy ".git.7z".
func RepoNameFromArchive(path string) (string, bool) {
	base := filepath.Base(path)
	for _, suffix := range []string{ArchiveSuffix, LegacyArchiveSuffix} {
		if len(base) > len(suffix) && strings.EqualFold(base[len(base)-len(suffix):], suffix) {
			return base[:len(base)-len(suffix)], true
		}
	}
	return "", false
}
