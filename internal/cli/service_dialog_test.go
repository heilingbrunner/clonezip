package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/heilingbrunner/clonezip/internal/service"
)

func templateConfig(t *testing.T) service.Config {
	t.Helper()
	var cfg service.Config
	if err := yaml.Unmarshal(serviceConfigTemplate, &cfg); err != nil {
		t.Fatalf("parse embedded template: %v", err)
	}
	if len(cfg.Groups) == 0 {
		t.Fatal("embedded template has no groups; init would write a config that cannot load")
	}
	return cfg
}

// Seeding the form from the template and applying it back unedited must be a
// no-op: anything the pair drops would silently disappear from the config
// that "service init -i" writes.
func TestServiceInitAnswersRoundTrip(t *testing.T) {
	want := templateConfig(t)
	got := templateConfig(t)

	if err := serviceInitAnswersFromConfig(got).applyTo(&got); err != nil {
		t.Fatalf("applyTo: %v", err)
	}

	wantYAML, err := yaml.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	gotYAML, err := yaml.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotYAML) != string(wantYAML) {
		t.Errorf("round trip changed the config:\n got:\n%s\nwant:\n%s", gotYAML, wantYAML)
	}
}

func TestServiceInitAnswersApplyTo(t *testing.T) {
	cfg := templateConfig(t)

	a := serviceInitDialogAnswers{
		Title:        "Edited",
		DateFormat:   "de-DE",
		Listen:       "127.0.0.1:9000",
		Out:          "./elsewhere",
		CloneDepth:   "0",
		Jobs:         "9",
		Retries:      "5",
		Timeout:      "1h15m",
		HistoryLimit: "40",
		Options:      []string{optFailFast, optSkipCheck},
	}
	if err := a.applyTo(&cfg); err != nil {
		t.Fatalf("applyTo: %v", err)
	}

	if cfg.Title != "Edited" || cfg.DateFormat != "de-DE" ||
		cfg.Listen != "127.0.0.1:9000" || cfg.Out != "./elsewhere" {
		t.Errorf("service settings = %q/%q/%q/%q, want the edited values",
			cfg.Title, cfg.DateFormat, cfg.Listen, cfg.Out)
	}

	want := service.GroupDefaults{
		CloneDepth: 0, Jobs: 9, Retries: 5,
		Timeout: 75 * time.Minute, FailFast: true, SkipChecks: true, HistoryLimit: 40,
	}
	if cfg.Defaults != want {
		t.Errorf("defaults = %+v, want %+v", cfg.Defaults, want)
	}
}

// An unticked option must clear the flag, not just fail to set it.
func TestServiceInitAnswersApplyToClearsUntickedOptions(t *testing.T) {
	cfg := templateConfig(t)
	cfg.Defaults.FailFast = true
	cfg.Defaults.SkipChecks = true

	a := serviceInitAnswersFromConfig(cfg)
	a.Options = nil
	if err := a.applyTo(&cfg); err != nil {
		t.Fatalf("applyTo: %v", err)
	}

	if cfg.Defaults.FailFast || cfg.Defaults.SkipChecks {
		t.Errorf("failFast=%v skipChecks=%v, want both cleared",
			cfg.Defaults.FailFast, cfg.Defaults.SkipChecks)
	}
}

func TestServiceInitAnswersApplyToRejectsBadInput(t *testing.T) {
	tests := []struct {
		name string
		want string
		mut  func(*serviceInitDialogAnswers)
	}{
		{"clone depth", "clone depth", func(a *serviceInitDialogAnswers) { a.CloneDepth = "deep" }},
		{"jobs", "jobs", func(a *serviceInitDialogAnswers) { a.Jobs = "" }},
		{"retries", "retries", func(a *serviceInitDialogAnswers) { a.Retries = "3.5" }},
		{"timeout", "timeout", func(a *serviceInitDialogAnswers) { a.Timeout = "30x" }},
		{"history limit", "history limit", func(a *serviceInitDialogAnswers) { a.HistoryLimit = "many" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := templateConfig(t)
			a := serviceInitAnswersFromConfig(cfg)
			tc.mut(&a)

			err := a.applyTo(&cfg)
			if err == nil {
				t.Fatalf("applyTo accepted an invalid %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to name %q", err, tc.want)
			}
		})
	}
}

// The form covers neither groups nor the retired defaults.out key, and
// writing either would produce a file that no longer loads.
func TestServiceInitAppliedConfigPreservesGroups(t *testing.T) {
	cfg := templateConfig(t)
	before := cfg.Groups

	a := serviceInitAnswersFromConfig(cfg)
	a.Out = "./somewhere-else"
	if err := a.applyTo(&cfg); err != nil {
		t.Fatalf("applyTo: %v", err)
	}

	if !slices.EqualFunc(cfg.Groups, before, func(x, y service.GroupConfig) bool {
		return x.Name == y.Name && x.Schedule == y.Schedule && x.Out == y.Out &&
			slices.Equal(x.Repos, y.Repos)
	}) {
		t.Errorf("groups = %+v, want them unchanged", cfg.Groups)
	}
	if cfg.Defaults.Out != "" {
		t.Errorf("defaults.out = %q, want it left empty (the key is retired)", cfg.Defaults.Out)
	}
}

// What the form produces must survive Save and load back through the same
// path the service itself uses.
func TestServiceInitAppliedConfigSavesAndLoads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "clonezip-service.yaml")

	cfg := templateConfig(t)
	a := serviceInitAnswersFromConfig(cfg)
	a.Title = "Saved"
	a.Listen = ":9191"
	a.Jobs = "7"
	if err := a.applyTo(&cfg); err != nil {
		t.Fatalf("applyTo: %v", err)
	}
	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	var reloaded service.Config
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(data, &reloaded); err != nil {
		t.Fatalf("written config does not parse: %v", err)
	}
	if reloaded.Title != "Saved" || reloaded.Listen != ":9191" || reloaded.Defaults.Jobs != 7 {
		t.Errorf("written config = %q/%q/jobs %d, want the edited values",
			reloaded.Title, reloaded.Listen, reloaded.Defaults.Jobs)
	}
	if len(reloaded.Groups) != len(cfg.Groups) {
		t.Errorf("written groups = %d, want %d", len(reloaded.Groups), len(cfg.Groups))
	}
}

func TestValidateLocale(t *testing.T) {
	v := validateLocale("date format")
	for _, ok := range []string{"en", "en-US", "de-DE", "zh-Hans-CN"} {
		if err := v(ok); err != nil {
			t.Errorf("validateLocale(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "nope!", "en_US", "en-", "-US"} {
		if err := v(bad); err == nil {
			t.Errorf("validateLocale(%q) = nil, want an error", bad)
		}
	}
}

func TestValidateListenAddr(t *testing.T) {
	v := validateListenAddr("listen address")
	for _, ok := range []string{":8091", "127.0.0.1:8091", "localhost:80", "[::1]:8091"} {
		if err := v(ok); err != nil {
			t.Errorf("validateListenAddr(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "8091", "127.0.0.1"} {
		if err := v(bad); err == nil {
			t.Errorf("validateListenAddr(%q) = nil, want an error", bad)
		}
	}
}

func TestValidateRequired(t *testing.T) {
	v := validateRequired("backup root")
	if err := v("./backups"); err != nil {
		t.Errorf("validateRequired(%q) = %v, want nil", "./backups", err)
	}
	for _, bad := range []string{"", "   "} {
		if err := v(bad); err == nil {
			t.Errorf("validateRequired(%q) = nil, want an error", bad)
		}
	}
}
