package webapi

import (
	"errors"
	"io/fs"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/robfig/cron/v3"

	"github.com/heilingbrunner/clonezip/internal/common/fsx"
	"github.com/heilingbrunner/clonezip/internal/common/pipeline"
	"github.com/heilingbrunner/clonezip/internal/common/repolist"
	"github.com/heilingbrunner/clonezip/internal/service"
)

// api holds the dependencies every handler needs.
type api struct {
	store   *service.Store
	sched   *service.Scheduler
	cfg     *service.ConfigStore
	restore *service.RestoreRunner
	started time.Time

	dirSizeMu    sync.Mutex
	dirSizeCache map[string]dirSizeEntry
}

type dirSizeEntry struct {
	bytes int64
	at    time.Time
}

// dirSizeTTL bounds how stale a cached out-dir size may be. The dashboard polls
// the group list every few seconds; without the cache that would walk every
// backup tree on each poll.
const dirSizeTTL = 30 * time.Second

// dirSize returns the on-disk size of dir, walking it at most once per
// dirSizeTTL. A missing directory counts as zero (a configured group that has
// not run yet); any other error falls back to the last cached value, or reports
// nothing.
func (a *api) dirSize(dir string) (int64, bool) {
	if dir == "" {
		return 0, false
	}

	a.dirSizeMu.Lock()
	cached, hit := a.dirSizeCache[dir]
	a.dirSizeMu.Unlock()
	if hit && time.Since(cached.at) < dirSizeTTL {
		return cached.bytes, true
	}

	n, err := fsx.DirSize(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		if hit {
			return cached.bytes, true
		}
		return 0, false
	}

	a.dirSizeMu.Lock()
	if a.dirSizeCache == nil {
		a.dirSizeCache = make(map[string]dirSizeEntry)
	}
	a.dirSizeCache[dir] = dirSizeEntry{bytes: n, at: time.Now()}
	a.dirSizeMu.Unlock()
	return n, true
}

func (a *api) health(c *gin.Context) {
	snap := a.cfg.Snapshot()
	root := snap.BackupRoot()
	used, free := a.rootStorage(root)
	c.JSON(http.StatusOK, healthDTO{
		Status:         "ok",
		ActionsAllowed: actionsAllowed(c),
		Uptime:         time.Since(a.started).Round(time.Second).String(),
		Title:          snap.Title,
		DateFormat:     snap.DateFormat,
		BackupRoot:     root,
		StorageBytes:   used,
		FreeBytes:      free,
		TotalRepos:     a.totalRepoCount(),
	})
}

// totalRepoCount sums repoCount across every configured group, for the
// dashboard's at-a-glance repository total.
func (a *api) totalRepoCount() int {
	total := 0
	for _, gs := range a.store.Snapshot() {
		total += a.repoCount(gs.Name)
	}
	return total
}

// rootStorage measures the backup root: the size on disk of everything under it
// (lightly cached) and the free space on its volume. This is the only
// disk-usage figure the dashboard shows - no per-group directory is measured.
func (a *api) rootStorage(root string) (int64, *uint64) {
	used, _ := a.dirSize(root)
	free, err := fsx.FreeSpace(root)
	if err != nil {
		// The root may not exist until the first run; the config directory is
		// on the same volume it will be created on.
		if free, err = fsx.FreeSpace(a.cfg.Snapshot().Dir); err != nil {
			return used, nil
		}
	}
	return used, &free
}

func (a *api) listGroups(c *gin.Context) {
	snap := a.store.Snapshot()
	out := make([]groupSummaryDTO, len(snap))
	for i, gs := range snap {
		out[i] = toGroupSummary(gs, a.repoCount(gs.Name))
	}
	c.JSON(http.StatusOK, out)
}

// repoCount looks up how many repos a group is currently configured with, for
// display alongside its schedule - independent of any run's history, so it
// shows even for a group that has never run yet.
func (a *api) repoCount(name string) int {
	if g, ok := a.sched.Group(name); ok {
		return len(repolist.ParseEntries(g.Repos).Entries)
	}
	return 0
}

// groupConfigView pairs a group's stored config with its resolved settings for
// the dashboard: the raw values keep the edit form round-tripping (a relative
// path stays relative), the resolved ones give the effective numbers and the
// absolute output path. Returns false only for a name neither side knows.
func (a *api) groupConfigView(name string) (groupConfigDTO, bool) {
	resolved, ok := a.sched.Group(name)
	if !ok {
		return groupConfigDTO{}, false
	}
	var raw service.GroupConfig
	for _, g := range a.cfg.Snapshot().Groups {
		if g.Name == name {
			raw = g
			break
		}
	}
	return toGroupConfig(raw, resolved), true
}

func (a *api) getGroup(c *gin.Context) {
	name := c.Param("name")
	gs, ok := a.store.Get(name)
	if !ok {
		c.JSON(http.StatusNotFound, errorDTO{Error: "unknown group"})
		return
	}

	detail := groupDetailDTO{
		groupSummaryDTO: toGroupSummary(gs, a.repoCount(name)),
		CurrentRunID:    gs.CurrentRunID,
		HistoryCount:    len(gs.History),
	}
	if live, running := a.store.Live(name); running {
		l := toLive(live)
		detail.Live = &l
	}
	if cfg, ok := a.groupConfigView(name); ok {
		detail.Config = cfg
	}
	c.JSON(http.StatusOK, detail)
}

func (a *api) history(c *gin.Context) {
	name := c.Param("name")
	gs, ok := a.store.Get(name)
	if !ok {
		c.JSON(http.StatusNotFound, errorDTO{Error: "unknown group"})
		return
	}

	history := gs.History
	if raw := c.Query("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 0 && n < len(history) {
			history = history[:n]
		}
	}

	out := make([]runRecordDTO, len(history))
	for i, r := range history {
		out[i] = toRunRecord(r)
	}
	c.JSON(http.StatusOK, out)
}

func (a *api) live(c *gin.Context) {
	name := c.Param("name")
	if _, ok := a.store.Get(name); !ok {
		c.JSON(http.StatusNotFound, errorDTO{Error: "unknown group"})
		return
	}

	live, running := a.store.Live(name)
	if !running {
		c.JSON(http.StatusOK, liveDTO{})
		return
	}
	c.JSON(http.StatusOK, toLive(live))
}

func (a *api) createGroup(c *gin.Context) {
	var dto groupWriteDTO
	if err := c.ShouldBindJSON(&dto); err != nil {
		c.JSON(http.StatusBadRequest, errorDTO{Error: err.Error()})
		return
	}
	gc, ferr := dto.toGroupConfig()
	if ferr != nil {
		c.JSON(http.StatusBadRequest, validationErrorDTO{Error: "invalid request", Fields: []fieldErrorDTO{*ferr}})
		return
	}

	if err := a.cfg.CreateGroup(gc); err != nil {
		writeGroupError(c, err)
		return
	}
	cfg, _ := a.groupConfigView(gc.Name)
	c.Header("Location", "/api/groups/"+gc.Name)
	c.JSON(http.StatusCreated, cfg)
}

func (a *api) updateGroup(c *gin.Context) {
	oldName := c.Param("name")
	var dto groupWriteDTO
	if err := c.ShouldBindJSON(&dto); err != nil {
		c.JSON(http.StatusBadRequest, errorDTO{Error: err.Error()})
		return
	}
	gc, ferr := dto.toGroupConfig()
	if ferr != nil {
		c.JSON(http.StatusBadRequest, validationErrorDTO{Error: "invalid request", Fields: []fieldErrorDTO{*ferr}})
		return
	}

	if err := a.cfg.UpdateGroup(oldName, gc); err != nil {
		writeGroupError(c, err)
		return
	}
	cfg, _ := a.groupConfigView(gc.Name)
	c.JSON(http.StatusOK, cfg)
}

func (a *api) deleteGroup(c *gin.Context) {
	if err := a.cfg.DeleteGroup(c.Param("name")); err != nil {
		writeGroupError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// getSettings is a GET but is refused to clients outside allowActionsFrom
// anyway: unlike the groups overview, it exposes the listen address and
// backup root, which is more than a LAN viewer needs to watch backups run.
func (a *api) getSettings(c *gin.Context) {
	if !actionsAllowed(c) {
		c.AbortWithStatusJSON(http.StatusForbidden, errorDTO{
			Error: "service settings are available on the server host only",
		})
		return
	}
	c.JSON(http.StatusOK, toSettingsDTO(a.cfg.Snapshot()))
}

func (a *api) updateSettings(c *gin.Context) {
	var dto settingsWriteDTO
	if err := c.ShouldBindJSON(&dto); err != nil {
		c.JSON(http.StatusBadRequest, errorDTO{Error: err.Error()})
		return
	}
	s, ferr := dto.toSettings()
	if ferr != nil {
		c.JSON(http.StatusBadRequest, validationErrorDTO{Error: "invalid request", Fields: []fieldErrorDTO{*ferr}})
		return
	}

	if err := a.cfg.UpdateSettings(s); err != nil {
		writeGroupError(c, err)
		return
	}
	c.JSON(http.StatusOK, toSettingsDTO(a.cfg.Snapshot()))
}

// writeGroupError maps a ConfigStore error to the right HTTP status: a
// *service.ValidationError becomes a 400 with field-level detail, a conflict
// (name taken, group running) becomes a 409, an unknown group a 404.
func writeGroupError(c *gin.Context, err error) {
	var verr *service.ValidationError
	switch {
	case errors.As(err, &verr):
		c.JSON(http.StatusBadRequest, toValidationDTO(verr))
	case errors.Is(err, service.ErrGroupExists):
		c.JSON(http.StatusConflict, errorDTO{Error: err.Error()})
	case errors.Is(err, service.ErrGroupRunning):
		c.JSON(http.StatusConflict, errorDTO{Error: "group is running, try again once it finishes"})
	case errors.Is(err, service.ErrUnknownGroup):
		c.JSON(http.StatusNotFound, errorDTO{Error: "unknown group"})
	default:
		c.JSON(http.StatusInternalServerError, errorDTO{Error: err.Error()})
	}
}

func (a *api) triggerRun(c *gin.Context) {
	name := c.Param("name")

	runID, err := a.sched.TriggerNow(name)
	switch {
	case errors.Is(err, service.ErrUnknownGroup):
		c.JSON(http.StatusNotFound, errorDTO{Error: "unknown group"})
	case errors.Is(err, service.ErrAlreadyRunning):
		c.JSON(http.StatusConflict, errorDTO{Error: "already running"})
	case err != nil:
		c.JSON(http.StatusInternalServerError, errorDTO{Error: err.Error()})
	default:
		c.JSON(http.StatusAccepted, triggerResponseDTO{RunID: runID, Started: time.Now()})
	}
}

func (a *api) triggerRunAll(c *gin.Context) {
	status, err := a.sched.TriggerAllNow()
	if errors.Is(err, service.ErrBatchAlreadyRunning) {
		c.JSON(http.StatusConflict, errorDTO{Error: "run-all already in progress"})
		return
	}
	c.JSON(http.StatusAccepted, toBatchStatus(status))
}

func (a *api) getRunAll(c *gin.Context) {
	c.JSON(http.StatusOK, toBatchStatus(a.sched.BatchStatus()))
}

// listArchives lists every repository a group holds a backup of - the picker
// the restore dialog is built on.
func (a *api) listArchives(c *gin.Context) {
	name := c.Param("name")
	g, ok := a.sched.Group(name)
	if !ok {
		c.JSON(http.StatusNotFound, errorDTO{Error: "unknown group"})
		return
	}

	found, err := service.LatestArchives(g.Out)
	if err != nil {
		c.JSON(http.StatusInternalServerError, errorDTO{Error: err.Error()})
		return
	}

	out := make([]archiveDTO, len(found))
	for i, f := range found {
		out[i] = toArchive(f)
	}
	c.JSON(http.StatusOK, out)
}

func (a *api) triggerRestore(c *gin.Context) {
	var dto restoreWriteDTO
	if err := c.ShouldBindJSON(&dto); err != nil {
		c.JSON(http.StatusBadRequest, errorDTO{Error: err.Error()})
		return
	}
	if ferr := validateRestore(dto); ferr != nil {
		c.JSON(http.StatusBadRequest, validationErrorDTO{Error: "invalid request", Fields: []fieldErrorDTO{*ferr}})
		return
	}

	g, ok := a.sched.Group(dto.Group)
	if !ok {
		c.JSON(http.StatusNotFound, errorDTO{Error: "unknown group"})
		return
	}
	archives, err := service.LatestArchives(g.Out)
	if err != nil {
		c.JSON(http.StatusInternalServerError, errorDTO{Error: err.Error()})
		return
	}
	archive, ok := findArchive(archives, dto.Project, dto.Repo)
	if !ok {
		c.JSON(http.StatusNotFound, errorDTO{Error: "no archive for that repository"})
		return
	}

	req := service.RestoreRequest{
		Group:          dto.Group,
		Project:        dto.Project,
		Repo:           dto.Repo,
		Dest:           filepath.Clean(dto.Dest),
		Origin:         dto.Origin,
		Clone:          dto.Clone,
		Force:          dto.Force,
		ConfirmShallow: dto.ConfirmShallow,
		NoVerify:       dto.NoVerify,
		SkipChecks:     dto.SkipChecks,
	}
	if err := a.restore.Trigger(req, archive); err != nil {
		c.JSON(http.StatusConflict, errorDTO{Error: err.Error()})
		return
	}
	c.JSON(http.StatusAccepted, toRestoreStatus(a.restore.Status()))
}

func (a *api) getRestore(c *gin.Context) {
	c.JSON(http.StatusOK, toRestoreStatus(a.restore.Status()))
}

func findArchive(archives []service.ArchiveInfo, project, repo string) (service.ArchiveInfo, bool) {
	for _, a := range archives {
		if a.Project == project && a.Repo == repo {
			return a, true
		}
	}
	return service.ArchiveInfo{}, false
}

// validateRestore checks the fields the dashboard supplies. The destination
// is deliberately not confined to the backup root - restoring elsewhere is
// the point - but it must be an explicit absolute path, so a typo can never
// be resolved against whatever working directory the service was started in.
func validateRestore(dto restoreWriteDTO) *fieldErrorDTO {
	switch {
	case strings.TrimSpace(dto.Group) == "":
		return &fieldErrorDTO{Field: "group", Msg: "pick a group"}
	case strings.TrimSpace(dto.Repo) == "":
		return &fieldErrorDTO{Field: "repo", Msg: "pick a repository"}
	case strings.TrimSpace(dto.Dest) == "":
		return &fieldErrorDTO{Field: "dest", Msg: "required"}
	case !filepath.IsAbs(dto.Dest):
		return &fieldErrorDTO{Field: "dest", Msg: "must be an absolute path"}
	}
	switch dto.Origin {
	case pipeline.OriginSource, pipeline.OriginBare:
	default:
		return &fieldErrorDTO{Field: "origin", Msg: `must be "source" or "bare"`}
	}
	if dto.Origin == pipeline.OriginBare && !dto.Clone {
		return &fieldErrorDTO{Field: "origin", Msg: "only applies with \"Clone a working copy\""}
	}
	return nil
}

// previewCron parses a cron expression with the same parser
// service.Config.Validate uses, so a preview can never accept something Save
// would later reject (or vice versa). It's stateless - no group lookup.
func (a *api) previewCron(c *gin.Context) {
	expr := c.Query("expr")
	sched, err := cron.ParseStandard(expr)
	if err != nil {
		c.JSON(http.StatusOK, cronPreviewDTO{Valid: false, Error: err.Error()})
		return
	}

	const previewCount = 5
	runs := make([]string, 0, previewCount)
	from := time.Now()
	for range previewCount {
		from = sched.Next(from)
		runs = append(runs, from.Format(time.RFC3339))
	}
	c.JSON(http.StatusOK, cronPreviewDTO{Valid: true, NextRuns: runs})
}
