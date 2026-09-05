package doctorx

import (
	"context"

	"golang.org/x/sys/windows/registry"

	"github.com/heilingbrunner/clonezip/internal/common/gitx"
)

// sevenZipCandidates are the usual install locations, checked after PATH.
//
// Hardcoding a single path was the scripts' mistake: this machine has 7-Zip in three
// different places, and the script only looked at one of them.
func sevenZipCandidates() []string {
	return []string{
		`C:\Program Files\7-Zip\7z.exe`,
		`C:\Program Files (x86)\7-Zip\7z.exe`,
	}
}

func platformChecks(ctx context.Context, o Options) []Result {
	caseFold := Result{
		ID: IDCaseFoldMount, Group: GroupPlatform, Name: "case-folding mount",
		Detail: "Linux only",
	}
	return []Result{
		checkLongPathsRegistry(),
		checkLongPathsGit(ctx, o.Git),
		checkAntivirus(),
		caseFold.withStatus(StatusSkip),
	}
}

// checkLongPathsRegistry reports whether Windows itself allows paths beyond 260
// characters.
//
// This bites on restore rather than on backup: a bare repository is path-shallow, but
// a restored working tree materialises real source trees, which is where the limit is
// actually reached.
func checkLongPathsRegistry() Result {
	res := Result{ID: IDLongPathsReg, Group: GroupPlatform, Name: "long paths (registry)"}

	key, err := registry.OpenKey(registry.LOCAL_MACHINE,
		`SYSTEM\CurrentControlSet\Control\FileSystem`, registry.QUERY_VALUE)
	if err != nil {
		res.Detail = "could not read the registry: " + err.Error()
		return res.withStatus(StatusWarn)
	}
	// Read-only handle; a failed close cannot affect the value already read.
	defer func() { _ = key.Close() }()

	value, _, err := key.GetIntegerValue("LongPathsEnabled")
	if err != nil || value != 1 {
		res.Value = "not enabled"
		res.Hints = []string{
			"run as administrator:",
			`  reg add "HKLM\SYSTEM\CurrentControlSet\Control\FileSystem" ` +
				`/v LongPathsEnabled /t REG_DWORD /d 1 /f`,
		}
		return res.withStatus(StatusFail)
	}
	res.Value = "enabled"
	return res.withStatus(StatusOK)
}

// checkLongPathsGit reports the git setting, which clonezip does not depend on: it passes
// -c core.longpaths=true on every invocation precisely because this is usually unset.
func checkLongPathsGit(ctx context.Context, git *gitx.Git) Result {
	res := Result{ID: IDLongPathsGit, Group: GroupPlatform, Name: "git core.longpaths"}

	value, err := git.ConfigGet(ctx, "core.longpaths")
	if err != nil || value == "" {
		res.Value = "unset"
		res.Detail = "clonezip passes -c core.longpaths=true itself, so this is informational"
		res.Hints = []string{
			"to set it for your own git use: git config --global core.longpaths true",
		}
		return res.withStatus(StatusWarn)
	}
	res.Value = value
	return res.withStatus(StatusOK)
}

// checkAntivirus never probes anything. Detecting a scanner reliably is not feasible,
// and the advice is the same either way.
func checkAntivirus() Result {
	res := Result{
		ID: IDAntivirus, Group: GroupPlatform, Name: "antivirus",
		Detail: "exclude the staging directory to avoid slow clones and locked files during cleanup",
	}
	return res.withStatus(StatusInfo)
}
