package service

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("archive"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLatestArchivesFindsProjectAndFlatArchives(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "Demos", "alpha.git.zip"))
	writeFile(t, filepath.Join(root, "Proj", "old.git.7z"))
	writeFile(t, filepath.Join(root, "flat.git.zip"))

	got, err := LatestArchives(root)
	if err != nil {
		t.Fatal(err)
	}
	keys := make([]string, len(got))
	for i, a := range got {
		keys[i] = a.Key()
	}
	want := []string{"Demos/alpha", "Proj/old", "flat"}
	if len(keys) != len(want) {
		t.Fatalf("got %v, want %v", keys, want)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("got %v, want %v", keys, want)
		}
	}
	if got[0].Project != "Demos" || got[0].Repo != "alpha" {
		t.Errorf("got %q/%q, want Demos/alpha", got[0].Project, got[0].Repo)
	}
}

func TestLatestArchivesSkipsInternalDirsAndNonArchives(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".stage", "0001-alpha", "alpha.git.zip"))
	writeFile(t, filepath.Join(root, "_failures", "alpha.stderr.txt"))
	writeFile(t, filepath.Join(root, "clonezip-20260101-000000.log"))
	writeFile(t, filepath.Join(root, "alpha.git.zip.part"))

	got, err := LatestArchives(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("got %+v, want nothing restorable", got)
	}
}

func TestLatestArchivesMissingDirIsEmpty(t *testing.T) {
	got, err := LatestArchives(filepath.Join(t.TempDir(), "never-ran"))
	if err != nil {
		t.Fatalf("a group that has never run must not error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %+v, want none", got)
	}
}
