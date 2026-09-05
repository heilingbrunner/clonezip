//go:build !windows

package doctorx

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

// sevenZipCandidates are the usual install locations, checked after PATH.
func sevenZipCandidates() []string {
	return []string{
		"/usr/bin/7z",
		"/usr/bin/7zz",
		"/usr/local/bin/7zz",
	}
}

// platformChecks reports the Linux-relevant facts, and marks the Windows-only ones as
// skipped rather than hiding them, so the shape of the report is the same everywhere.
func platformChecks(_ context.Context, o Options) []Result {
	longPathsReg := Result{
		ID: IDLongPathsReg, Group: GroupPlatform, Name: "long paths (registry)",
		Detail: "Windows only; PATH_MAX here is 4096",
	}
	longPathsGit := Result{
		ID: IDLongPathsGit, Group: GroupPlatform, Name: "git core.longpaths",
		Detail: "Windows only; git ignores it here",
	}
	antivirus := Result{
		ID: IDAntivirus, Group: GroupPlatform, Name: "antivirus",
		Detail: "Windows only",
	}
	return []Result{
		longPathsReg.withStatus(StatusSkip),
		longPathsGit.withStatus(StatusSkip),
		antivirus.withStatus(StatusSkip),
		checkCaseFolding(o.Out),
	}
}

// caseFoldingFilesystems fold case, so two names differing only in case collide on
// them just as they would on Windows.
var caseFoldingFilesystems = map[string]bool{
	"cifs":    true,
	"smb3":    true,
	"exfat":   true,
	"vfat":    true,
	"msdos":   true,
	"ntfs":    true,
	"ntfs3":   true,
	"fuseblk": true, // usually ntfs-3g
}

// checkCaseFolding reports whether the output directory is on a filesystem that folds
// case.
//
// Either way clonezip enforces the case-collision check, and this explains why: on a
// case-sensitive filesystem the check exists to keep the resulting backup set
// restorable on Windows, not to protect the local write.
func checkCaseFolding(out string) Result {
	res := Result{ID: IDCaseFoldMount, Group: GroupPlatform, Name: "case-folding mount"}

	fsType := filesystemType(out)
	if fsType == "" {
		res.Value = "unknown"
		res.Detail = "clonezip rejects case-only collisions regardless, so archives stay " +
			"restorable on Windows"
		return res.withStatus(StatusInfo)
	}
	res.Value = fsType
	if caseFoldingFilesystems[fsType] {
		res.Detail = "this filesystem folds case, so names differing only in case would collide"
	} else {
		res.Detail = "case-sensitive; clonezip still rejects case-only collisions so archives " +
			"stay restorable on Windows"
	}
	return res.withStatus(StatusInfo)
}

// filesystemType finds the mount covering path by taking the longest matching mount
// point in /proc/mounts. An empty result simply means "could not tell".
func filesystemType(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return ""
	}

	best, bestLen := "", -1
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		point, fsType := fields[1], fields[2]
		if !strings.HasPrefix(abs, point) {
			continue
		}
		// Only whole path components count, or "/var" would match "/variant".
		if point != "/" && len(abs) > len(point) && abs[len(point)] != filepath.Separator {
			continue
		}
		if len(point) > bestLen {
			best, bestLen = fsType, len(point)
		}
	}
	return best
}
