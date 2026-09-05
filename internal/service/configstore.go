package service

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"
	"sync"
)

// Errors ConfigStore can return, beyond the *ValidationError a bad edit
// produces.
var (
	ErrGroupExists  = errors.New("group already exists")
	ErrGroupRunning = errors.New("group is running")
)

// ConfigStore is the single write path for the service's group configuration:
// every edit validates the candidate config, saves it to disk, and only then
// applies it to the running Scheduler and Store - so a failed write (a full
// disk, a permissions problem) never leaves the running service out of sync
// with the file backing it.
//
// Locking order is always ConfigStore.mu first, then Scheduler's or Store's
// own lock inside AddOrUpdateGroup/RemoveGroup/AddGroup/etc - neither of
// those ever calls back into ConfigStore, so there is no risk of a cyclic
// wait.
type ConfigStore struct {
	mu    sync.Mutex
	path  string
	cfg   Config
	sched *Scheduler
	store *Store
}

// NewConfigStore wraps an already-loaded, already-validated Config as the
// live source of truth for group configuration, coordinating writes to path
// with the Scheduler and Store that must stay in step with it.
func NewConfigStore(cfg Config, path string, sched *Scheduler, store *Store) *ConfigStore {
	return &ConfigStore{path: path, cfg: cfg, sched: sched, store: store}
}

// Snapshot returns the current config, safe to read concurrently with any
// edit in progress.
func (cs *ConfigStore) Snapshot() Config {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.cfg
}

// CreateGroup adds a new group, effective immediately.
func (cs *ConfigStore) CreateGroup(g GroupConfig) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	g.Repos = sortReposAscending(g.Repos)
	g.Out = cs.cfg.relToRoot(g.Out)
	g.Stage = cs.cfg.relToRoot(g.Stage)

	for _, existing := range cs.cfg.Groups {
		if existing.Name == g.Name {
			return ErrGroupExists
		}
	}

	next := cs.cfg
	next.Groups = append(append([]GroupConfig{}, cs.cfg.Groups...), g)
	if err := next.Validate(); err != nil {
		return err
	}
	if err := next.Save(cs.path); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	cs.cfg = next

	resolved := next.Resolve(g)
	// Store.AddGroup must run before Scheduler.AddOrUpdateGroup: the latter
	// calls Store.SetNextRun, which silently no-ops for a name Store does not
	// know about yet.
	cs.store.AddGroup(g.Name, resolved.Schedule, resolved.Enabled, resolved.HistoryLimit, resolved.Out)
	if err := cs.sched.AddOrUpdateGroup(resolved); err != nil {
		return err
	}
	return nil
}

// UpdateGroup replaces oldName's config with g. oldName != g.Name is a rename.
//
// Renaming or deleting a group that is currently running is refused: the
// in-flight run itself is unaffected (its ResolvedGroup snapshot, taken when
// it started, is already independent of Scheduler.groups/Store.groups), but
// severing the Store/Scheduler key it is reporting progress against would
// make it vanish from the dashboard mid-run, which is worse than making the
// operator wait. A same-name edit (schedule/repos/settings changed, name
// unchanged) is always allowed, running or not - it only affects the next
// run.
func (cs *ConfigStore) UpdateGroup(oldName string, g GroupConfig) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	g.Repos = sortReposAscending(g.Repos)
	g.Out = cs.cfg.relToRoot(g.Out)
	g.Stage = cs.cfg.relToRoot(g.Stage)

	idx := -1
	for i, existing := range cs.cfg.Groups {
		if existing.Name == oldName {
			idx = i
		}
		if existing.Name == g.Name && existing.Name != oldName {
			return ErrGroupExists
		}
	}
	if idx < 0 {
		return ErrUnknownGroup
	}

	renaming := g.Name != oldName
	if renaming {
		if st, ok := cs.store.Get(oldName); ok && st.Running {
			return ErrGroupRunning
		}
	}

	next := cs.cfg
	next.Groups = append([]GroupConfig{}, cs.cfg.Groups...)
	next.Groups[idx] = g
	if err := next.Validate(); err != nil {
		return err
	}
	if err := next.Save(cs.path); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	cs.cfg = next

	resolved := next.Resolve(g)
	if renaming {
		cs.sched.RemoveGroup(oldName)
		cs.store.RemoveGroup(oldName)
		cs.store.AddGroup(g.Name, resolved.Schedule, resolved.Enabled, resolved.HistoryLimit, resolved.Out)
	} else {
		cs.store.UpdateGroup(g.Name, resolved.Schedule, resolved.Enabled, resolved.HistoryLimit)
	}
	if err := cs.sched.AddOrUpdateGroup(resolved); err != nil {
		return err
	}
	return nil
}

// DeleteGroup removes a group entirely. Refused while it is running, for the
// same reason a rename is - see UpdateGroup.
func (cs *ConfigStore) DeleteGroup(name string) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	if st, ok := cs.store.Get(name); ok && st.Running {
		return ErrGroupRunning
	}

	idx := -1
	for i, existing := range cs.cfg.Groups {
		if existing.Name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		return ErrUnknownGroup
	}

	next := cs.cfg
	next.Groups = append(append([]GroupConfig{}, cs.cfg.Groups[:idx]...), cs.cfg.Groups[idx+1:]...)
	if err := next.Validate(); err != nil {
		return err
	}
	if err := next.Save(cs.path); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	cs.cfg = next

	cs.sched.RemoveGroup(name)
	cs.store.RemoveGroup(name)
	return nil
}

// Settings is the service-wide part of the config - everything outside the
// group list - as edited from the dashboard. The backup root is deliberately
// absent: repointing it would strand every existing backup, so it stays a
// file-only setting.
type Settings struct {
	Title      string
	DateFormat string
	Listen     string
	Defaults   GroupDefaults
}

// SettingsOf returns the service-wide settings currently in effect.
func (c Config) SettingsOf() Settings {
	return Settings{
		Title:      c.Title,
		DateFormat: c.DateFormat,
		Listen:     c.Listen,
		Defaults:   c.Defaults,
	}
}

// UpdateSettings replaces the service-wide settings, effective immediately.
//
// Changed defaults reach the groups that inherit them by re-resolving every
// group: a group's ResolvedGroup is what the scheduler and store actually
// work with, and it merges Defaults in at resolve time. Runs already in
// flight keep the snapshot they started with. Listen is the exception - it is
// persisted but only takes effect on the next service start, since the
// listener is bound at startup.
func (cs *ConfigStore) UpdateSettings(s Settings) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	if err := validateSettings(s); err != nil {
		return err
	}

	next := cs.cfg
	next.Groups = append([]GroupConfig{}, cs.cfg.Groups...)
	next.Title = strings.TrimSpace(s.Title)
	next.DateFormat = firstNonEmpty(strings.TrimSpace(s.DateFormat), defaultDateFormat)
	next.Listen = firstNonEmpty(strings.TrimSpace(s.Listen), defaultListen)

	// Defaults.Out is the retired legacy key: never carry an incoming value
	// into it, or the next LoadConfig would reject the file it just wrote.
	next.Defaults = s.Defaults
	next.Defaults.Out = ""

	if err := next.Validate(); err != nil {
		return err
	}
	if err := next.Save(cs.path); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	cs.cfg = next

	for _, g := range next.Groups {
		resolved := next.Resolve(g)
		cs.store.UpdateGroup(g.Name, resolved.Schedule, resolved.Enabled, resolved.HistoryLimit)
		if err := cs.sched.AddOrUpdateGroup(resolved); err != nil {
			return err
		}
	}
	return nil
}

// validateSettings reports every problem with the incoming settings at once,
// in the same shape a bad group edit produces, so the dashboard can highlight
// the offending fields. Config.Validate only covers groups.
func validateSettings(s Settings) error {
	var verr ValidationError

	if f := strings.TrimSpace(s.DateFormat); f != "" && !bcp47Tag.MatchString(f) {
		verr.add("", "dateFormat", "must be a locale tag such as \"en-US\" or \"de-DE\"")
	}
	if l := strings.TrimSpace(s.Listen); l != "" {
		if _, _, err := net.SplitHostPort(l); err != nil {
			verr.add("", "listen", "must be a host:port address such as \":8091\"")
		}
	}

	d := s.Defaults
	if d.CloneDepth < 0 {
		verr.add("", "cloneDepth", "must be 0 (full history) or greater")
	}
	if d.Jobs < 1 {
		verr.add("", "jobs", "must be at least 1")
	}
	if d.Retries < 0 {
		verr.add("", "retries", "must be 0 or greater")
	}
	if d.Timeout <= 0 {
		verr.add("", "timeout", "must be greater than 0, e.g. \"30m\"")
	}
	if d.HistoryLimit < 1 {
		verr.add("", "historyLimit", "must be at least 1")
	}

	if len(verr.Fields) > 0 {
		return &verr
	}
	return nil
}

// bcp47Tag matches the shape of a language tag (language plus optional
// subtags); the browser's Intl is the real arbiter of what resolves.
var bcp47Tag = regexp.MustCompile(`^[A-Za-z]{2,8}(-[A-Za-z0-9]{1,8})*$`)
