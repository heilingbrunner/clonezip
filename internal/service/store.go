package service

import (
	"sync"
	"time"
)

// groupState is one group's mutable state. It is only ever touched through
// Store, which owns the lock.
type groupState struct {
	name     string
	schedule string
	enabled  bool

	running      bool
	currentRunID string

	// historyLimit bounds history below, and how many clonezip-*.log files
	// pruneHistoryFiles keeps under this group's out directory. Configured
	// per group (see ResolvedGroup.HistoryLimit), defaulting to
	// defaultHistoryLimit.
	historyLimit int

	nextRun time.Time
	lastRun *RunRecord
	history []RunRecord

	live LiveProgress
}

// Store holds every group's status and recent history in memory, safe for
// concurrent use: the scheduler's worker goroutines write to it while the web
// API's handlers read from it at the same time.
type Store struct {
	mu     sync.Mutex
	groups map[string]*groupState
	// order preserves the order groups were configured in, so Snapshot reads
	// the same way the YAML file lists them.
	order []string
}

// NewStore seeds a Store from the resolved group list, reloading each
// group's run history from its out directory's clonezip-*.log files - see
// history.go - so a service restart does not start every group's dashboard
// history empty.
func NewStore(groups []ResolvedGroup) *Store {
	s := &Store{groups: make(map[string]*groupState, len(groups))}
	for _, g := range groups {
		s.groups[g.Name] = newGroupState(g.Name, g.Schedule, g.Enabled, g.HistoryLimit, g.Out)
		s.order = append(s.order, g.Name)
	}
	return s
}

// newGroupState builds a group's initial state, including its history
// reloaded from out's clonezip-*.log files, so both NewStore and AddGroup seed a
// group the same way.
func newGroupState(name, schedule string, enabled bool, historyLimit int, out string) *groupState {
	gs := &groupState{name: name, schedule: schedule, enabled: enabled, historyLimit: historyLimit}
	gs.history = loadHistoryFromDisk(out, historyLimit)
	if len(gs.history) > 0 {
		last := gs.history[0]
		gs.lastRun = &last
	}
	return gs
}

// AddGroup registers a new group's display state, idempotently - a no-op if
// name is already known, since ConfigStore calls this on both create and on
// a same-name update, where the group already exists. Its history is
// reloaded from out the same way NewStore seeds it, so a group created (or
// renamed) into an out directory with pre-existing clonezip-*.log files picks
// them up immediately rather than only after the next service restart.
func (s *Store) AddGroup(name, schedule string, enabled bool, historyLimit int, out string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.groups[name]; exists {
		return
	}
	s.groups[name] = newGroupState(name, schedule, enabled, historyLimit, out)
	s.order = append(s.order, name)
}

// UpdateGroup applies a config edit's schedule/enabled/historyLimit to a
// group's display state. It does not touch running/history/live otherwise -
// those belong to runs, not config, and are otherwise untouched by an edit.
func (s *Store) UpdateGroup(name, schedule string, enabled bool, historyLimit int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if gs, ok := s.groups[name]; ok {
		gs.schedule, gs.enabled, gs.historyLimit = schedule, enabled, historyLimit
	}
}

// RemoveGroup drops a group's display state entirely - used only once its
// removal (or the rename that replaces it) has already been persisted and
// applied to the Scheduler.
func (s *Store) RemoveGroup(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.groups, name)
	for i, n := range s.order {
		if n == name {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
}

// Snapshot returns every group's status, in configured order.
func (s *Store) Snapshot() []GroupStatus {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]GroupStatus, 0, len(s.order))
	for _, name := range s.order {
		out = append(out, snapshotOf(s.groups[name]))
	}
	return out
}

// Get returns one group's status.
func (s *Store) Get(name string) (GroupStatus, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	gs, ok := s.groups[name]
	if !ok {
		return GroupStatus{}, false
	}
	return snapshotOf(gs), true
}

func snapshotOf(gs *groupState) GroupStatus {
	history := make([]RunRecord, len(gs.history))
	copy(history, gs.history)

	return GroupStatus{
		Name:         gs.name,
		Enabled:      gs.enabled,
		Schedule:     gs.schedule,
		Running:      gs.running,
		CurrentRunID: gs.currentRunID,
		LastRun:      gs.lastRun,
		NextRun:      gs.nextRun,
		History:      history,
	}
}

// MarkRunning records that a run has started, unless one is already in
// progress - in which case it reports false and the caller must not start
// another.
func (s *Store) MarkRunning(name, runID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	gs, ok := s.groups[name]
	if !ok || gs.running {
		return false
	}
	gs.running = true
	gs.currentRunID = runID
	gs.live = LiveProgress{}
	return true
}

// MarkFinished records a run's outcome and clears the running flag.
func (s *Store) MarkFinished(name string, rec RunRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()

	gs, ok := s.groups[name]
	if !ok {
		return
	}
	gs.running = false
	gs.currentRunID = ""

	last := rec
	gs.lastRun = &last

	gs.history = append([]RunRecord{rec}, gs.history...)
	if len(gs.history) > gs.historyLimit {
		gs.history = gs.history[:gs.historyLimit]
	}
}

// SetNextRun records when a group's cron schedule will next fire.
func (s *Store) SetNextRun(name string, t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if gs, ok := s.groups[name]; ok {
		gs.nextRun = t
	}
}

// Live returns a group's in-flight progress. The second result is false when
// the group is unknown or not currently running.
func (s *Store) Live(name string) (LiveProgress, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	gs, ok := s.groups[name]
	if !ok || !gs.running {
		return LiveProgress{}, false
	}
	live := gs.live
	live.Repos = make([]RepoLive, len(gs.live.Repos))
	copy(live.Repos, gs.live.Repos)
	live.LogTail = make([]LiveLogLine, len(gs.live.LogTail))
	copy(live.LogTail, gs.live.LogTail)
	return live, true
}

// StartLive records a run's starting metadata: how many repositories it
// covers, and where and how it was launched.
func (s *Store) StartLive(name string, meta LiveProgress) {
	s.mu.Lock()
	defer s.mu.Unlock()

	gs, ok := s.groups[name]
	if !ok {
		return
	}
	meta.Done = 0
	meta.Repos = make([]RepoLive, meta.Total)
	gs.live = meta
}

// UpdateLive applies one repository's progress within the current run. index
// must be within the bounds SetLiveTotal established; out-of-range updates
// are dropped rather than panicking, since a Reporter event racing a run's
// own completion is a timing quirk, not a bug worth crashing over.
func (s *Store) UpdateLive(name string, index int, update func(*RepoLive)) {
	s.mu.Lock()
	defer s.mu.Unlock()

	gs, ok := s.groups[name]
	if !ok || index < 0 || index >= len(gs.live.Repos) {
		return
	}
	update(&gs.live.Repos[index])
}

// MarkRepoDone advances the run's completed counter.
func (s *Store) MarkRepoDone(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if gs, ok := s.groups[name]; ok {
		gs.live.Done++
	}
}

// AppendLog adds one line to the run's combined log tail, prefixed with the
// repository it came from when index names a known one.
func (s *Store) AppendLog(name string, index int, level, line string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	gs, ok := s.groups[name]
	if !ok {
		return
	}
	entry := LiveLogLine{Level: level, Line: line}
	if index >= 0 && index < len(gs.live.Repos) {
		r := gs.live.Repos[index]
		switch {
		case r.Project != "" && r.Repo != "":
			entry.Repo = r.Project + "/" + r.Repo
		case r.Repo != "":
			entry.Repo = r.Repo
		case r.Project != "":
			entry.Repo = r.Project
		}
	}
	gs.live.LogTail = append(gs.live.LogTail, entry)
	if len(gs.live.LogTail) > logTailLimit {
		gs.live.LogTail = gs.live.LogTail[len(gs.live.LogTail)-logTailLimit:]
	}
}
