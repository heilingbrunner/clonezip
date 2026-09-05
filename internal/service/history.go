package service

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/heilingbrunner/clonezip/internal/common/fsx"
)

// historyLogGlob matches every run's per-run debug log inside a group's out
// directory - clonezip-<timestamp>.log, git clone/LFS progress written via slog.
// The [0-9] after "clonezip-" is what keeps this from also matching
// historyFileName, which lives in the same directory.
const historyLogGlob = "clonezip-[0-9]*.log"

// historyFileName holds one JSON line per run, oldest first, separate from
// the verbose per-run debug logs so history can be trimmed and read back
// without touching them.
const historyFileName = "clonezip-history.log"

func historyLogPath(out string) string {
	return filepath.Join(out, historyFileName)
}

// appendHistoryRecord appends rec to out's history file and trims it to the
// newest limit entries, so the file both records every run and never grows
// without bound.
func appendHistoryRecord(out string, rec RunRecord, limit int) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal run record: %w", err)
	}

	path := historyLogPath(out)
	lines, err := readLines(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	lines = append(lines, string(data))
	if len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}
	return writeLinesAtomic(path, lines)
}

// loadHistoryFromDisk rebuilds a group's run history from out's history
// file, newest first and capped at limit, so history survives a service
// restart. It is best-effort: a line that fails to parse (a corrupt write,
// or - for a file predating this feature - not present at all) is skipped
// rather than failing the whole load.
func loadHistoryFromDisk(out string, limit int) []RunRecord {
	lines, err := readLines(historyLogPath(out))
	if err != nil {
		return nil
	}

	records := make([]RunRecord, 0, len(lines))
	for _, line := range lines {
		if line == "" {
			continue
		}
		var rec RunRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil || rec.ID == "" {
			continue
		}
		records = append(records, rec)
	}

	// The file is oldest-first (append order); GroupStatus.History is
	// documented newest-first.
	for i, j := 0, len(records)-1; i < j; i, j = i+1, j-1 {
		records[i], records[j] = records[j], records[i]
	}
	if len(records) > limit {
		records = records[:limit]
	}
	return records
}

// pruneHistoryFiles deletes every per-run debug log under out beyond the
// newest limit, so a group's out directory never accumulates more than
// limit of them. The history file itself (historyFileName) is unaffected -
// appendHistoryRecord trims that on its own.
func pruneHistoryFiles(out string, limit int) {
	matches, err := filepath.Glob(filepath.Join(out, historyLogGlob))
	if err != nil {
		return
	}
	sort.Sort(sort.Reverse(sort.StringSlice(matches)))
	if len(matches) <= limit {
		return
	}
	for _, path := range matches[limit:] {
		// Best-effort, the same as every other cleanup step in a run: a log
		// file that cannot be deleted this time is litter, not a failure.
		_ = fsx.RemoveAllRetry(path, "")
	}
}

// readLines reads path as newline-separated lines. A missing file reads as
// no lines rather than an error, since a group's history file does not
// exist until its first run.
func readLines(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	text := strings.TrimRight(string(data), "\n")
	if text == "" {
		return nil, nil
	}
	return strings.Split(text, "\n"), nil
}

// writeLinesAtomic replaces path's content with lines, one per line: write
// to a temp file in the same directory, sync, then rename over path - the
// same crash-safe pattern Config.Save uses - so a crash mid-write can never
// leave a half-written history file behind.
func writeLinesAtomic(path string, lines []string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, ".clonezip-history-*.tmp")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // no-op once the rename below succeeds

	content := strings.Join(lines, "\n")
	if len(lines) > 0 {
		content += "\n"
	}
	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
