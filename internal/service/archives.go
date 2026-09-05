package service

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/heilingbrunner/clonezip/internal/common/layout"
)

// ArchiveInfo is one repository archive found under a group's out directory.
type ArchiveInfo struct {
	// Project is the directory the archive sits in, relative to the group's
	// out directory: an Azure DevOps project or a Bitbucket workspace. Empty
	// for hosts without a project layer (github.com, codeberg.org).
	Project string

	Repo string
	Path string

	Size     int64
	Modified time.Time
}

// Key identifies the repository an archive holds.
func (a ArchiveInfo) Key() string {
	if a.Project == "" {
		return a.Repo
	}
	return a.Project + "/" + a.Repo
}

// LatestArchives lists every repository a group holds a backup of, sorted by
// project then repository.
//
// A group backs up into its own out directory rather than a fresh timestamped
// one per run, so whatever sits there is by construction the newest: each run
// overwrites the previous one's archives. A missing out directory is not an
// error - a group that has never run simply has nothing to restore.
func LatestArchives(groupOut string) ([]ArchiveInfo, error) {
	entries, err := os.ReadDir(groupOut)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read group directory: %w", err)
	}

	var out []ArchiveInfo
	for _, e := range entries {
		if !e.IsDir() {
			if a, ok := newArchiveInfo(groupOut, "", e); ok {
				out = append(out, a)
			}
			continue
		}
		if isInternalDir(e.Name()) {
			continue
		}
		project := e.Name()
		projectDir := filepath.Join(groupOut, project)
		nested, err := os.ReadDir(projectDir)
		if err != nil {
			continue
		}
		for _, n := range nested {
			if n.IsDir() {
				continue
			}
			if a, ok := newArchiveInfo(projectDir, project, n); ok {
				out = append(out, a)
			}
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Key() < out[j].Key() })
	return out, nil
}

func newArchiveInfo(dir, project string, e os.DirEntry) (ArchiveInfo, bool) {
	repo, ok := layout.RepoNameFromArchive(e.Name())
	if !ok {
		return ArchiveInfo{}, false
	}
	info, err := e.Info()
	if err != nil {
		return ArchiveInfo{}, false
	}
	return ArchiveInfo{
		Project:  project,
		Repo:     repo,
		Path:     filepath.Join(dir, e.Name()),
		Size:     info.Size(),
		Modified: info.ModTime(),
	}, true
}

// isInternalDir reports whether a directory is run bookkeeping rather than
// archive output.
func isInternalDir(name string) bool {
	return name == layout.StageDirName ||
		name == layout.FailuresDirName ||
		strings.HasPrefix(name, ".")
}
