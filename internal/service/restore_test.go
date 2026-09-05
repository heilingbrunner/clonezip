package service

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/heilingbrunner/clonezip/internal/common/pipeline"
)

func TestRestoreRunnerRefusesASecondRestore(t *testing.T) {
	r := NewRestoreRunner(nil)

	// Trigger without letting the goroutine finish would race; drive the
	// guard directly instead by marking one in flight.
	r.status = RestoreStatus{Running: true}

	err := r.Trigger(RestoreRequest{Group: "demos", Repo: "alpha"}, ArchiveInfo{Repo: "alpha"})
	if !errors.Is(err, ErrRestoreAlreadyRunning) {
		t.Fatalf("got %v, want ErrRestoreAlreadyRunning", err)
	}
}

func TestRestoreReporterFoldsEventsIntoStatus(t *testing.T) {
	r := NewRestoreRunner(nil)
	rep := &restoreReporter{runner: r}

	rep.Report(pipeline.RepoPhase{Index: 0, Phase: pipeline.PhaseExtract, Detail: "12 refs"})
	rep.Report(pipeline.RepoPercent{Index: 0, Pct: 42})
	rep.Report(pipeline.RepoLog{Index: 0, Level: pipeline.LevelWarn, Line: "heads up"})

	got := r.Status()
	if got.Pct != 42 {
		t.Errorf("pct = %d, want 42", got.Pct)
	}
	if got.Phase != pipeline.PhaseExtract.String()+" (12 refs)" {
		t.Errorf("phase = %q, want the phase with its detail", got.Phase)
	}
	if len(got.LogTail) != 1 || got.LogTail[0].Level != "warn" || got.LogTail[0].Line != "heads up" {
		t.Errorf("log tail = %+v, want one warn line", got.LogTail)
	}
}

func TestRestoreRunnerLogTailIsBounded(t *testing.T) {
	r := NewRestoreRunner(nil)
	for i := 0; i < logTailLimit+50; i++ {
		r.appendLog("info", "line")
	}
	if got := len(r.Status().LogTail); got != logTailLimit {
		t.Errorf("log tail kept %d lines, want %d", got, logTailLimit)
	}
}

func TestDashboardMessagePointsAtCheckboxes(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"shallow", fmt.Errorf("%w: pass --confirm-shallow", pipeline.ErrShallowNeedsConfirm), "Confirm truncated/shallow history"},
		{"exists", fmt.Errorf("%w: remove it or pass --force", pipeline.ErrTargetExists), "Force overwrite of an existing target"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dashboardMessage(tt.err)
			if !strings.Contains(got, tt.want) {
				t.Errorf("got %q, want it to name the %q checkbox", got, tt.want)
			}
			if strings.Contains(got, "--") {
				t.Errorf("got %q, want no CLI flag in a dashboard message", got)
			}
		})
	}
}

func TestDashboardMessagePassesOtherErrorsThrough(t *testing.T) {
	if got := dashboardMessage(errors.New("disk full")); got != "disk full" {
		t.Errorf("got %q, want the error unchanged", got)
	}
}
