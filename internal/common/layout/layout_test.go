package layout

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixedClock pins the run time so directory names are deterministic.
func fixedClock() Clock {
	stamp := time.Date(2026, 7, 27, 16, 42, 11, 0, time.FixedZone("CEST", 2*60*60))
	return func() time.Time { return stamp }
}

func newTestPlan(t *testing.T, listPath, baseDir string) Plan {
	t.Helper()
	plan, err := NewPlan(listPath, baseDir, "", "", fixedClock())
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}
	return plan
}

func TestNewPlanNamesRunDirectoryByDateAndList(t *testing.T) {
	base := t.TempDir()
	plan := newTestPlan(t, filepath.Join("somewhere", "azuredevops-contoso.txt"), base)

	want := filepath.Join(base, "20260727-164211-azuredevops-contoso")
	if plan.Out != want {
		t.Errorf("Out = %q, want %q", plan.Out, want)
	}
	// Staging defaults inside the run directory so it lands on the same volume
	// as the output rather than on whatever drive holds the temp directory.
	if got, want := plan.Stage, filepath.Join(plan.Out, ".stage"); got != want {
		t.Errorf("Stage = %q, want %q", got, want)
	}
}

func TestNewPlanMakesEveryPathAbsolute(t *testing.T) {
	// Go applies the Windows \\?\ long-path prefix only to absolute paths, so
	// this is what gives the whole program long-path support.
	plan, err := NewPlan("list.txt", ".", "", "", fixedClock())
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}
	for name, path := range map[string]string{
		"Out":      plan.Out,
		"Stage":    plan.Stage,
		"ListPath": plan.ListPath,
	} {
		if !filepath.IsAbs(path) {
			t.Errorf("%s = %q, want an absolute path", name, path)
		}
	}
}

func TestNewPlanHonoursOverrides(t *testing.T) {
	base := t.TempDir()
	out := filepath.Join(base, "custom-out")
	stage := filepath.Join(base, "custom-stage")

	plan, err := NewPlan("list.txt", base, out, stage, fixedClock())
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}
	if plan.Out != out {
		t.Errorf("Out = %q, want the --out override %q", plan.Out, out)
	}
	if plan.Stage != stage {
		t.Errorf("Stage = %q, want the --stage override %q", plan.Stage, stage)
	}
}

func TestNewPlanRequiresAClock(t *testing.T) {
	if _, err := NewPlan("list.txt", ".", "", "", nil); err == nil {
		t.Fatal("NewPlan accepted a nil clock, want an error")
	}
}

func TestListBaseName(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"azuredevops-contoso.txt", "azuredevops-contoso"},
		{filepath.Join("assets", "powershell", "azuredevops-heilingbrunner.txt"), "azuredevops-heilingbrunner"},
		{"my.repo.list.txt", "my.repo.list"},
		{"noextension", "noextension"},
	}
	for _, tc := range tests {
		if got := ListBaseName(tc.path); got != tc.want {
			t.Errorf("ListBaseName(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestArchivePathNestsByProject(t *testing.T) {
	plan := newTestPlan(t, "list.txt", t.TempDir())

	got := plan.ArchivePath("Inosoft", "VisiWin7-Licenses")
	want := filepath.Join(plan.Out, "Inosoft", "VisiWin7-Licenses.git.zip")
	if got != want {
		t.Errorf("ArchivePath = %q, want %q", got, want)
	}

	// The extension has to say what the file is. The PowerShell script wrote
	// ".git.7z" for what 7-Zip -tzip had produced as a plain ZIP.
	if !strings.HasSuffix(got, ".git.zip") {
		t.Errorf("ArchivePath = %q, want it to end in .git.zip", got)
	}
	if gotPart, wantPart := plan.PartPath("Inosoft", "VisiWin7-Licenses"), got+".part"; gotPart != wantPart {
		t.Errorf("PartPath = %q, want %q", gotPart, wantPart)
	}
}

func TestArchivePathNoProjectLandsInOutDir(t *testing.T) {
	plan := newTestPlan(t, "list.txt", t.TempDir())

	// github.com / codeberg.org entries have no project - the archive goes
	// straight into the run's output directory, no extra folder.
	got := plan.ArchivePath("", "dirscan")
	want := filepath.Join(plan.Out, "dirscan.git.zip")
	if got != want {
		t.Errorf("ArchivePath(\"\", ...) = %q, want %q", got, want)
	}
	if plan.ProjectDir("") != plan.Out {
		t.Errorf("ProjectDir(\"\") = %q, want %q", plan.ProjectDir(""), plan.Out)
	}
	if got := plan.FailureLogPath("", "dirscan"); got != filepath.Join(plan.FailuresDir(), "dirscan.stderr.txt") {
		t.Errorf("FailureLogPath(\"\", ...) = %q", got)
	}
}

func TestArchivePathSeparatesRepeatedRepoNames(t *testing.T) {
	plan := newTestPlan(t, "list.txt", t.TempDir())

	// VisiWinPowerToys exists under both Inosoft and Tools; 25 basenames repeat
	// across projects in the committed lists. Nesting by project is what keeps
	// their archives apart.
	a := plan.ArchivePath("Inosoft", "VisiWinPowerToys")
	b := plan.ArchivePath("Tools", "VisiWinPowerToys")
	if a == b {
		t.Fatalf("both projects resolved to the same archive path: %q", a)
	}
}

func TestStageDirIsUniquePerEntry(t *testing.T) {
	plan := newTestPlan(t, "list.txt", t.TempDir())

	// The PowerShell script staged at temp/<repo>, which was safe only because
	// it ran strictly sequentially. The index makes the name unique by
	// construction, so --jobs above one cannot corrupt a clone.
	seen := make(map[string]int)
	entries := []struct {
		project, repo string
	}{
		{"Inosoft", "VisiWinPowerToys"},
		{"Tools", "VisiWinPowerToys"},
		{"Contoso", "Contoso.SNGDOC"},
		{"Contoso", "Contoso.SNGDOC"}, // the duplicate pair, before dedupe
		{"Project.CNBM", "Contoso.Sngr"},
	}
	for i, e := range entries {
		dir := plan.StageDir(i, e.project, e.repo)
		if prev, dup := seen[dir]; dup {
			t.Errorf("entries %d and %d share the staging directory %q", prev, i, dir)
		}
		seen[dir] = i

		if !strings.HasPrefix(dir, plan.Stage+string(filepath.Separator)) {
			t.Errorf("StageDir = %q, want it under %q", dir, plan.Stage)
		}
	}
}

func TestStageDirIsStableAndZeroPadded(t *testing.T) {
	plan := newTestPlan(t, "list.txt", t.TempDir())

	first := plan.StageDir(7, "Inosoft", "VisiWin7-Licenses")
	if second := plan.StageDir(7, "Inosoft", "VisiWin7-Licenses"); first != second {
		t.Errorf("StageDir is not stable: %q then %q", first, second)
	}
	// Zero padding keeps the directory listing in list order, which makes a
	// half-finished staging directory easy to relate back to the list.
	if got := filepath.Base(first); !strings.HasPrefix(got, "0007-") {
		t.Errorf("StageDir base = %q, want it to start with a zero-padded index", got)
	}
}

func TestStageDirSanitisesAwkwardNames(t *testing.T) {
	plan := newTestPlan(t, "list.txt", t.TempDir())

	// These never reach here from a parsed list, because ParseRepoURL rejects
	// them, but the staging name must be safe regardless of what it is handed.
	for _, name := range []string{"My Project*", "NUL", "trailing ", "with/slash", `back\slash`} {
		dir := plan.StageDir(1, name, name)
		base := strings.TrimPrefix(filepath.Base(dir), "0001-")
		for _, bad := range []string{"*", "/", `\`, " "} {
			if strings.Contains(base, bad) {
				t.Errorf("StageDir(%q) produced %q, which still contains %q", name, base, bad)
			}
		}
	}
}

func TestLogPathCarriesTheRunTimestamp(t *testing.T) {
	plan := newTestPlan(t, "list.txt", t.TempDir())

	// Archives are overwritten by design on a re-run; the log must not be, or
	// the earlier run's diagnosis is lost.
	want := filepath.Join(plan.Out, "clonezip-20260727-164211.log")
	if got := plan.LogPath(); got != want {
		t.Errorf("LogPath = %q, want %q", got, want)
	}
}

func TestFailureAndTrashPaths(t *testing.T) {
	plan := newTestPlan(t, "list.txt", t.TempDir())

	if got, want := plan.FailuresDir(), filepath.Join(plan.Out, "_failures"); got != want {
		t.Errorf("FailuresDir = %q, want %q", got, want)
	}
	if got, want := plan.TrashDir(), filepath.Join(plan.Stage, ".trash"); got != want {
		t.Errorf("TrashDir = %q, want %q", got, want)
	}

	log := plan.FailureLogPath("Project.CNBM", "Contoso.Sngr")
	if !strings.HasPrefix(log, plan.FailuresDir()+string(filepath.Separator)) {
		t.Errorf("FailureLogPath = %q, want it under %q", log, plan.FailuresDir())
	}
	if !strings.HasSuffix(log, ".stderr.txt") {
		t.Errorf("FailureLogPath = %q, want it to end in .stderr.txt", log)
	}
}

func TestRepoNameFromArchive(t *testing.T) {
	tests := []struct {
		name string
		path string
		want string
		ok   bool
	}{
		{"current suffix", "MyRepo.git.zip", "MyRepo", true},
		{"legacy suffix", "MyRepo.git.7z", "MyRepo", true},
		{"with directory", filepath.Join("out", "Proj", "MyRepo.git.zip"), "MyRepo", true},
		{"dotted repo name", "Contoso.MESlite.OPC-UA.git.zip", "Contoso.MESlite.OPC-UA", true},
		{"uppercase suffix", "MyRepo.GIT.ZIP", "MyRepo", true},
		{"plain zip", "MyRepo.zip", "", false},
		{"no suffix", "MyRepo", "", false},
		{"suffix only", ".git.zip", "", false},
		{"wrong suffix", "MyRepo.tar.gz", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := RepoNameFromArchive(tc.path)
			if ok != tc.ok {
				t.Fatalf("RepoNameFromArchive(%q) ok = %v, want %v", tc.path, ok, tc.ok)
			}
			if got != tc.want {
				t.Errorf("RepoNameFromArchive(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

func TestRunDirName(t *testing.T) {
	stamp := time.Date(2026, 7, 27, 16, 42, 11, 0, time.UTC)
	if got, want := RunDirName("azuredevops-contoso.txt", stamp), "20260727-164211-azuredevops-contoso"; got != want {
		t.Errorf("RunDirName = %q, want %q", got, want)
	}
}
