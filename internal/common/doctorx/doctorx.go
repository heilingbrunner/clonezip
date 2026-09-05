// Package doctorx reports whether the environment can actually do the work.
//
// Every check here is local and cheap except the authentication probe, which is the
// one question that cannot be answered without talking to a server - and the one
// worth the most, because it turns three hundred silent failures spread over two
// hours into a single failure in five seconds.
package doctorx

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/heilingbrunner/clonezip/internal/common/fsx"
	"github.com/heilingbrunner/clonezip/internal/common/gitx"
	"github.com/heilingbrunner/clonezip/internal/common/repolist"
)

// Status is the outcome of one check.
type Status int

const (
	// StatusOK means the check passed.
	StatusOK Status = iota
	// StatusWarn is something to know about that does not prevent a run.
	StatusWarn
	// StatusFail prevents clonezip from working correctly.
	StatusFail
	// StatusInfo is context, never a problem.
	StatusInfo
	// StatusSkip means the check does not apply, or was not asked for.
	StatusSkip
)

func (s Status) String() string {
	switch s {
	case StatusOK:
		return "OK"
	case StatusWarn:
		return "WARN"
	case StatusFail:
		return "FAIL"
	case StatusInfo:
		return "INFO"
	case StatusSkip:
		return "SKIP"
	}
	return "?"
}

// Groups a check belongs to, used to order the report.
const (
	GroupTools    = "TOOLS"
	GroupAuth     = "AUTH"
	GroupStorage  = "STORAGE"
	GroupPlatform = "PLATFORM"
)

// Check IDs, stable so they can be named in documentation and asserted in tests.
const (
	IDGitPresent    = "git.present"
	IDLFSPresent    = "lfs.present"
	IDLFSFilters    = "lfs.filters"
	IDZipBuiltin    = "zip.builtin"
	IDSevenZip      = "sevenzip"
	IDAuthHelper    = "auth.helper"
	IDAuthProbe     = "auth.probe"
	IDDiskWritable  = "disk.writable"
	IDDiskFree      = "disk.free"
	IDLongPathsReg  = "win.longpaths.registry"
	IDLongPathsGit  = "win.longpaths.git"
	IDAutoCRLF      = "git.autocrlf"
	IDAntivirus     = "win.av"
	IDCaseFoldMount = "linux.casefold"
)

// Free-space thresholds. A backup of three hundred real repositories needs room, and
// finding that out at repository 200 is expensive.
const (
	freeSpaceFail = 5 << 30  // 5 GB
	freeSpaceWarn = 20 << 30 // 20 GB
)

// probeTimeout bounds the authentication probe. It exists to fail fast; a server that
// needs longer than this to list refs is itself the problem.
const probeTimeout = 20 * time.Second

// Result is what one check found.
type Result struct {
	ID     string `json:"id"`
	Group  string `json:"group"`
	Name   string `json:"name"`
	Status string `json:"status"`

	// Value is the answer: a version, a path, a size.
	Value string `json:"value,omitempty"`

	// Detail expands on the value when there is more to say.
	Detail string `json:"detail,omitempty"`

	// Hints are what to do about it, shown only when the check did not pass.
	Hints []string `json:"hints,omitempty"`

	status Status
}

// Level returns the typed status.
func (r Result) Level() Status { return r.status }

// withStatus records the status in both its typed and rendered form, so JSON output
// stays readable instead of emitting bare integers.
func (r Result) withStatus(s Status) Result {
	r.status = s
	r.Status = s.String()
	return r
}

// Options configures a check run.
type Options struct {
	Git *gitx.Git

	// Out is the directory to test for writability and free space.
	Out string

	// Probe is a repo list or a single URL to test authentication against. Empty
	// skips that check. Ignored when ProbeList is set.
	Probe string

	// ProbeList is an already-parsed repo list to test authentication
	// against, for a caller (the service scheduler) that has one in hand
	// from an inline config and would otherwise have to write it to a file
	// just to satisfy Probe. Takes priority over Probe when non-nil.
	ProbeList *repolist.List

	// Strict promotes every warning to a failure.
	Strict bool
}

// Report is the outcome of a whole run.
type Report struct {
	CloneZip string   `json:"clonezip"`
	OS       string   `json:"os"`
	Results  []Result `json:"checks"`
}

// Count returns how many results carry the given status.
func (r Report) Count(status Status) int {
	n := 0
	for _, res := range r.Results {
		if res.status == status {
			n++
		}
	}
	return n
}

// Failed reports whether anything must be fixed.
func (r Report) Failed() bool { return r.Count(StatusFail) > 0 }

// ExitCode is 0 when nothing failed. Warnings are deliberately not failures: an unset
// core.longpaths is worth mentioning, not worth refusing to run over.
func (r Report) ExitCode() int {
	if r.Failed() {
		return 1
	}
	return 0
}

// Run performs every check.
func Run(ctx context.Context, version string, o Options) Report {
	if o.Git == nil {
		o.Git = gitx.New(gitx.NewExecRunner())
	}
	if o.Out == "" {
		o.Out = "."
	}

	report := Report{
		CloneZip: version,
		OS:       runtime.GOOS + "/" + runtime.GOARCH,
	}

	report.Results = append(report.Results,
		checkGit(ctx, o.Git),
		checkLFS(ctx, o.Git),
		checkLFSFilters(ctx, o.Git),
		checkZipBuiltin(),
		checkSevenZip(),
		checkAuthHelper(ctx, o.Git),
	)
	report.Results = append(report.Results, checkAuthProbe(ctx, o.Git, o.Probe, o.ProbeList)...)
	report.Results = append(report.Results,
		checkWritable(o.Out),
		checkFreeSpace(o.Out),
		checkAutoCRLF(ctx, o.Git),
	)
	report.Results = append(report.Results, platformChecks(ctx, o)...)

	if o.Strict {
		for i, res := range report.Results {
			if res.status == StatusWarn {
				report.Results[i] = res.withStatus(StatusFail)
			}
		}
	}
	return report
}

func checkGit(ctx context.Context, git *gitx.Git) Result {
	res := Result{ID: IDGitPresent, Group: GroupTools, Name: "git"}

	path, err := exec.LookPath("git")
	if err != nil {
		res.Detail = "not found on PATH"
		res.Hints = []string{"install git from https://git-scm.com and reopen your shell"}
		return res.withStatus(StatusFail)
	}

	version, err := git.Version(ctx)
	if err != nil {
		res.Value = path
		res.Detail = err.Error()
		return res.withStatus(StatusFail)
	}
	res.Value = version
	res.Detail = path
	return res.withStatus(StatusOK)
}

func checkLFS(ctx context.Context, git *gitx.Git) Result {
	res := Result{ID: IDLFSPresent, Group: GroupTools, Name: "git-lfs"}

	// Asked through git rather than by looking for a git-lfs binary, because that is
	// what validates the wiring: a binary git cannot invoke is no use.
	version, err := git.LFSVersion(ctx)
	if err != nil {
		res.Detail = "git cannot run git-lfs"
		res.Hints = []string{
			"install git-lfs from https://git-lfs.com",
			"then run: git lfs install",
		}
		return res.withStatus(StatusFail)
	}
	res.Value = version
	return res.withStatus(StatusOK)
}

func checkLFSFilters(ctx context.Context, git *gitx.Git) Result {
	res := Result{ID: IDLFSFilters, Group: GroupTools, Name: "lfs filters"}

	value, err := git.ConfigGet(ctx, "filter.lfs.process")
	if err != nil || value == "" {
		// Without these, restore's git lfs checkout cannot turn pointer files into
		// content, which is the whole point of capturing the LFS payload.
		res.Detail = "filter.lfs.process is not configured"
		res.Hints = []string{"run: git lfs install"}
		return res.withStatus(StatusFail)
	}
	res.Value = value
	return res.withStatus(StatusOK)
}

func checkZipBuiltin() Result {
	res := Result{
		ID: IDZipBuiltin, Group: GroupTools, Name: "zip support",
		Value:  "built in",
		Detail: "archives are written with the standard library",
	}
	return res.withStatus(StatusOK)
}

// checkSevenZip reports 7-Zip for information only.
//
// The PowerShell scripts required it at a hardcoded path and refused to run without
// it. They also called it with -tzip, which means every archive they ever produced
// was a plain ZIP container - so clonezip reads all of them without it.
func checkSevenZip() Result {
	res := Result{
		ID: IDSevenZip, Group: GroupTools, Name: "7-Zip",
		Detail: "not required; clonezip reads and writes zip itself",
	}
	if path := findSevenZip(); path != "" {
		res.Value = path
	} else {
		res.Value = "not installed"
	}
	return res.withStatus(StatusInfo)
}

func findSevenZip() string {
	for _, name := range []string{"7z", "7zz", "7za"} {
		if path, err := exec.LookPath(name); err == nil {
			return path
		}
	}
	for _, path := range sevenZipCandidates() {
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path
		}
	}
	return ""
}

func checkAuthHelper(ctx context.Context, git *gitx.Git) Result {
	res := Result{ID: IDAuthHelper, Group: GroupAuth, Name: "credential.helper"}

	helpers, err := git.ConfigGetAll(ctx, "credential.helper")
	if err != nil || len(helpers) == 0 {
		res.Detail = "none configured"
		res.Hints = []string{
			"clonezip sets GIT_TERMINAL_PROMPT=0, so clones fail fast rather than hanging",
			"on Windows: git config --global credential.helper manager",
			"GitHub/Codeberg: run 'gh auth login', or store a personal access token via your credential helper",
			"Bitbucket: store an app password or API token via your credential helper (username + token as the password)",
		}
		return res.withStatus(StatusWarn)
	}
	res.Value = strings.Join(helpers, ", ")
	return res.withStatus(StatusOK)
}

// checkAuthProbe asks each distinct host whether the stored credentials
// work. probeList, when non-nil, is used directly instead of parsing probe
// as a file path - the service scheduler already has a parsed list in hand
// from an inline config and would otherwise have to write it to a file just
// to satisfy this check.
func checkAuthProbe(ctx context.Context, git *gitx.Git, probe string, probeList *repolist.List) []Result {
	if probe == "" && probeList == nil {
		res := Result{
			ID: IDAuthProbe, Group: GroupAuth, Name: "auth probe",
			Detail: "not run",
			Hints:  []string{"add --probe <repolist.yaml|url> to test authentication"},
		}
		return []Result{res.withStatus(StatusSkip)}
	}

	var targets map[string]string
	var err error
	if probeList != nil {
		targets = targetsFromList(*probeList)
	} else {
		targets, err = probeTargets(probe)
	}
	if err != nil {
		res := Result{ID: IDAuthProbe, Group: GroupAuth, Name: "auth probe", Detail: err.Error()}
		return []Result{res.withStatus(StatusFail)}
	}
	if len(targets) == 0 {
		res := Result{
			ID: IDAuthProbe, Group: GroupAuth, Name: "auth probe",
			Detail: "no remote URL found in " + probe,
		}
		return []Result{res.withStatus(StatusWarn)}
	}

	var out []Result
	for host, target := range targets {
		res := Result{ID: IDAuthProbe, Group: GroupAuth, Name: "auth " + host}

		probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
		err := git.LSRemote(probeCtx, target)
		cancel()

		if err != nil {
			res.Detail = sanitizeTarget(target) + ": " + shortReason(err)
			res.Hints = []string{
				"only one repo per host is probed, so this may be a credential, permission, or" +
					" renamed/deleted-repo problem specific to the repo named above, not to every" +
					" repo on this host",
				"refresh the credential for this host, then retry. For example:",
				"  Azure DevOps:      git credential-manager azure-repos",
				"  GitHub / Codeberg: gh auth login, or re-enter a personal access token",
				"  Bitbucket:         re-enter an app password or API token as the password",
			}
			out = append(out, res.withStatus(StatusFail))
			continue
		}
		res.Value = sanitizeTarget(target)
		out = append(out, res.withStatus(StatusOK))
	}
	return out
}

// sanitizeTarget renders a probe target for display with any embedded
// credentials stripped, so a token pasted into a repo list URL never ends up
// in a report or log line.
func sanitizeTarget(target string) string {
	u, err := url.Parse(target)
	if err != nil {
		return target
	}
	return repolist.SanitizedURL(u)
}

// probeTargets picks one URL per distinct host, so a list of three hundred entries
// costs a handful of requests. The two committed lists use different hosts with
// different credential shapes, which is exactly why per-host coverage matters.
func probeTargets(probe string) (map[string]string, error) {
	targets := map[string]string{}

	if strings.Contains(probe, "://") {
		u, err := url.Parse(probe)
		if err != nil {
			return nil, fmt.Errorf("parse --probe URL: %w", err)
		}
		if host := u.Hostname(); host != "" {
			targets[host] = probe
		}
		return targets, nil
	}

	list, err := repolist.ParseFile(probe)
	if err != nil {
		return nil, err
	}
	return targetsFromList(list), nil
}

// targetsFromList picks one URL per distinct host out of an already-parsed
// list, the same one-per-host selection probeTargets does for a file path.
func targetsFromList(list repolist.List) map[string]string {
	targets := map[string]string{}
	for _, entry := range list.Entries {
		host := entry.URL.Hostname()
		if host == "" {
			continue // a local file:// entry has nothing to authenticate against
		}
		if _, seen := targets[host]; !seen {
			targets[host] = entry.CloneURL()
		}
	}
	return targets
}

func checkWritable(out string) Result {
	res := Result{ID: IDDiskWritable, Group: GroupStorage, Name: "output dir", Value: out}

	if err := fsx.Writable(out); err != nil {
		res.Detail = err.Error()
		return res.withStatus(StatusFail)
	}
	res.Detail = "writable"
	return res.withStatus(StatusOK)
}

func checkFreeSpace(out string) Result {
	res := Result{ID: IDDiskFree, Group: GroupStorage, Name: "free space"}

	free, err := fsx.FreeSpace(out)
	if err != nil {
		res.Detail = "could not be determined: " + err.Error()
		return res.withStatus(StatusInfo)
	}
	res.Value = humanBytes(free)

	switch {
	case free < freeSpaceFail:
		res.Hints = []string{"free up space, or choose another --out directory"}
		return res.withStatus(StatusFail)
	case free < freeSpaceWarn:
		res.Hints = []string{"a full backup of several hundred repositories may need more"}
		return res.withStatus(StatusWarn)
	}
	return res.withStatus(StatusOK)
}

func checkAutoCRLF(ctx context.Context, git *gitx.Git) Result {
	res := Result{ID: IDAutoCRLF, Group: GroupPlatform, Name: "core.autocrlf"}

	value, err := git.ConfigGet(ctx, "core.autocrlf")
	if err != nil || value == "" {
		res.Value = "unset"
		return res.withStatus(StatusInfo)
	}
	res.Value = value
	res.Detail = "affects the line endings of a restored working tree"
	return res.withStatus(StatusInfo)
}

// shortReason renders an error as one line, preferring what the remote said.
func shortReason(err error) string {
	var exitErr *gitx.ExitError
	if errors.As(err, &exitErr) {
		if reason := exitErr.Reason(); reason != "" {
			return fmt.Sprintf("exit %d: %s", exitErr.Code, reason)
		}
		return fmt.Sprintf("exit %d", exitErr.Code)
	}
	return err.Error()
}

func humanBytes(n uint64) string {
	// Decimal (SI) units, matching file managers and disk-properties dialogs.
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	value := float64(n)
	for _, suffix := range []string{"KB", "MB", "GB", "TB", "PB"} {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
	}
	return fmt.Sprintf("%.0f EB", value)
}
