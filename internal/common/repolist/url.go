package repolist

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/heilingbrunner/clonezip/internal/common/safepath"
)

// gitSeparator is the literal segment Azure DevOps puts between the project and
// the repository in every clone URL, in both the dev.azure.com and the older
// visualstudio.com form.
const gitSeparator = "/_git/"

// Provider names the kind of host a clone URL points at. It is informational -
// it goes into the manifest and drives the service's optional project override -
// and never changes how a repository is cloned or archived.
const (
	ProviderAzure     = "azure"     // Azure DevOps, or a self-hosted Azure DevOps Server (any host with a /_git/ segment)
	ProviderGitHub    = "github"    // github.com
	ProviderCodeberg  = "codeberg"  // codeberg.org
	ProviderBitbucket = "bitbucket" // bitbucket.org (Bitbucket Cloud)
	ProviderLocal     = "local"     // a file:// repository
)

// ownerRepoHost describes one of the non-Azure hosts whose clone URLs have the
// shape https://<host>/<owner>/<repo>[.git] with no "/_git/" segment.
type ownerRepoHost struct {
	provider string
	// nestUnderOwner nests the archive under an <owner>/ directory instead of
	// writing it straight into the run's output directory. Bitbucket Cloud sets
	// this so each workspace gets its own folder; github.com and codeberg.org do
	// not, matching how they have always behaved.
	nestUnderOwner bool
}

// ownerRepoHosts are the non-Azure hosts whose clone URLs have the shape
// https://<host>/<owner>/<repo>[.git] with no project layer. Only these hosts are
// accepted without a "/_git/" segment.
var ownerRepoHosts = map[string]ownerRepoHost{
	"github.com":    {provider: ProviderGitHub},
	"codeberg.org":  {provider: ProviderCodeberg},
	"bitbucket.org": {provider: ProviderBitbucket, nestUnderOwner: true},
}

// Errors describing an unusable repository URL.
var (
	ErrNotHTTP      = errors.New("not an http or https URL")
	ErrNoHost       = errors.New("no host")
	ErrNoGitSegment = errors.New(`not a recognized clone URL (no "/_git/" segment, and not a github.com, codeberg.org or bitbucket.org URL)`)
	ErrNoRepo       = errors.New("no repository name in the URL")
	ErrNoProject    = errors.New(`no project name before "/_git/"`)
)

// Target is the organization, project and repository derived from a clone URL,
// plus the Provider of the host it points at. Project and Repo have both passed
// safepath.SanitizeComponent, so they are safe to use as directory names.
type Target struct {
	Org      string
	Project  string
	Repo     string
	Provider string
}

// Slug identifies the target in output: "project/repo" for Azure and Bitbucket
// (where the project is the workspace), or just the repository name when there is
// no project (github.com, codeberg.org).
func (t Target) Slug() string {
	if t.Project == "" {
		return t.Repo
	}
	return t.Project + "/" + t.Repo
}

// ParseRepoURL derives the organization, project and repository from a clone
// URL, and names its Provider.
//
// Azure DevOps has two host shapes and both must work:
//
//	https://<user>@dev.azure.com/<org>/<project>/_git/<repo>
//	https://<org>.visualstudio.com/<project>/_git/<repo>
//
// The project is always the last path segment before "/_git/", which is correct
// for both shapes (and for the legacy .../DefaultCollection/<project>/_git/...
// form). Only the organization needs to branch on the host.
//
// github.com, codeberg.org and bitbucket.org use
// https://<host>/<owner>/<repo>[.git] and have no project layer. github.com and
// codeberg.org leave Project empty and write the archive straight into the output
// directory; bitbucket.org nests the archive under its workspace, so Project is
// the workspace name. Any other host is only accepted with a "/_git/" segment
// (a self-hosted Azure DevOps Server).
//
// The label lines in a repo list are never consulted: in the committed lists a
// label reads "GunPG:" while the URL segment is "GnuPG", and several disagree in
// case or punctuation. The URL is the only trustworthy source.
func ParseRepoURL(raw string) (Target, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return Target{}, fmt.Errorf("parse URL: %w", err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		if u.Hostname() == "" {
			return Target{}, ErrNoHost
		}
	case "file":
		// A local repository is a legitimate thing to back up - a mirror, or a
		// fixture used to exercise the whole pipeline without touching Azure
		// DevOps - and git treats file:// as a real remote. It carries no host, so
		// the host requirement does not apply.
	default:
		return Target{}, ErrNotHTTP
	}

	// Split on the first occurrence: a project name can never contain "/_git/",
	// so anything after the first one belongs to the repository part.
	before, after, found := strings.Cut(u.Path, gitSeparator)
	if !found {
		// No "/_git/" segment: the only other shapes clonezip accepts are the
		// owner/repo clone URLs of github.com, codeberg.org and bitbucket.org.
		if h, ok := ownerRepoHosts[strings.ToLower(u.Hostname())]; ok {
			return ownerRepoTarget(u, h.provider, h.nestUnderOwner)
		}
		return Target{}, ErrNoGitSegment
	}

	repo, err := repoFromPath(after)
	if err != nil {
		return Target{}, err
	}

	segments := pathSegments(before)
	if len(segments) == 0 {
		return Target{}, ErrNoProject
	}
	project := segments[len(segments)-1]

	if _, err := safepath.SanitizeComponent(project); err != nil {
		return Target{}, fmt.Errorf("project name: %w", err)
	}
	if _, err := safepath.SanitizeComponent(repo); err != nil {
		return Target{}, fmt.Errorf("repository name: %w", err)
	}

	provider := ProviderAzure
	if strings.EqualFold(u.Scheme, "file") {
		provider = ProviderLocal
	}

	return Target{
		Org:      orgFromURL(u, segments),
		Project:  project,
		Repo:     repo,
		Provider: provider,
	}, nil
}

// ownerRepoTarget parses a clone URL of the shape
// https://<host>/<owner>/<repo>[.git] - the form github.com, codeberg.org and
// bitbucket.org use. There is no project layer in the URL: the owner is always
// reported as the organization. When nestUnderOwner is set (bitbucket.org), the
// owner is also used as the Project so the archive nests under an <owner>/
// directory; otherwise Project is left empty and the archive lands directly in
// the run's output directory.
func ownerRepoTarget(u *url.URL, provider string, nestUnderOwner bool) (Target, error) {
	segments := pathSegments(u.Path)
	if len(segments) != 2 {
		return Target{}, fmt.Errorf("%w: expected https://%s/<owner>/<repo>", ErrNoRepo, u.Hostname())
	}

	owner := segments[0]
	repo, err := repoFromPath(segments[1])
	if err != nil {
		return Target{}, err
	}

	if _, err := safepath.SanitizeComponent(owner); err != nil {
		return Target{}, fmt.Errorf("owner name: %w", err)
	}
	if _, err := safepath.SanitizeComponent(repo); err != nil {
		return Target{}, fmt.Errorf("repository name: %w", err)
	}

	target := Target{
		Org:      owner,
		Repo:     repo,
		Provider: provider,
	}
	if nestUnderOwner {
		target.Project = owner
	}
	return target, nil
}

// repoFromPath turns the path remainder after "/_git/" into a repository name.
func repoFromPath(after string) (string, error) {
	repo := strings.Trim(after, "/")
	if repo == "" {
		return "", ErrNoRepo
	}
	// A well-formed URL has nothing after the repository name. Anything else is
	// malformed rather than a repository whose name contains a slash.
	if strings.Contains(repo, "/") {
		return "", fmt.Errorf("%w: unexpected path after the repository name (%q)", ErrNoRepo, after)
	}
	// Azure DevOps percent-encodes spaces and similar in clone URLs.
	decoded, err := url.PathUnescape(repo)
	if err != nil {
		return "", fmt.Errorf("decode repository name %q: %w", repo, err)
	}
	// A ".git" suffix and a trailing slash are alternative spellings of the same
	// repository, and must normalise so dedupe recognises them.
	return strings.TrimSuffix(decoded, ".git"), nil
}

// orgFromURL determines the organization, which is the one part that depends on
// which host shape the URL uses.
func orgFromURL(u *url.URL, segments []string) string {
	host := strings.ToLower(u.Hostname())

	switch {
	// dev.azure.com/<org>/<project>/_git/<repo>
	case host == "dev.azure.com" || strings.HasSuffix(host, ".dev.azure.com"):
		if len(segments) >= 2 {
			return segments[0]
		}
		// Only a project in the path: the userinfo carries the organization in
		// every list entry of this shape.
		if u.User != nil {
			return u.User.Username()
		}
		return ""

	// <org>.visualstudio.com/<project>/_git/<repo>
	case strings.HasSuffix(host, ".visualstudio.com"):
		return strings.SplitN(host, ".", 2)[0]

	// A local repository has no organization. Its leading path segments are
	// directories, and on Windows the first is a drive letter, so naming one of
	// them would be actively misleading.
	case strings.EqualFold(u.Scheme, "file"):
		return "local"

	// An unrecognised host: prefer an explicit organization segment, and fall back
	// to the leading host label.
	case len(segments) >= 2:
		return segments[0]
	default:
		return strings.SplitN(host, ".", 2)[0]
	}
}

func pathSegments(p string) []string {
	var out []string
	for seg := range strings.SplitSeq(p, "/") {
		if seg != "" {
			out = append(out, seg)
		}
	}
	return out
}

// NormalizeURL returns a comparison key that treats spellings of the same
// repository as equal: the scheme and host are lowercased, credentials dropped,
// and a trailing slash or ".git" removed. Path case is preserved, because Azure
// DevOps reports names case-sensitively even though it matches them loosely.
func NormalizeURL(u *url.URL) string {
	path := strings.TrimSuffix(strings.TrimRight(u.Path, "/"), ".git")
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host) + path
}

// SanitizedURL renders a URL with all credentials removed, for the manifest and
// for restoring origin. Archives travel to USB sticks and file shares, so a
// personal access token pasted into the list must never be written into one.
func SanitizedURL(u *url.URL) string {
	clean := *u
	clean.User = nil
	return clean.String()
}

// DisplayURL renders a URL for logs and the console, keeping the username but
// masking any password.
func DisplayURL(u *url.URL) string {
	if u.User == nil {
		return u.String()
	}
	if _, hasPassword := u.User.Password(); !hasPassword {
		return u.String()
	}
	masked := *u
	masked.User = url.UserPassword(u.User.Username(), "***")
	// url.URL.String percent-encodes the mask; keep it readable.
	return strings.Replace(masked.String(), "%2A%2A%2A", "***", 1)
}
