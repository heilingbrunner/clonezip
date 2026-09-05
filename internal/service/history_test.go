package service

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAppendHistoryRecordRoundTrip(t *testing.T) {
	dir := t.TempDir()

	want := RunRecord{
		ID: "20260826-120000.000001", Trigger: TriggerManual,
		Started: time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC),
		Ended:   time.Date(2026, 8, 26, 12, 1, 0, 0, time.UTC),
		OK:      2, Warn: 1, Failed: 0, Skipped: 0,
		TotalBytes: 1024, RepoCount: 3, Out: dir,
	}
	if err := appendHistoryRecord(dir, want, 50); err != nil {
		t.Fatalf("appendHistoryRecord: %v", err)
	}

	got := loadHistoryFromDisk(dir, 50)
	if len(got) != 1 {
		t.Fatalf("loadHistoryFromDisk returned %d records, want 1", len(got))
	}
	if got[0].ID != want.ID || got[0].OK != want.OK || got[0].RepoCount != want.RepoCount || !got[0].Started.Equal(want.Started) {
		t.Errorf("loadHistoryFromDisk[0] = %+v, want %+v", got[0], want)
	}
}

func TestAppendHistoryRecordTrimsToLimit(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	var ids []string
	for i := range 5 {
		ts := base.Add(time.Duration(i) * time.Minute)
		rec := RunRecord{ID: ts.Format("20060102-150405"), Started: ts}
		if err := appendHistoryRecord(dir, rec, 2); err != nil {
			t.Fatalf("appendHistoryRecord: %v", err)
		}
		ids = append(ids, rec.ID)
	}

	got := loadHistoryFromDisk(dir, 50)
	if len(got) != 2 {
		t.Fatalf("loadHistoryFromDisk returned %d records, want 2 (trimmed)", len(got))
	}
	// Newest first: the last two writes (i=4, i=3), reversed.
	if got[0].ID != ids[4] || got[1].ID != ids[3] {
		t.Errorf("loadHistoryFromDisk order = [%s, %s], want [%s, %s]", got[0].ID, got[1].ID, ids[4], ids[3])
	}
}

func TestLoadHistoryFromDiskCapsIndependentlyOfFileSize(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	var ids []string
	for i := range 5 {
		ts := base.Add(time.Duration(i) * time.Minute)
		rec := RunRecord{ID: ts.Format("20060102-150405"), Started: ts}
		// A generous file-side limit, so every entry is kept on disk...
		if err := appendHistoryRecord(dir, rec, 50); err != nil {
			t.Fatalf("appendHistoryRecord: %v", err)
		}
		ids = append(ids, rec.ID)
	}

	// ...but the reader can still be asked for fewer than that.
	got := loadHistoryFromDisk(dir, 2)
	if len(got) != 2 {
		t.Fatalf("loadHistoryFromDisk returned %d records, want 2", len(got))
	}
	if got[0].ID != ids[4] || got[1].ID != ids[3] {
		t.Errorf("loadHistoryFromDisk order = [%s, %s], want [%s, %s]", got[0].ID, got[1].ID, ids[4], ids[3])
	}
}

func TestLoadHistoryFromDiskSkipsCorruptAndMissingFile(t *testing.T) {
	dir := t.TempDir()

	if got := loadHistoryFromDisk(dir, 50); len(got) != 0 {
		t.Errorf("loadHistoryFromDisk on a missing file returned %d records, want 0", len(got))
	}

	path := historyLogPath(dir)
	content := "not json\n" +
		`{"ok":1}` + "\n" + // parses, but has no ID - a pre-feature-shaped line
		`{"ID":"20260826-120500.000001","OK":1}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}

	got := loadHistoryFromDisk(dir, 50)
	if len(got) != 1 {
		t.Fatalf("loadHistoryFromDisk returned %d records, want 1 (only the valid line)", len(got))
	}
	if got[0].ID != "20260826-120500.000001" {
		t.Errorf("loadHistoryFromDisk[0].ID = %q, want %q", got[0].ID, "20260826-120500.000001")
	}
}

func TestPruneHistoryFilesDeletesOnlyOldestDebugLogs(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	var paths []string
	for i := range 4 {
		ts := base.Add(time.Duration(i) * time.Minute)
		path := filepath.Join(dir, "clonezip-"+ts.Format("20060102-150405")+".log")
		if err := os.WriteFile(path, []byte(`{"msg":"run started"}`+"\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		paths = append(paths, path)
	}
	// A history file living alongside the debug logs must never be treated
	// as one of them, whatever historyLimit is.
	if err := appendHistoryRecord(dir, RunRecord{ID: "keep-me"}, 50); err != nil {
		t.Fatalf("appendHistoryRecord: %v", err)
	}

	pruneHistoryFiles(dir, 2)

	if _, err := os.Stat(historyLogPath(dir)); err != nil {
		t.Errorf("pruneHistoryFiles removed the history file: %v", err)
	}
	for _, stale := range paths[:2] {
		if _, err := os.Stat(stale); !os.IsNotExist(err) {
			t.Errorf("pruneHistoryFiles did not delete %s", stale)
		}
	}
	for _, kept := range paths[2:] {
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("pruneHistoryFiles deleted %s, want it kept", kept)
		}
	}
}
