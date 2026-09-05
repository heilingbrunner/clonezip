package webapi

import (
	"time"

	"github.com/gin-gonic/gin"

	"github.com/heilingbrunner/clonezip/internal/service"
)

// NewServer builds the gin engine: a health check, the JSON API under /api,
// and the embedded static dashboard for everything else.
func NewServer(store *service.Store, sched *service.Scheduler, cfgStore *service.ConfigStore, restore *service.RestoreRunner) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)

	r := gin.New()

	// Client IPs must come from the TCP peer alone. Gin trusts every proxy by
	// default, which makes ClientIP() believe X-Forwarded-For - and would let
	// any remote client claim to be 127.0.0.1 and walk straight past
	// actionGuard. Both settings are needed: one empties the trusted list, the
	// other stops the header being consulted at all.
	r.ForwardedByClientIP = false
	// Never fails for a nil list; the parse errors it reports come from
	// malformed entries.
	_ = r.SetTrustedProxies(nil)

	// allowActionsFrom is a file-only setting - nothing at runtime can change
	// it - so the allowlist is resolved once here rather than per request.
	r.Use(gin.Recovery(), actionGuard(cfgStore.Snapshot().ActionAllowList()))

	a := &api{
		store: store, sched: sched, cfg: cfgStore, restore: restore, started: time.Now(),
		dirSizeCache: make(map[string]dirSizeEntry),
	}

	r.GET("/healthz", a.health)

	groups := r.Group("/api/groups")
	groups.GET("", a.listGroups)
	groups.POST("", a.createGroup)
	groups.GET("/:name", a.getGroup)
	groups.PUT("/:name", a.updateGroup)
	groups.DELETE("/:name", a.deleteGroup)
	groups.GET("/:name/history", a.history)
	groups.GET("/:name/live", a.live)
	groups.POST("/:name/run", a.triggerRun)
	groups.GET("/:name/archives", a.listArchives)

	r.POST("/api/run-all", a.triggerRunAll)
	r.GET("/api/run-all", a.getRunAll)

	r.POST("/api/restore", a.triggerRestore)
	r.GET("/api/restore", a.getRestore)

	r.GET("/api/settings", a.getSettings)
	r.PUT("/api/settings", a.updateSettings)

	r.GET("/api/cron/preview", a.previewCron)

	mountStatic(r)

	return r
}
