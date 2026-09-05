// Package webapi is the HTTP presentation layer over internal/service: a
// JSON API plus an embedded static dashboard. It depends on internal/service
// for its data; internal/service has no idea webapi exists, the same
// separation internal/pipeline keeps from internal/ui.
package webapi

import (
	"time"

	"github.com/heilingbrunner/clonezip/internal/service"
)

type groupSummaryDTO struct {
	Name      string        `json:"name"`
	Enabled   bool          `json:"enabled"`
	Schedule  string        `json:"schedule"`
	Running   bool          `json:"running"`
	RepoCount int           `json:"repoCount"`
	NextRun   *time.Time    `json:"nextRun,omitempty"`
	LastRun   *runRecordDTO `json:"lastRun,omitempty"`
}

type groupDetailDTO struct {
	groupSummaryDTO
	CurrentRunID string         `json:"currentRunId,omitempty"`
	Live         *liveDTO       `json:"live,omitempty"`
	HistoryCount int            `json:"historyCount"`
	Config       groupConfigDTO `json:"config"`
}

type groupConfigDTO struct {
	Repos []string `json:"repos"`
	// Out and Stage are the raw values stored in the config file - relative to
	// the backup root, empty when left to default. This is what the edit form
	// binds to, so a save round-trips instead of freezing an absolute path.
	Out   string `json:"out"`
	Stage string `json:"stage,omitempty"`
	// OutResolved is the effective absolute output directory (Out resolved
	// under the backup root, or <root>/<group name> when Out is empty), for
	// display where the real location matters more than the stored value.
	OutResolved string `json:"outResolved"`
	CloneDepth  int    `json:"cloneDepth"`
	Jobs        int    `json:"jobs"`
	Retries     int    `json:"retries"`
	Timeout     string `json:"timeout"`
	FailFast    bool   `json:"failFast"`
	SkipChecks  bool   `json:"skipChecks"`
}

// groupWriteDTO is the POST/PUT request body for creating or updating a
// group. Pointer fields mirror service.GroupConfig's own pointers 1:1:
// omitted/null means "inherit the configured default", present means an
// explicit per-group override - the same distinction GroupConfig already
// models, so toGroupConfig is a direct translation plus parsing Timeout's
// duration string (JSON has no native duration type).
type groupWriteDTO struct {
	Name       string   `json:"name"`
	Schedule   string   `json:"schedule"`
	Enabled    *bool    `json:"enabled"`
	Out        string   `json:"out"`
	Stage      string   `json:"stage"`
	CloneDepth *int     `json:"cloneDepth"`
	Jobs       *int     `json:"jobs"`
	Retries    *int     `json:"retries"`
	Timeout    *string  `json:"timeout"`
	FailFast   *bool    `json:"failFast"`
	SkipChecks *bool    `json:"skipChecks"`
	Repos      []string `json:"repos"`
}

// toGroupConfig converts a request body into a service.GroupConfig. The only
// error possible here is a malformed Timeout string - every other kind of
// "invalid" field (a bad schedule, a bad repo URL) is reported by
// service.Config.Validate instead, since that is the single place both this
// API and --check-config already get validation from.
func (dto groupWriteDTO) toGroupConfig() (service.GroupConfig, *fieldErrorDTO) {
	gc := service.GroupConfig{
		Name:       dto.Name,
		Repos:      dto.Repos,
		Schedule:   dto.Schedule,
		Enabled:    dto.Enabled,
		Out:        dto.Out,
		Stage:      dto.Stage,
		CloneDepth: dto.CloneDepth,
		Jobs:       dto.Jobs,
		Retries:    dto.Retries,
		FailFast:   dto.FailFast,
		SkipChecks: dto.SkipChecks,
	}
	if dto.Timeout != nil {
		d, err := time.ParseDuration(*dto.Timeout)
		if err != nil {
			return service.GroupConfig{}, &fieldErrorDTO{Field: "timeout", Msg: err.Error()}
		}
		gc.Timeout = &d
	}
	return gc, nil
}

// settingsDTO is the GET /api/settings response: the service-wide config the
// dashboard can edit, plus the backup root as read-only context (repointing
// it would strand every existing backup, so it stays a file-only setting).
type settingsDTO struct {
	Title       string              `json:"title"`
	DateFormat  string              `json:"dateFormat"`
	Listen      string              `json:"listen"`
	Out         string              `json:"out"`
	OutResolved string              `json:"outResolved"`
	Defaults    settingsDefaultsDTO `json:"defaults"`
}

type settingsDefaultsDTO struct {
	CloneDepth   int    `json:"cloneDepth"`
	Jobs         int    `json:"jobs"`
	Retries      int    `json:"retries"`
	Timeout      string `json:"timeout"`
	FailFast     bool   `json:"failFast"`
	SkipChecks   bool   `json:"skipChecks"`
	HistoryLimit int    `json:"historyLimit"`
}

// settingsWriteDTO is the PUT /api/settings request body. Unlike
// groupWriteDTO these are not overrides, so the fields are plain values: a
// settings save always sends the complete set the form shows.
type settingsWriteDTO struct {
	Title      string                   `json:"title"`
	DateFormat string                   `json:"dateFormat"`
	Listen     string                   `json:"listen"`
	Defaults   settingsDefaultsWriteDTO `json:"defaults"`
}

type settingsDefaultsWriteDTO struct {
	CloneDepth   int    `json:"cloneDepth"`
	Jobs         int    `json:"jobs"`
	Retries      int    `json:"retries"`
	Timeout      string `json:"timeout"`
	FailFast     bool   `json:"failFast"`
	SkipChecks   bool   `json:"skipChecks"`
	HistoryLimit int    `json:"historyLimit"`
}

func toSettingsDTO(cfg service.Config) settingsDTO {
	s := cfg.SettingsOf()
	return settingsDTO{
		Title:       s.Title,
		DateFormat:  s.DateFormat,
		Listen:      s.Listen,
		Out:         cfg.Out,
		OutResolved: cfg.BackupRoot(),
		Defaults: settingsDefaultsDTO{
			CloneDepth:   s.Defaults.CloneDepth,
			Jobs:         s.Defaults.Jobs,
			Retries:      s.Defaults.Retries,
			Timeout:      s.Defaults.Timeout.String(),
			FailFast:     s.Defaults.FailFast,
			SkipChecks:   s.Defaults.SkipChecks,
			HistoryLimit: s.Defaults.HistoryLimit,
		},
	}
}

// toSettings converts a request body into service.Settings. As with
// toGroupConfig the only error possible here is a malformed duration string;
// range checks live in service.ConfigStore.UpdateSettings.
func (dto settingsWriteDTO) toSettings() (service.Settings, *fieldErrorDTO) {
	d, err := time.ParseDuration(dto.Defaults.Timeout)
	if err != nil {
		return service.Settings{}, &fieldErrorDTO{Field: "timeout", Msg: err.Error()}
	}
	return service.Settings{
		Title:      dto.Title,
		DateFormat: dto.DateFormat,
		Listen:     dto.Listen,
		Defaults: service.GroupDefaults{
			CloneDepth:   dto.Defaults.CloneDepth,
			Jobs:         dto.Defaults.Jobs,
			Retries:      dto.Defaults.Retries,
			Timeout:      d,
			FailFast:     dto.Defaults.FailFast,
			SkipChecks:   dto.Defaults.SkipChecks,
			HistoryLimit: dto.Defaults.HistoryLimit,
		},
	}, nil
}

type fieldErrorDTO struct {
	Field string `json:"field"`
	Msg   string `json:"msg"`
}

type validationErrorDTO struct {
	Error  string          `json:"error"`
	Fields []fieldErrorDTO `json:"fields,omitempty"`
}

func toValidationDTO(verr *service.ValidationError) validationErrorDTO {
	fields := make([]fieldErrorDTO, len(verr.Fields))
	for i, f := range verr.Fields {
		fields[i] = fieldErrorDTO{Field: f.Field, Msg: f.Msg}
	}
	return validationErrorDTO{Error: "invalid request", Fields: fields}
}

type runRecordDTO struct {
	ID         string           `json:"id"`
	Trigger    string           `json:"trigger"`
	Started    time.Time        `json:"started"`
	Ended      time.Time        `json:"ended"`
	OK         int              `json:"ok"`
	Warn       int              `json:"warn"`
	Failed     int              `json:"failed"`
	Skipped    int              `json:"skipped"`
	TotalBytes int64            `json:"totalBytes"`
	DirBytes   int64            `json:"dirBytes"`
	RepoCount  int              `json:"repoCount"`
	GitVersion string           `json:"gitVersion,omitempty"`
	LFSVersion string           `json:"lfsVersion,omitempty"`
	StuckDirs  int              `json:"stuckDirs,omitempty"`
	Repos      []repoOutcomeDTO `json:"repos,omitempty"`
	Out        string           `json:"out"`
	LogPath    string           `json:"logPath"`
	Err        string           `json:"err,omitempty"`
}

type repoOutcomeDTO struct {
	Project string `json:"project"`
	Repo    string `json:"repo"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Bytes   int64  `json:"bytes"`
}

type repoLiveDTO struct {
	Index   int       `json:"index"`
	Project string    `json:"project"`
	Repo    string    `json:"repo"`
	Phase   string    `json:"phase"`
	Detail  string    `json:"detail,omitempty"`
	Pct     int       `json:"pct"`
	Status  string    `json:"status,omitempty"`
	Started time.Time `json:"started,omitempty"`

	BytesDone  int64  `json:"bytesDone,omitempty"`
	BytesTotal int64  `json:"bytesTotal,omitempty"`
	LastLine   string `json:"lastLine,omitempty"`

	RetryAttempt int    `json:"retryAttempt,omitempty"`
	RetryOf      int    `json:"retryOf,omitempty"`
	RetryReason  string `json:"retryReason,omitempty"`
	RetryWaitMs  int64  `json:"retryWaitMs,omitempty"`
}

type logLineDTO struct {
	Repo  string `json:"repo,omitempty"`
	Level string `json:"level"`
	Line  string `json:"line"`
}

type liveDTO struct {
	Total int           `json:"total"`
	Done  int           `json:"done"`
	Repos []repoLiveDTO `json:"repos"`

	Out     string    `json:"out"`
	Log     string    `json:"log"`
	Jobs    int       `json:"jobs"`
	Depth   int       `json:"depth"`
	Started time.Time `json:"started"`

	LogTail []logLineDTO `json:"logTail,omitempty"`
}

type healthDTO struct {
	Status     string `json:"status"`
	Uptime     string `json:"uptime"`
	Title      string `json:"title,omitempty"`
	DateFormat string `json:"dateFormat,omitempty"`

	// ActionsAllowed tells the dashboard whether this client may change
	// anything, so it can hide the controls that would only come back as a
	// 403 (see actionGuard). It is a hint for the UI, never the control
	// itself - the guard runs on every request regardless of what the page
	// chose to render.
	ActionsAllowed bool `json:"actionsAllowed"`

	// BackupRoot is the top-level "out" directory; StorageBytes is its current
	// on-disk size and FreeBytes the space left on the volume holding it. This
	// is the whole of the dashboard's disk-usage figure - nothing per-group is
	// measured.
	BackupRoot   string  `json:"backupRoot"`
	StorageBytes int64   `json:"storageBytes"`
	FreeBytes    *uint64 `json:"freeBytes,omitempty"`

	// TotalRepos is the sum of RepoCount across every configured group, for
	// the dashboard's at-a-glance repository total.
	TotalRepos int `json:"totalRepos"`
}

type triggerResponseDTO struct {
	RunID   string    `json:"runId"`
	Started time.Time `json:"started"`
}

type batchResultDTO struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// batchStatusDTO mirrors service.BatchStatus for the "run all" progress
// endpoint - polled on page load and on every tick so a batch started before
// the browser was closed (or by another client) is picked back up.
type batchStatusDTO struct {
	Running  bool             `json:"running"`
	Names    []string         `json:"names"`
	Current  string           `json:"current,omitempty"`
	Total    int              `json:"total"`
	Done     int              `json:"done"`
	Started  *time.Time       `json:"started,omitempty"`
	Finished *time.Time       `json:"finished,omitempty"`
	Results  []batchResultDTO `json:"results"`
}

func toBatchStatus(b service.BatchStatus) batchStatusDTO {
	results := make([]batchResultDTO, len(b.Results))
	for i, r := range b.Results {
		results[i] = batchResultDTO{Name: r.Name, Status: r.Status, Reason: r.Reason}
	}
	dto := batchStatusDTO{
		Running: b.Running,
		Names:   b.Names,
		Current: b.Current,
		Total:   len(b.Names),
		Done:    len(b.Results),
		Results: results,
	}
	if !b.Started.IsZero() {
		t := b.Started
		dto.Started = &t
	}
	if !b.Finished.IsZero() {
		t := b.Finished
		dto.Finished = &t
	}
	return dto
}

// archiveDTO is one restorable repository: the archive a group currently
// holds for it.
type archiveDTO struct {
	Project  string    `json:"project,omitempty"`
	Repo     string    `json:"repo"`
	Label    string    `json:"label"`
	Path     string    `json:"path"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

func toArchive(a service.ArchiveInfo) archiveDTO {
	return archiveDTO{
		Project:  a.Project,
		Repo:     a.Repo,
		Label:    a.Key(),
		Path:     a.Path,
		Size:     a.Size,
		Modified: a.Modified,
	}
}

// restoreWriteDTO is the restore request the dashboard posts.
type restoreWriteDTO struct {
	Group   string `json:"group"`
	Project string `json:"project"`
	Repo    string `json:"repo"`
	Dest    string `json:"dest"`

	Origin         string `json:"origin"`
	Clone          bool   `json:"clone"`
	Force          bool   `json:"force"`
	ConfirmShallow bool   `json:"confirmShallow"`
	NoVerify       bool   `json:"noVerify"`
	SkipChecks     bool   `json:"skipChecks"`
}

type restoreOutcomeDTO struct {
	BarePath  string   `json:"barePath"`
	WorkPath  string   `json:"workPath,omitempty"`
	Cloned    bool     `json:"cloned"`
	Shallow   bool     `json:"shallow"`
	Refs      int      `json:"refs"`
	LFSBytes  int64    `json:"lfsBytes,omitempty"`
	OriginURL string   `json:"originUrl,omitempty"`
	Warnings  []string `json:"warnings,omitempty"`
}

// restoreStatusDTO mirrors service.RestoreStatus for the polled restore
// progress endpoint, the single-archive counterpart of liveDTO.
type restoreStatusDTO struct {
	Running bool `json:"running"`

	Group   string `json:"group,omitempty"`
	Project string `json:"project,omitempty"`
	Repo    string `json:"repo,omitempty"`
	Archive string `json:"archive,omitempty"`
	Dest    string `json:"dest,omitempty"`

	Started  *time.Time `json:"started,omitempty"`
	Finished *time.Time `json:"finished,omitempty"`

	Phase   string       `json:"phase,omitempty"`
	Pct     int          `json:"pct"`
	LogTail []logLineDTO `json:"logTail,omitempty"`

	Result *restoreOutcomeDTO `json:"result,omitempty"`
	Err    string             `json:"err,omitempty"`
}

func toRestoreStatus(s service.RestoreStatus) restoreStatusDTO {
	dto := restoreStatusDTO{
		Running: s.Running,
		Group:   s.Group,
		Project: s.Project,
		Repo:    s.Repo,
		Archive: s.Archive,
		Dest:    s.Dest,
		Phase:   s.Phase,
		Pct:     s.Pct,
		Err:     s.Err,
	}
	if !s.Started.IsZero() {
		t := s.Started
		dto.Started = &t
	}
	if !s.Finished.IsZero() {
		t := s.Finished
		dto.Finished = &t
	}
	for _, l := range s.LogTail {
		dto.LogTail = append(dto.LogTail, logLineDTO{Repo: l.Repo, Level: l.Level, Line: l.Line})
	}
	if s.Result != nil {
		dto.Result = &restoreOutcomeDTO{
			BarePath:  s.Result.BarePath,
			WorkPath:  s.Result.WorkPath,
			Cloned:    s.Result.Cloned,
			Shallow:   s.Result.Shallow,
			Refs:      s.Result.Refs,
			LFSBytes:  s.Result.LFSBytes,
			OriginURL: s.Result.OriginURL,
			Warnings:  s.Result.Warnings,
		}
	}
	return dto
}

type errorDTO struct {
	Error string `json:"error"`
}

type cronPreviewDTO struct {
	Valid    bool     `json:"valid"`
	Error    string   `json:"error,omitempty"`
	NextRuns []string `json:"nextRuns,omitempty"`
}

func toGroupSummary(gs service.GroupStatus, repoCount int) groupSummaryDTO {
	dto := groupSummaryDTO{
		Name:      gs.Name,
		Enabled:   gs.Enabled,
		Schedule:  gs.Schedule,
		Running:   gs.Running,
		RepoCount: repoCount,
	}
	if !gs.NextRun.IsZero() {
		t := gs.NextRun
		dto.NextRun = &t
	}
	if gs.LastRun != nil {
		r := toRunRecord(*gs.LastRun)
		dto.LastRun = &r
	}
	return dto
}

func toRunRecord(r service.RunRecord) runRecordDTO {
	repos := make([]repoOutcomeDTO, len(r.Repos))
	for i, ro := range r.Repos {
		repos[i] = repoOutcomeDTO{Project: ro.Project, Repo: ro.Repo, Status: ro.Status, Reason: ro.Reason, Bytes: ro.Bytes}
	}
	return runRecordDTO{
		ID:         r.ID,
		Trigger:    r.Trigger.String(),
		Started:    r.Started,
		Ended:      r.Ended,
		OK:         r.OK,
		Warn:       r.Warn,
		Failed:     r.Failed,
		Skipped:    r.Skipped,
		TotalBytes: r.TotalBytes,
		DirBytes:   r.DirBytes,
		RepoCount:  r.RepoCount,
		GitVersion: r.GitVersion,
		LFSVersion: r.LFSVersion,
		StuckDirs:  r.StuckDirs,
		Repos:      repos,
		Out:        r.Out,
		LogPath:    r.LogPath,
		Err:        r.Err,
	}
}

// toGroupConfig builds the config view sent to the dashboard. raw is the
// group exactly as stored in the file (so the edit form round-trips relative
// paths and inherited-empty fields); resolved supplies the effective numeric
// settings and the absolute output path for display.
func toGroupConfig(raw service.GroupConfig, resolved service.ResolvedGroup) groupConfigDTO {
	return groupConfigDTO{
		Repos:       raw.Repos,
		Out:         raw.Out,
		Stage:       raw.Stage,
		OutResolved: resolved.Out,
		CloneDepth:  resolved.CloneDepth,
		Jobs:        resolved.Jobs,
		Retries:     resolved.Retries,
		Timeout:     resolved.Timeout.String(),
		FailFast:    resolved.FailFast,
		SkipChecks:  resolved.SkipChecks,
	}
}

func toLive(l service.LiveProgress) liveDTO {
	repos := make([]repoLiveDTO, len(l.Repos))
	for i, r := range l.Repos {
		repos[i] = repoLiveDTO{
			Index: r.Index, Project: r.Project, Repo: r.Repo,
			Phase: r.Phase, Detail: r.Detail, Pct: r.Pct, Status: r.Status, Started: r.Started,
			BytesDone: r.BytesDone, BytesTotal: r.BytesTotal, LastLine: r.LastLine,
			RetryAttempt: r.RetryAttempt, RetryOf: r.RetryOf, RetryReason: r.RetryReason,
			RetryWaitMs: r.RetryWait.Milliseconds(),
		}
	}
	logTail := make([]logLineDTO, len(l.LogTail))
	for i, ll := range l.LogTail {
		logTail[i] = logLineDTO{Repo: ll.Repo, Level: ll.Level, Line: ll.Line}
	}
	return liveDTO{
		Total: l.Total, Done: l.Done, Repos: repos,
		Out: l.Out, Log: l.Log, Jobs: l.Jobs, Depth: l.Depth, Started: l.Started,
		LogTail: logTail,
	}
}
