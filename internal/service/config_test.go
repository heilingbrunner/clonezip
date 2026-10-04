package service

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/heilingbrunner/clonezip/internal/common/gitx"
)

func TestConfigRelToRoot(t *testing.T) {
	// A real OS-rooted directory, not a hardcoded Unix path: filepath.IsAbs
	// requires a drive letter on Windows, so a bare "/srv/clonezip" isn't
	// absolute there and every check below would silently take the wrong branch.
	dir := filepath.Join(t.TempDir(), "clonezip")
	c := Config{Dir: dir} // rootOut() -> <dir>/backups
	root := c.rootOut()
	outside := filepath.Join(filepath.Dir(dir), "elsewhere")

	tests := []struct {
		name string
		in   string
		want string
	}{
		{"absolute inside root", filepath.Join(root, "grp"), "./grp"},
		{"the root itself", root, "."},
		{"bare relative gets ./ prefix", "grp", "./grp"},
		{"relative already prefixed", "./grp", "./grp"},
		{"parent relative kept for Validate to reject", "../escape", "../escape"},
		{"empty", "", ""},
		{"absolute outside root", outside, outside},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.relToRoot(tc.in); got != tc.want {
				t.Errorf("relToRoot(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}

	// relToRoot must undo underRoot for a path inside the root, and be stable.
	if got := c.relToRoot(c.underRoot("out/here")); got != "./out/here" {
		t.Errorf("relToRoot(underRoot(%q)) = %q, want %q", "out/here", got, "./out/here")
	}
	if got := c.relToRoot("./out/here"); got != "./out/here" {
		t.Errorf("relToRoot not idempotent: %q", got)
	}
}

func TestResolveGroupOutUnderRoot(t *testing.T) {
	c := Config{Dir: filepath.FromSlash("/srv/clonezip"), Out: "backups"}
	root := c.rootOut()

	if got := c.Resolve(GroupConfig{Name: "demos"}).Out; got != filepath.Join(root, "demos") {
		t.Errorf("unset out = %q, want <root>/demos", got)
	}
	if got := c.Resolve(GroupConfig{Name: "demos", Out: "sub/dir"}).Out; got != filepath.Join(root, "sub", "dir") {
		t.Errorf("relative out = %q, want <root>/sub/dir", got)
	}
}

func TestValidateRejectsGroupOutEscapingRoot(t *testing.T) {
	// A real OS-rooted directory, not a hardcoded Unix path: filepath.IsAbs
	// requires a drive letter on Windows, so a bare "/srv/clonezip" isn't
	// absolute there and the absolute-outside-root case below would silently
	// be treated as relative (and land inside the root) instead of rejected.
	dir := filepath.Join(t.TempDir(), "clonezip")
	outside := filepath.Join(filepath.Dir(dir), "anywhere")
	mk := func(out string) Config {
		return Config{
			Dir: dir, Out: "backups",
			Groups: []GroupConfig{{
				Name: "g", Schedule: "0 3 * * *", Out: out,
				Repos: []string{"https://github.com/octocat/Hello-World.git"},
			}},
		}
	}
	for _, out := range []string{"../outside", "../../etc", outside} {
		if err := mk(out).Validate(); err == nil {
			t.Errorf("Validate accepted out %q that escapes the backup root", out)
		}
	}
	if err := mk("nested/ok").Validate(); err != nil {
		t.Errorf("Validate rejected a valid nested out: %v", err)
	}
}

func TestLoadConfigRejectsLegacyDefaultsOut(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "clonezip-service.yaml")
	body := "listen: :8091\n" +
		"defaults:\n  out: ./backups\n" +
		"groups:\n  - name: g\n    schedule: 0 3 * * *\n    repos:\n      - https://github.com/octocat/Hello-World.git\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("LoadConfig accepted a config with the removed defaults.out key")
	}
}

func groupNames(groups []GroupConfig) []string {
	names := make([]string, len(groups))
	for i, g := range groups {
		names[i] = g.Name
	}
	return names
}

func TestSortGroupsAscending(t *testing.T) {
	in := []GroupConfig{{Name: "beta"}, {Name: "Alpha"}, {Name: "alpha"}, {Name: "gamma"}}
	got := groupNames(sortGroupsAscending(in))
	want := []string{"Alpha", "alpha", "beta", "gamma"}
	if !slices.Equal(got, want) {
		t.Errorf("sorted = %v, want %v", got, want)
	}
	if orig := groupNames(in); orig[0] != "beta" {
		t.Errorf("input was mutated: %v", orig)
	}
}

func TestSaveWritesGroupsAscendingByName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "clonezip-service.yaml")
	mk := func(name string) GroupConfig {
		return GroupConfig{Name: name, Schedule: "0 3 * * *", Repos: []string{"https://github.com/octocat/Hello-World.git"}}
	}
	cfg := Config{Dir: dir, Out: "backups", Groups: []GroupConfig{mk("zeta"), mk("alpha"), mk("Mid")}}
	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if got := groupNames(cfg.Groups); got[0] != "zeta" {
		t.Errorf("Save mutated the caller's config: %v", got)
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	want := []string{"alpha", "Mid", "zeta"}
	if got := groupNames(loaded.Groups); !slices.Equal(got, want) {
		t.Errorf("groups on disk = %v, want %v", got, want)
	}
}

func TestConfigStoreCreateGroupKeepsFileSorted(t *testing.T) {
	dir := t.TempDir()
	cs, path := newTestConfigStore(t, settingsTestConfig(dir)) // holds "grp"
	err := cs.CreateGroup(GroupConfig{
		Name: "aaa", Schedule: "0 4 * * *",
		Repos: []string{"https://github.com/octocat/Hello-World.git"},
	})
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got, want := groupNames(loaded.Groups), []string{"aaa", "grp"}; !slices.Equal(got, want) {
		t.Errorf("groups on disk = %v, want %v", got, want)
	}
}

func TestConfigStoreUpdateGroupStoresRootRelativeOut(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "clonezip-service.yaml")

	cfg := Config{
		Dir: dir,
		Out: "backups",
		Groups: []GroupConfig{{
			Name:     "grp",
			Schedule: "0 3 * * *",
			Out:      "grp",
			Repos:    []string{"https://github.com/octocat/Hello-World.git"},
		}},
	}
	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	store := NewStore(loaded.ResolvedGroups())
	sched := NewScheduler(loaded, store, gitx.New(gitx.NewFakeRunner()), "test")
	cs := NewConfigStore(loaded, path, sched, store)

	// The dashboard sends back the resolved absolute path it was prefilled with.
	g := loaded.Groups[0]
	g.Out = loaded.Resolve(g).Out
	if err := cs.UpdateGroup("grp", g); err != nil {
		t.Fatalf("UpdateGroup: %v", err)
	}

	after, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig after update: %v", err)
	}
	if got := after.Groups[0].Out; got != "./grp" {
		t.Errorf("stored out = %q, want %q (relative to the backup root)", got, "./grp")
	}
}

// newTestConfigStore writes cfg to a temp clonezip-service.yaml, reloads it
// through the normal LoadConfig path, and wires up the Store/Scheduler pair a
// ConfigStore needs. It returns the store and the config path.
func newTestConfigStore(t *testing.T, cfg Config) (*ConfigStore, string) {
	t.Helper()
	path := filepath.Join(cfg.Dir, "clonezip-service.yaml")
	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	store := NewStore(loaded.ResolvedGroups())
	sched := NewScheduler(loaded, store, gitx.New(gitx.NewFakeRunner()), "test")
	return NewConfigStore(loaded, path, sched, store), path
}

func settingsTestConfig(dir string) Config {
	return Config{
		Dir:      dir,
		Out:      "backups",
		Listen:   ":8091",
		Title:    "Before",
		Defaults: GroupDefaults{CloneDepth: 1, Jobs: 4, Retries: 2, Timeout: 30 * time.Minute, HistoryLimit: 10},
		Groups: []GroupConfig{{
			Name:     "grp",
			Schedule: "0 3 * * *",
			Out:      "./grp",
			Repos:    []string{"https://github.com/octocat/Hello-World.git"},
		}},
	}
}

func TestConfigStoreUpdateSettingsPersists(t *testing.T) {
	dir := t.TempDir()
	cs, path := newTestConfigStore(t, settingsTestConfig(dir))

	want := Settings{
		Title:      "After",
		DateFormat: "de-DE",
		Listen:     "127.0.0.1:9000",
		Defaults: GroupDefaults{
			CloneDepth: 0, Jobs: 8, Retries: 5,
			Timeout: 45 * time.Minute, FailFast: true, SkipChecks: true, HistoryLimit: 25,
		},
	}
	if err := cs.UpdateSettings(want); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}

	after, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig after update: %v", err)
	}
	if got := after.SettingsOf(); got != want {
		t.Errorf("settings on disk = %+v, want %+v", got, want)
	}
	if got := cs.Snapshot().SettingsOf(); got != want {
		t.Errorf("in-memory settings = %+v, want %+v", got, want)
	}
}

func TestConfigStoreUpdateSettingsPreservesGroupsAndBackupRoot(t *testing.T) {
	dir := t.TempDir()
	cfg := settingsTestConfig(dir)
	cs, path := newTestConfigStore(t, cfg)

	s := cs.Snapshot().SettingsOf()
	s.Title = "renamed"
	if err := cs.UpdateSettings(s); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}

	after, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig after update: %v", err)
	}
	if after.Out != cfg.Out {
		t.Errorf("backup root = %q, want %q (settings must not touch it)", after.Out, cfg.Out)
	}
	if len(after.Groups) != 1 || after.Groups[0].Name != "grp" || after.Groups[0].Out != "./grp" {
		t.Errorf("groups = %+v, want the original single group unchanged", after.Groups)
	}
	if !slices.Equal(after.Groups[0].Repos, cfg.Groups[0].Repos) {
		t.Errorf("repos = %v, want %v", after.Groups[0].Repos, cfg.Groups[0].Repos)
	}
}

func TestConfigStoreUpdateSettingsRejectsInvalid(t *testing.T) {
	valid := Settings{Listen: ":8091", DateFormat: "en-US",
		Defaults: GroupDefaults{Jobs: 4, Timeout: time.Minute, HistoryLimit: 10}}

	tests := []struct {
		name  string
		field string
		mut   func(*Settings)
	}{
		{"bad locale", "dateFormat", func(s *Settings) { s.DateFormat = "not a locale!" }},
		{"bad listen", "listen", func(s *Settings) { s.Listen = "8091" }},
		{"negative clone depth", "cloneDepth", func(s *Settings) { s.Defaults.CloneDepth = -1 }},
		{"zero jobs", "jobs", func(s *Settings) { s.Defaults.Jobs = 0 }},
		{"negative retries", "retries", func(s *Settings) { s.Defaults.Retries = -1 }},
		{"zero timeout", "timeout", func(s *Settings) { s.Defaults.Timeout = 0 }},
		{"zero history limit", "historyLimit", func(s *Settings) { s.Defaults.HistoryLimit = 0 }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cs, path := newTestConfigStore(t, settingsTestConfig(dir))
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}

			s := valid
			tc.mut(&s)
			err = cs.UpdateSettings(s)

			var verr *ValidationError
			if !errors.As(err, &verr) {
				t.Fatalf("UpdateSettings(%s) error = %v, want *ValidationError", tc.name, err)
			}
			if len(verr.Fields) != 1 || verr.Fields[0].Field != tc.field {
				t.Errorf("reported fields = %+v, want a single %q error", verr.Fields, tc.field)
			}

			// A rejected edit must not have touched the file on disk.
			afterBytes, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, afterBytes) {
				t.Error("config file changed despite the edit being rejected")
			}
		})
	}
}

func TestConfigStoreUpdateSettingsPropagatesDefaultsToGroups(t *testing.T) {
	dir := t.TempDir()
	cfg := settingsTestConfig(dir)
	// A second group pins its own values, so the test also proves an explicit
	// override still wins over the new defaults.
	jobs, limit := 2, 3
	cfg.Groups = append(cfg.Groups, GroupConfig{
		Name: "pinned", Schedule: "0 4 * * *", Out: "./pinned",
		Jobs: &jobs, HistoryLimit: &limit,
		Repos: []string{"https://github.com/octocat/Spoon-Knife.git"},
	})
	cs, _ := newTestConfigStore(t, cfg)

	s := cs.Snapshot().SettingsOf()
	s.Defaults.Jobs = 9
	s.Defaults.HistoryLimit = 40
	if err := cs.UpdateSettings(s); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}

	next := cs.Snapshot()
	inherited := next.Resolve(next.Groups[0])
	if inherited.Jobs != 9 || inherited.HistoryLimit != 40 {
		t.Errorf("inheriting group resolved jobs=%d historyLimit=%d, want 9 and 40",
			inherited.Jobs, inherited.HistoryLimit)
	}
	pinned := next.Resolve(next.Groups[1])
	if pinned.Jobs != jobs || pinned.HistoryLimit != limit {
		t.Errorf("pinned group resolved jobs=%d historyLimit=%d, want %d and %d",
			pinned.Jobs, pinned.HistoryLimit, jobs, limit)
	}
}

func TestConfigStoreUpdateSettingsDropsLegacyDefaultsOut(t *testing.T) {
	dir := t.TempDir()
	cs, path := newTestConfigStore(t, settingsTestConfig(dir))

	s := cs.Snapshot().SettingsOf()
	// Whatever a caller puts here, writing it back would make the config
	// unloadable - LoadConfig rejects the retired defaults.out key outright.
	s.Defaults.Out = "./somewhere"
	if err := cs.UpdateSettings(s); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
	if _, err := LoadConfig(path); err != nil {
		t.Fatalf("config became unloadable after a settings save: %v", err)
	}
}

func TestSortReposAscending(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "unordered enabled URLs",
			in: []string{
				"https://dev.azure.com/org/proj/_git/charlie",
				"https://dev.azure.com/org/proj/_git/alpha",
				"https://dev.azure.com/org/proj/_git/bravo",
			},
			want: []string{
				"https://dev.azure.com/org/proj/_git/alpha",
				"https://dev.azure.com/org/proj/_git/bravo",
				"https://dev.azure.com/org/proj/_git/charlie",
			},
		},
		{
			name: "case-insensitive ordering",
			in: []string{
				"https://dev.azure.com/org/proj/_git/Zeta",
				"https://dev.azure.com/org/proj/_git/alpha",
			},
			want: []string{
				"https://dev.azure.com/org/proj/_git/alpha",
				"https://dev.azure.com/org/proj/_git/Zeta",
			},
		},
		{
			name: "disabled entry interleaves by URL",
			in: []string{
				"https://dev.azure.com/org/proj/_git/charlie",
				"# https://dev.azure.com/org/proj/_git/bravo",
				"https://dev.azure.com/org/proj/_git/alpha",
			},
			want: []string{
				"https://dev.azure.com/org/proj/_git/alpha",
				"# https://dev.azure.com/org/proj/_git/bravo",
				"https://dev.azure.com/org/proj/_git/charlie",
			},
		},
		{
			name: "already sorted is unchanged",
			in: []string{
				"https://dev.azure.com/org/proj/_git/alpha",
				"https://dev.azure.com/org/proj/_git/bravo",
			},
			want: []string{
				"https://dev.azure.com/org/proj/_git/alpha",
				"https://dev.azure.com/org/proj/_git/bravo",
			},
		},
		{
			name: "same URL key is deterministic",
			in: []string{
				"https://dev.azure.com/org/proj/_git/alpha",
				"# https://dev.azure.com/org/proj/_git/alpha",
			},
			want: []string{
				"# https://dev.azure.com/org/proj/_git/alpha",
				"https://dev.azure.com/org/proj/_git/alpha",
			},
		},
		{
			name: "empty",
			in:   nil,
			want: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			orig := slices.Clone(tt.in)
			got := sortReposAscending(tt.in)
			if !slices.Equal(got, tt.want) {
				t.Errorf("sortReposAscending() = %q, want %q", got, tt.want)
			}
			if !slices.Equal(tt.in, orig) {
				t.Errorf("input mutated: %q, was %q", tt.in, orig)
			}
		})
	}
}
