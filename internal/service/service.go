package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/heilingbrunner/clonezip/internal/common/gitx"
)

// shutdownGrace bounds how long the HTTP server is given to finish in-flight
// requests once the service starts shutting down.
const shutdownGrace = 10 * time.Second

// Service ties the config, status store and scheduler together. It has no
// notion of HTTP itself - Run accepts a handler built by the caller (see
// internal/webapi and internal/cli/service.go) so this package stays as free
// of presentation concerns as internal/pipeline is of internal/ui.
type Service struct {
	Config      Config
	Store       *Store
	Scheduler   *Scheduler
	ConfigStore *ConfigStore
	Restore     *RestoreRunner
}

// New builds a Service from a validated config. configPath is where
// ConfigStore persists edits made through it - normally the same file cfg
// was loaded from.
func New(cfg Config, version, configPath string) *Service {
	store := NewStore(cfg.ResolvedGroups())
	git := gitx.New(gitx.NewExecRunner())
	sched := NewScheduler(cfg, store, git, version)
	cfgStore := NewConfigStore(cfg, configPath, sched, store)
	return &Service{
		Config:      cfg,
		Store:       store,
		Scheduler:   sched,
		ConfigStore: cfgStore,
		Restore:     NewRestoreRunner(git),
	}
}

// Run starts the scheduler and an HTTP server on addr serving handler, and
// blocks until ctx is cancelled or the server fails to start. Shutdown is
// graceful: the HTTP server stops accepting new connections and is given
// shutdownGrace to finish in-flight ones, and the scheduler waits for any
// run in progress - cron-fired or manually triggered - to finish.
func (s *Service) Run(ctx context.Context, addr string, handler http.Handler) error {
	if err := s.Scheduler.Start(ctx); err != nil {
		return fmt.Errorf("start scheduler: %w", err)
	}
	s.Restore.Start(ctx)

	httpServer := &http.Server{Addr: addr, Handler: handler}
	serveErr := make(chan error, 1)
	go func() { serveErr <- httpServer.ListenAndServe() }()

	var runErr error
	select {
	case <-ctx.Done():
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			runErr = fmt.Errorf("listen on %s: %w", addr, err)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil && runErr == nil {
		runErr = fmt.Errorf("shut down http server: %w", err)
	}

	s.Scheduler.Stop()
	s.Restore.Wait()
	return runErr
}
