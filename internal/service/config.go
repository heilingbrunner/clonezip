// Package service runs backup groups on a cron schedule, headlessly: no TUI, no
// stdout output, just an in-memory status store that a presentation layer
// (internal/webapi) can read. It mirrors the layering internal/pipeline
// already uses to stay independent of internal/ui.
package service

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"go.yaml.in/yaml/v3"

	"github.com/heilingbrunner/clonezip/internal/common/repolist"
)

// defaultListen is used when the config omits "listen".
const defaultListen = ":8091"

// defaultTimeout mirrors the --timeout default of the backup command.
const defaultTimeout = 30 * time.Minute

// defaultHistoryLimit bounds how many past runs a group keeps, in memory and
// as clonezip-*.log files under its out directory, when historyLimit is left
// unset.
const defaultHistoryLimit = 50

// defaultDateFormat is used when the config omits "dateFormat".
const defaultDateFormat = "en-US"

// defaultOut is the backup root used when the config omits the top-level "out".
const defaultOut = "backups"

// Config is the service's YAML configuration: where to listen, and which
// backup groups to run on which schedule.
type Config struct {
	// Title is an optional label shown in the dashboard header, under "clonezip
	// service" - useful for telling multiple instances apart at a glance.
	Title string `yaml:"title,omitempty"`

	// DateFormat is a BCP-47 locale tag (e.g. "en-US", "de-DE") the dashboard
	// uses to render run timestamps, passed straight through to the browser's
	// Intl/toLocaleString. Defaults to "en-US" when omitted.
	DateFormat string `yaml:"dateFormat,omitempty"`
	Listen     string `yaml:"listen"`

	// AllowActionsFrom lists the client networks - CIDR blocks, or bare IP
	// addresses meaning a single host - allowed to change anything: creating,
	// editing and deleting groups, editing these settings, and triggering
	// runs and restores. Every other client gets the dashboard read-only:
	// GET requests are served as usual, everything else is refused with 403.
	//
	// Omitted means loopback only (see defaultAllowActionsFrom), which makes
	// a service bound to every interface safe to expose without further
	// configuration. Widen it to hand actions to a LAN range, or to the
	// address a same-host reverse proxy connects from - without which such a
	// proxy makes every client look local and the guard lets everyone act.
	//
	// Deliberately not editable through the dashboard: an access policy the
	// surface it protects can rewrite is not much of a policy.
	AllowActionsFrom []string `yaml:"allowActionsFrom,omitempty"`

	// Out is the backup root. Every group writes under it, and a group's own
	// "out" is resolved relative to it and may never climb above it - so a
	// stray "../" or an absolute path in a group config cannot write anywhere
	// else. Relative to Dir (the config file's directory); "backups" when
	// omitted.
	Out string `yaml:"out"`

	Defaults GroupDefaults `yaml:"defaults"`
	Groups   []GroupConfig `yaml:"groups"`

	// Dir is the absolute directory the config file lives in. The top-level
	// "out" and every relative repoList/stage path resolves against it, not the
	// process's working directory - important once the service is launched by
	// systemd or a Windows Service with its own working directory.
	Dir string `yaml:"-"`
}

// GroupDefaults are applied to any group field left unset.
type GroupDefaults struct {
	// Out is no longer used: the backup root moved to the top-level "out" key.
	// The field is kept only so LoadConfig can detect an old config and point
	// the operator at the new key; it is never written back.
	Out string `yaml:"out,omitempty"`

	CloneDepth   int           `yaml:"cloneDepth"`
	Jobs         int           `yaml:"jobs"`
	Retries      int           `yaml:"retries"`
	Timeout      time.Duration `yaml:"timeout"`
	FailFast     bool          `yaml:"failFast"`
	SkipChecks   bool          `yaml:"skipChecks"`
	HistoryLimit int           `yaml:"historyLimit"`
}

// GroupConfig is one scheduled backup group as written in the YAML file.
//
// The override fields are pointers so that "not set, use the default" stays
// distinguishable from an explicit zero/false - the same problem the backup
// command's flags sidestep by having their defaults baked into cmd.Flags().
type GroupConfig struct {
	Name     string   `yaml:"name"`
	Repos    []string `yaml:"repos"`
	Schedule string   `yaml:"schedule"`
	Enabled  *bool    `yaml:"enabled,omitempty"`

	// Out and Stage are resolved relative to the top-level "out" (the backup
	// root) and may not climb above it. Out defaults to the group name, so a
	// group "demos" writes to <root>/demos; Stage defaults to <out>/.stage.
	Out   string `yaml:"out,omitempty"`
	Stage string `yaml:"stage,omitempty"`

	CloneDepth   *int           `yaml:"cloneDepth,omitempty"`
	Jobs         *int           `yaml:"jobs,omitempty"`
	Retries      *int           `yaml:"retries,omitempty"`
	Timeout      *time.Duration `yaml:"timeout,omitempty"`
	FailFast     *bool          `yaml:"failFast,omitempty"`
	SkipChecks   *bool          `yaml:"skipChecks,omitempty"`
	HistoryLimit *int           `yaml:"historyLimit,omitempty"`
}

// IsEnabled reports whether the group runs on its schedule. Unset means enabled.
func (g GroupConfig) IsEnabled() bool { return orPtr(g.Enabled, true) }

// ResolvedGroup is a group's effective settings: GroupConfig with defaults
// merged in and every path made absolute. This is what the scheduler and the
// store actually work with.
type ResolvedGroup struct {
	Name     string
	Repos    []string
	Schedule string
	Enabled  bool
	Out      string
	Stage    string

	CloneDepth   int
	Jobs         int
	Retries      int
	Timeout      time.Duration
	FailFast     bool
	SkipChecks   bool
	HistoryLimit int
}

// Resolve merges defaults into g and resolves its paths under the backup root.
func (c Config) Resolve(g GroupConfig) ResolvedGroup {
	d := c.Defaults

	repos := make([]string, len(g.Repos))
	copy(repos, g.Repos)

	stage := ""
	if g.Stage != "" {
		stage = c.underRoot(g.Stage)
	}

	r := ResolvedGroup{
		Name:         g.Name,
		Repos:        repos,
		Schedule:     g.Schedule,
		Enabled:      g.IsEnabled(),
		Out:          c.underRoot(firstNonEmpty(g.Out, g.Name)),
		Stage:        stage,
		CloneDepth:   orPtr(g.CloneDepth, d.CloneDepth),
		Jobs:         orPtr(g.Jobs, d.Jobs),
		Retries:      orPtr(g.Retries, d.Retries),
		Timeout:      orPtr(g.Timeout, d.Timeout),
		FailFast:     orPtr(g.FailFast, d.FailFast),
		SkipChecks:   orPtr(g.SkipChecks, d.SkipChecks),
		HistoryLimit: orPtr(g.HistoryLimit, d.HistoryLimit),
	}
	if r.Jobs < 1 {
		r.Jobs = 1
	}
	if r.Timeout <= 0 {
		r.Timeout = defaultTimeout
	}
	if r.HistoryLimit < 1 {
		r.HistoryLimit = defaultHistoryLimit
	}
	return r
}

// rootOut is the absolute backup root: the top-level "out", resolved against
// the config directory, or "backups" there when unset.
func (c Config) rootOut() string {
	return c.abs(firstNonEmpty(c.Out, defaultOut))
}

// BackupRoot is the absolute directory every group backs up under. Exported for
// the CLI banner; callers inside this package use rootOut.
func (c Config) BackupRoot() string { return c.rootOut() }

// underRoot resolves a group's out/stage path. A relative path is taken
// relative to the backup root; an absolute path is cleaned and returned as-is
// (Validate rejects it, and any relative path that escapes the root, before a
// run can use it).
func (c Config) underRoot(p string) string {
	if p == "" {
		return c.rootOut()
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(c.rootOut(), p)
}

// withinRoot reports whether an already-resolved absolute path is the backup
// root or sits inside it.
func (c Config) withinRoot(abs string) bool {
	rel, err := filepath.Rel(c.rootOut(), abs)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// ResolvedGroups returns every configured group, defaults merged in.
func (c Config) ResolvedGroups() []ResolvedGroup {
	out := make([]ResolvedGroup, len(c.Groups))
	for i, g := range c.Groups {
		out[i] = c.Resolve(g)
	}
	return out
}

func (c Config) abs(p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(c.Dir, p)
}

// relToRoot produces the form a group's out/stage path should take when
// written back to the config file: relative to the backup root, and
// unambiguously a path.
//
//   - An absolute path inside the root becomes relative to it, so a save from
//     the dashboard (whose form is prefilled with the resolved absolute path)
//     does not freeze that absolute path into the file.
//   - Any relative result is given an explicit leading "./" (or kept "../"), so
//     a stored "out: ./demos" reads clearly as a path.
//   - An absolute path outside the root, an empty value, and "." are left as-is
//     for Validate to reject or accept.
func (c Config) relToRoot(p string) string {
	if p == "" || p == "." {
		return p
	}
	root := c.rootOut()
	if filepath.IsAbs(p) {
		r, err := filepath.Rel(root, p)
		if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
			return p
		}
		if p = filepath.ToSlash(r); p == "." {
			return p
		}
	} else {
		p = filepath.ToSlash(p)
	}
	if strings.HasPrefix(p, "./") || strings.HasPrefix(p, "../") {
		return p
	}
	return "./" + p
}

// LoadConfig reads and validates the service config at path.
func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}

	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		return Config{}, fmt.Errorf("resolve %s: %w", path, err)
	}
	c.Dir = filepath.Dir(absPath)

	if c.Defaults.Out != "" {
		return Config{}, fmt.Errorf(
			"parse %s: 'defaults.out' is no longer supported - move it to a top-level 'out:' key "+
				"(below 'listen:') and make each group's 'out' relative to it", path)
	}

	if c.Listen == "" {
		c.Listen = defaultListen
	}
	if c.DateFormat == "" {
		c.DateFormat = defaultDateFormat
	}
	if c.Out == "" {
		c.Out = defaultOut
	}

	if len(c.Groups) == 0 {
		return Config{}, fmt.Errorf("no groups configured")
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// Save serializes c back to YAML and atomically replaces the file at path:
// write to a temp file in the same directory, sync, then rename over path -
// so a crash mid-write can never leave a half-written config on disk.
func (c Config) Save(path string) error {
	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".clonezip-service-*.yaml.tmp")
	if err != nil {
		return fmt.Errorf("create temp config file: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // no-op once the rename below succeeds

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp config file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp config file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp config file: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("replace config file: %w", err)
	}
	return nil
}

// FieldError is one problem found with a specific group's field.
type FieldError struct {
	Group, Field, Msg string
}

func (e FieldError) Error() string {
	if e.Group == "" {
		return fmt.Sprintf("%s: %s", e.Field, e.Msg)
	}
	return fmt.Sprintf("group %q: %s: %s", e.Group, e.Field, e.Msg)
}

// ValidationError collects every problem Validate found, so a config with
// several mistakes reports all of them at once rather than stopping at the
// first - useful both for --check-config output and for field-level errors
// returned by the web API.
type ValidationError struct {
	Fields []FieldError
}

func (e *ValidationError) Error() string {
	msgs := make([]string, len(e.Fields))
	for i, f := range e.Fields {
		msgs[i] = f.Error()
	}
	return strings.Join(msgs, "; ")
}

func (e *ValidationError) add(group, field, msg string) {
	e.Fields = append(e.Fields, FieldError{Group: group, Field: field, Msg: msg})
}

// Validate reports whether the config is usable: names are unique and
// non-empty, every group has at least one valid repository URL, and every
// schedule parses. A config with zero groups is valid as far as Validate is
// concerned - LoadConfig itself rejects that at startup, but ConfigStore
// must be able to call Validate on a config mid-delete, including the delete
// that empties the last group.
func (c Config) Validate() error {
	var verr ValidationError

	// allowActionsFrom governs who may change anything through the web API,
	// so an unparseable entry has to stop the service rather than quietly
	// shrink the allowlist and lock the operator out of their own dashboard.
	for i, entry := range c.AllowActionsFrom {
		if _, err := parseAllowEntry(entry); err != nil {
			verr.add("", fmt.Sprintf("allowActionsFrom[%d]", i), err.Error())
		}
	}

	seen := make(map[string]bool, len(c.Groups))
	for _, g := range c.Groups {
		if g.Name == "" {
			verr.add("", "name", "a group is missing a name")
			continue
		}
		if seen[g.Name] {
			verr.add(g.Name, "name", "duplicate group name")
		}
		seen[g.Name] = true

		if len(g.Repos) == 0 {
			verr.add(g.Name, "repos", "at least one repository URL is required")
		} else {
			list := repolist.ParseEntries(g.Repos)
			for _, issue := range list.Errors() {
				verr.add(g.Name, fmt.Sprintf("repos[%d]", issue.LineNo-1), issue.Msg)
			}
		}

		if g.Schedule == "" {
			verr.add(g.Name, "schedule", "schedule is required")
		} else if _, err := cron.ParseStandard(g.Schedule); err != nil {
			verr.add(g.Name, "schedule", err.Error())
		}

		// out and stage may only name a location inside the backup root, so a
		// stray "../", an absolute path, or a group name like ".." cannot make
		// a group write elsewhere. out is checked as it resolves (the group
		// name when the field is empty).
		root := firstNonEmpty(c.Out, defaultOut)
		if !c.withinRoot(c.underRoot(firstNonEmpty(g.Out, g.Name))) {
			verr.add(g.Name, "out", fmt.Sprintf("%q resolves outside the backup root %q",
				firstNonEmpty(g.Out, g.Name), root))
		}
		if g.Stage != "" && !c.withinRoot(c.underRoot(g.Stage)) {
			verr.add(g.Name, "stage", fmt.Sprintf("%q is outside the backup root %q", g.Stage, root))
		}
	}

	if len(verr.Fields) > 0 {
		return &verr
	}
	return nil
}

// sortReposAscending returns repos ordered case-insensitively ascending,
// ignoring a leading "#" disable marker so a disabled entry sorts beside where
// its URL belongs rather than clustering. Equal keys fall back to a
// case-sensitive compare for a stable, deterministic result. It returns a
// fresh slice, leaving the input untouched.
func sortReposAscending(repos []string) []string {
	out := append([]string(nil), repos...)
	slices.SortStableFunc(out, func(a, b string) int {
		if c := strings.Compare(
			strings.ToLower(repoSortKey(a)), strings.ToLower(repoSortKey(b)),
		); c != 0 {
			return c
		}
		return strings.Compare(a, b)
	})
	return out
}

// repoSortKey strips the optional "# " disable marker (written and read only by
// the web UI - see the repolist package docs) so enabled and disabled entries
// interleave by URL.
func repoSortKey(line string) string {
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "#"))
}

func orPtr[T any](p *T, fallback T) T {
	if p != nil {
		return *p
	}
	return fallback
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
