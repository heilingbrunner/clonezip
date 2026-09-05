package fsx

import (
	"os"
	"path/filepath"
	"testing"
)

// writeBareRepoTree builds a directory shaped like the bare clone clonezip stages,
// including the read-only pack files git writes. Those are what make a plain
// os.RemoveAll fail with "Access is denied" on Windows.
func writeBareRepoTree(t *testing.T, root string) {
	t.Helper()

	files := map[string]struct {
		content string
		mode    os.FileMode
	}{
		"HEAD":                                {"ref: refs/heads/main\n", 0o644},
		"config":                              {"[core]\n\tbare = true\n", 0o644},
		"objects/pack/pack-0123456789ab.pack": {"PACK fake contents", 0o444},
		"objects/pack/pack-0123456789ab.idx":  {"fake index", 0o444},
		"refs/heads/main":                     {"9f3c1ab\n", 0o644},
	}
	for rel, spec := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(spec.content), spec.mode); err != nil {
			t.Fatal(err)
		}
	}
	// An empty directory, as a bare repo has for branches/ and refs/tags/.
	if err := os.MkdirAll(filepath.Join(root, "refs", "tags"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveAllRetryDeletesReadOnlyPackFiles(t *testing.T) {
	base := t.TempDir()
	stage := filepath.Join(base, "0001-Proj-Repo")
	writeBareRepoTree(t, stage)

	if err := RemoveAllRetry(stage, filepath.Join(base, ".trash")); err != nil {
		t.Fatalf("RemoveAllRetry on a tree with read-only pack files: %v", err)
	}
	if _, err := os.Stat(stage); !os.IsNotExist(err) {
		t.Errorf("staging directory still present after RemoveAllRetry (stat err: %v)", err)
	}
}

func TestRemoveAllRetryOnMissingPathSucceeds(t *testing.T) {
	// The pipeline deletes the staging directory before cloning into it, when it
	// usually does not exist yet. That must not be an error.
	missing := filepath.Join(t.TempDir(), "never-created")
	if err := RemoveAllRetry(missing, ""); err != nil {
		t.Errorf("RemoveAllRetry on a missing path = %v, want nil", err)
	}
}

func TestRemoveAllRetryWithoutTrashDir(t *testing.T) {
	stage := filepath.Join(t.TempDir(), "stage")
	writeBareRepoTree(t, stage)

	if err := RemoveAllRetry(stage, ""); err != nil {
		t.Fatalf("RemoveAllRetry with no trash directory: %v", err)
	}
}

func TestRemoveAllRetrySurvivesALockedFile(t *testing.T) {
	base := t.TempDir()
	stage := filepath.Join(base, "0001-Proj-Repo")
	trash := filepath.Join(base, ".trash")
	writeBareRepoTree(t, stage)

	// An open handle is exactly what Defender, the search indexer or a
	// not-yet-exited git-lfs holds. On Windows it makes the delete fail outright;
	// on Linux the unlink succeeds anyway. Either way the run must continue, and
	// the staging path must be gone from where the next clone would land.
	// An open handle is exactly what Defender, the search indexer or a
	// not-yet-exited git-lfs holds.
	//
	// Note the platform difference this test pins down: on Linux the unlink
	// succeeds regardless, so the delete just works. On Windows it fails, and the
	// rename-aside fallback cannot rescue it either - Windows locks the whole
	// subtree for a directory rename, so a locked *descendant* blocks moving the
	// parent too. Reporting the failure and letting the run continue is therefore
	// the real contract, not a guarantee that the path is freed.
	locked := filepath.Join(stage, "objects", "pack", "pack-0123456789ab.pack")
	f, err := os.Open(locked)
	if err != nil {
		t.Fatal(err)
	}

	removeErr := RemoveAllRetry(stage, trash)
	if err := f.Close(); err != nil {
		t.Fatalf("closing the locked file: %v", err)
	}

	if removeErr != nil {
		// It gave up, which is allowed. What matters is that it says so, rather
		// than reporting success and leaving the caller to clone into a directory
		// that is still there.
		if _, statErr := os.Stat(stage); os.IsNotExist(statErr) {
			t.Error("RemoveAllRetry reported an error but the path is gone")
		}
		t.Logf("delete reported (expected on Windows): %v", removeErr)
	}

	// Once the handle is gone the path must be reclaimable, so a retry later in
	// the run is not permanently poisoned.
	if err := RemoveAllRetry(stage, trash); err != nil {
		t.Fatalf("RemoveAllRetry after the handle was released: %v", err)
	}
	if _, err := os.Stat(stage); !os.IsNotExist(err) {
		t.Errorf("staging path still occupied (stat err: %v)", err)
	}
	if stuck := EmptyTrash(trash); stuck != 0 {
		t.Errorf("EmptyTrash left %d directories stuck once nothing held a handle", stuck)
	}
}

func TestEmptyTrashClearsWhatWasLeftBehind(t *testing.T) {
	base := t.TempDir()
	trash := filepath.Join(base, ".trash")

	// Simulate two directories an earlier delete had to rename aside, both
	// carrying read-only files.
	for _, name := range []string{"0001-Proj-Repo-abc", "0002-Proj-Other-def"} {
		writeBareRepoTree(t, filepath.Join(trash, name))
	}

	if stuck := EmptyTrash(trash); stuck != 0 {
		t.Errorf("EmptyTrash left %d directories stuck, want 0", stuck)
	}
	if _, err := os.Stat(trash); !os.IsNotExist(err) {
		t.Errorf("trash directory still present (stat err: %v)", err)
	}
}

func TestEmptyTrashOnMissingDirIsHarmless(t *testing.T) {
	if stuck := EmptyTrash(filepath.Join(t.TempDir(), "no-trash-here")); stuck != 0 {
		t.Errorf("EmptyTrash on a missing directory reported %d stuck", stuck)
	}
}

func TestDirSize(t *testing.T) {
	root := t.TempDir()
	writeBareRepoTree(t, root)

	got, err := DirSize(root)
	if err != nil {
		t.Fatalf("DirSize: %v", err)
	}

	// Sum independently by stating each known file, so the assertion does not
	// just restate the implementation's own walk.
	var want int64
	for _, rel := range []string{
		"HEAD", "config",
		filepath.Join("objects", "pack", "pack-0123456789ab.pack"),
		filepath.Join("objects", "pack", "pack-0123456789ab.idx"),
		filepath.Join("refs", "heads", "main"),
	} {
		info, err := os.Stat(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		want += info.Size()
	}
	if got != want {
		t.Errorf("DirSize = %d, want %d", got, want)
	}
}

func TestWritableCreatesTheDirectoryAndLeavesNoProbe(t *testing.T) {
	// The run directory does not exist when this is checked, so creating it is
	// part of the job.
	dir := filepath.Join(t.TempDir(), "20260727-164211-azuredevops-contoso")

	if err := Writable(dir); err != nil {
		t.Fatalf("Writable: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("Writable left %d file(s) behind: %v", len(entries), entries)
	}
}

func TestFreeSpaceReportsSomething(t *testing.T) {
	got, err := FreeSpace(t.TempDir())
	if err != nil {
		t.Fatalf("FreeSpace: %v", err)
	}
	if got == 0 {
		t.Error("FreeSpace = 0 bytes on the temp volume, which cannot be right")
	}
}
