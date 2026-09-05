package repolist

import (
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseRepoURLBothHostShapes(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		org      string
		project  string
		repo     string
		provider string
	}{
		{
			// The shape used throughout azuredevops-heilingbrunner.yaml: the
			// organization is the first path segment, and also appears as
			// userinfo.
			name:    "dev.azure.com with user prefix",
			raw:     "https://heilingbrunner@dev.azure.com/heilingbrunner/AI-Experiments/_git/bubble",
			org:     "heilingbrunner",
			project: "AI-Experiments",
			repo:    "bubble",
		},
		{
			name:    "dev.azure.com without user prefix",
			raw:     "https://dev.azure.com/heilingbrunner/AI-Experiments/_git/Cowsays",
			org:     "heilingbrunner",
			project: "AI-Experiments",
			repo:    "Cowsays",
		},
		{
			name:    "repo name with dots and a hyphen",
			raw:     "https://dev.azure.com/heilingbrunner/AI-Experiments/_git/Cowsays.1-Hello",
			org:     "heilingbrunner",
			project: "AI-Experiments",
			repo:    "Cowsays.1-Hello",
		},
		{
			name:    "legacy DefaultCollection path",
			raw:     "https://example.visualstudio.com/DefaultCollection/MyProject/_git/MyRepo",
			org:     "example",
			project: "MyProject",
			repo:    "MyRepo",
		},
		{
			name:    "trailing slash",
			raw:     "https://dev.azure.com/org/Proj/_git/Repo/",
			org:     "org",
			project: "Proj",
			repo:    "Repo",
		},
		{
			// A ".git" suffix is an alternative spelling of the same
			// repository and must normalise away, or dedupe would miss it.
			name:    "dot-git suffix",
			raw:     "https://dev.azure.com/org/Proj/_git/Repo.git",
			org:     "org",
			project: "Proj",
			repo:    "Repo",
		},
		{
			name:    "percent-encoded space",
			raw:     "https://dev.azure.com/org/Proj/_git/My%20Repo",
			org:     "org",
			project: "Proj",
			repo:    "My Repo",
		},
		{
			name:    "unknown host with organization and project",
			raw:     "https://git.example.internal/MyOrg/MyProject/_git/MyRepo",
			org:     "MyOrg",
			project: "MyProject",
			repo:    "MyRepo",
		},
		{
			// A local repository, which git accepts as a remote. This is how the
			// whole pipeline can be exercised without touching Azure DevOps. There
			// is no organization to derive: the leading path segments are
			// directories, and on Windows the first is a drive letter.
			name:     "local file url",
			raw:      "file:///E:/tmp/e2e/MyProject/_git/MyRepo",
			org:      "local",
			project:  "MyProject",
			repo:     "MyRepo",
			provider: ProviderLocal,
		},
		{
			name:     "dev.azure.com provider",
			raw:      "https://dev.azure.com/org/Proj/_git/Repo",
			org:      "org",
			project:  "Proj",
			repo:     "Repo",
			provider: ProviderAzure,
		},
		{
			name:     "github with .git suffix - no project",
			raw:      "https://github.com/heilingbrunner/dirscan.git",
			org:      "heilingbrunner",
			project:  "",
			repo:     "dirscan",
			provider: ProviderGitHub,
		},
		{
			name:     "github without .git suffix",
			raw:      "https://github.com/heilingbrunner/dirscan",
			org:      "heilingbrunner",
			project:  "",
			repo:     "dirscan",
			provider: ProviderGitHub,
		},
		{
			name:     "github trailing slash",
			raw:      "https://github.com/heilingbrunner/dirscan/",
			org:      "heilingbrunner",
			project:  "",
			repo:     "dirscan",
			provider: ProviderGitHub,
		},
		{
			name:     "codeberg with .git suffix - no project",
			raw:      "https://codeberg.org/heilingbrunner/kanban.git",
			org:      "heilingbrunner",
			project:  "",
			repo:     "kanban",
			provider: ProviderCodeberg,
		},
		{
			// Bitbucket Cloud clone URLs are owner/repo like github.com, but the
			// workspace becomes the project so the archive nests under it.
			name:     "bitbucket with .git suffix - workspace is the project",
			raw:      "https://bitbucket.org/acme/widgets.git",
			org:      "acme",
			project:  "acme",
			repo:     "widgets",
			provider: ProviderBitbucket,
		},
		{
			name:     "bitbucket without .git suffix",
			raw:      "https://bitbucket.org/acme/widgets",
			org:      "acme",
			project:  "acme",
			repo:     "widgets",
			provider: ProviderBitbucket,
		},
		{
			name:     "bitbucket trailing slash",
			raw:      "https://bitbucket.org/acme/widgets/",
			org:      "acme",
			project:  "acme",
			repo:     "widgets",
			provider: ProviderBitbucket,
		},
		{
			name:     "bitbucket with user prefix",
			raw:      "https://alice@bitbucket.org/acme/widgets.git",
			org:      "acme",
			project:  "acme",
			repo:     "widgets",
			provider: ProviderBitbucket,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseRepoURL(tc.raw)
			if err != nil {
				t.Fatalf("ParseRepoURL(%q) = error %v", tc.raw, err)
			}
			if got.Org != tc.org {
				t.Errorf("Org = %q, want %q", got.Org, tc.org)
			}
			if got.Project != tc.project {
				t.Errorf("Project = %q, want %q", got.Project, tc.project)
			}
			if got.Repo != tc.repo {
				t.Errorf("Repo = %q, want %q", got.Repo, tc.repo)
			}
			if tc.provider != "" && got.Provider != tc.provider {
				t.Errorf("Provider = %q, want %q", got.Provider, tc.provider)
			}
		})
	}
}

func TestParseRepoURLTakesFirstGitSegment(t *testing.T) {
	// A project can never contain "/_git/", so the first occurrence separates
	// project from repository. A second occurrence means a malformed URL, not a
	// repository whose name contains slashes.
	if got, err := ParseRepoURL("https://dev.azure.com/org/Proj/_git/a/_git/b"); err == nil {
		t.Fatalf("a URL with two /_git/ segments was accepted as %+v, want rejection", got)
	}
}

func TestParseRepoURLRejects(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{"no scheme", "dev.azure.com/org/Proj/_git/Repo"},
		{"ssh scheme", "ssh://git@ssh.dev.azure.com/v3/org/Proj/Repo"},
		{"no host", "https:///org/Proj/_git/Repo"},
		{"no git segment", "https://dev.azure.com/org/Proj/Repo"},
		{"empty repo", "https://dev.azure.com/org/Proj/_git/"},
		{"empty repo trailing slashes", "https://dev.azure.com/org/Proj/_git///"},
		{"no project", "https://dev.azure.com/_git/Repo"},
		{"case sensitive separator", "https://dev.azure.com/org/Proj/_GIT/Repo"},
		{"reserved repo name", "https://dev.azure.com/org/Proj/_git/NUL"},
		{"repo name with asterisk", "https://dev.azure.com/org/Proj/_git/bad%2Aname"},
		{"github owner only", "https://github.com/heilingbrunner"},
		{"github extra path segments", "https://github.com/owner/repo/tree/main"},
		{"codeberg owner only", "https://codeberg.org/heilingbrunner"},
		{"bitbucket workspace only", "https://bitbucket.org/acme"},
		{"bitbucket extra path segments", "https://bitbucket.org/acme/widgets/src/main"},
		{"unknown host without git segment", "https://gitlab.com/owner/repo"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := ParseRepoURL(tc.raw); err == nil {
				t.Fatalf("ParseRepoURL(%q) = %+v, want rejection", tc.raw, got)
			}
		})
	}
}

func TestParseSkipsBlankEntries(t *testing.T) {
	// A blank scalar entry is silently dropped, the same as a blank line was in
	// the old plain-text format.
	input := "repos:\n" +
		"  - \"\"\n" +
		"  - https://heilingbrunner@dev.azure.com/heilingbrunner/AI-Experiments/_git/bubble\n"

	list, err := Parse([]byte(input))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(list.Entries) != 1 {
		t.Fatalf("got %d entries, want 1: %+v", len(list.Entries), list.Entries)
	}
	if list.HasErrors() {
		t.Errorf("unexpected errors: %v", list.Errors())
	}
	if got := list.Entries[0].Project; got != "AI-Experiments" {
		t.Errorf("entry project = %q, want AI-Experiments", got)
	}
}

func TestParseRejectsNonScalarEntry(t *testing.T) {
	// A "repos:" entry that is itself a list or a map is not a URL string;
	// treated as a bad entry rather than causing the whole document to fail.
	input := "repos:\n" +
		"  - [nested, list]\n" +
		"  - https://dev.azure.com/org/Proj/_git/Repo\n"

	list, err := Parse([]byte(input))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(list.Entries) != 1 {
		t.Fatalf("got %d entries, want 1: %+v", len(list.Entries), list.Entries)
	}
	if !list.HasErrors() {
		t.Fatal("a non-scalar entry was accepted, want a fatal error")
	}
	if got := list.Count(CodeBadURL); got != 1 {
		t.Errorf("badurl issues = %d, want 1", got)
	}
}

func TestParseDropsDuplicates(t *testing.T) {
	// The heilingbrunner list contains bubble twice. Left alone that
	// costs a full redundant clone and two writes to the same archive path.
	input := "repos:\n" +
		"  - https://heilingbrunner@dev.azure.com/heilingbrunner/AI-Experiments/_git/bubble\n" +
		"  - https://heilingbrunner@dev.azure.com/heilingbrunner/AI-Experiments/_git/bubble\n"

	list, err := Parse([]byte(input))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(list.Entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(list.Entries))
	}
	if got := list.Count(CodeDuplicate); got != 1 {
		t.Fatalf("duplicate issues = %d, want 1", got)
	}
	if list.HasErrors() {
		t.Errorf("a duplicate should be a notice, not an error: %v", list.Errors())
	}
}

func TestParseTreatsSpellingVariantsAsDuplicates(t *testing.T) {
	// Same repository, four spellings: credentials, host case, trailing slash
	// and a .git suffix. All must collapse to one clone.
	input := "repos:\n" +
		"  - https://user@dev.azure.com/org/Proj/_git/Repo\n" +
		"  - https://dev.azure.com/org/Proj/_git/Repo\n" +
		"  - https://DEV.AZURE.COM/org/Proj/_git/Repo/\n" +
		"  - https://dev.azure.com/org/Proj/_git/Repo.git\n"

	list, err := Parse([]byte(input))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(list.Entries) != 1 {
		t.Errorf("got %d entries, want 1: %+v", len(list.Entries), list.Entries)
	}
}

func TestParseKeepsSameRepoNameInDifferentProjects(t *testing.T) {
	// VisiWinPowerToys really does exist under Inosoft; reusing that name
	// under a second real project (Tools) must not collide. The output
	// layout nests by project, so this is not a collision and both must be
	// kept.
	input := "repos:\n" +
		"  - https://heilingbrunner@dev.azure.com/heilingbrunner/Inosoft/_git/VisiWinPowerToys\n" +
		"  - https://heilingbrunner@dev.azure.com/heilingbrunner/Tools/_git/VisiWinPowerToys\n"

	list, err := Parse([]byte(input))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(list.Entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(list.Entries))
	}
	if list.HasErrors() {
		t.Errorf("same repo name in different projects must not be an error: %v", list.Errors())
	}
}

func TestParseRejectsCaseOnlyCollision(t *testing.T) {
	// Two different repositories whose archive paths differ only by case. On
	// Windows the second would overwrite the first; on Linux both are written
	// and the backup set then cannot be restored on Windows. Either way the
	// list has to be fixed first, so this is fatal.
	input := "repos:\n" +
		"  - https://dev.azure.com/org/Proj/_git/Tools\n" +
		"  - https://dev.azure.com/org/Proj/_git/tools\n"

	list, err := Parse([]byte(input))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !list.HasErrors() {
		t.Fatal("a case-only collision was accepted, want a fatal error")
	}
	if got := list.Count(CodeCollision); got != 1 {
		t.Errorf("collision issues = %d, want 1", got)
	}
	// The message has to name both lines (2 and 3: line 1 is "repos:"),
	// otherwise it cannot be acted on.
	msg := list.Errors()[0].Msg
	for _, want := range []string{"line 2", "line 3"} {
		if !strings.Contains(msg, want) {
			t.Errorf("collision message %q does not mention %q", msg, want)
		}
	}
}

func TestParseReportsBadURLWithoutLosingGoodEntries(t *testing.T) {
	// One typo must not cost the rest of the run.
	input := "repos:\n" +
		"  - https://dev.azure.com/org/Proj/_git/Good\n" +
		"  - https://dev.azure.com/org/Proj/no-git-segment\n" +
		"  - https://dev.azure.com/org/Proj/_git/AlsoGood\n"

	list, err := Parse([]byte(input))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(list.Entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(list.Entries))
	}
	if got := list.Count(CodeBadURL); got != 1 {
		t.Errorf("badurl issues = %d, want 1", got)
	}
	// Line 1 is "repos:", so the bad entry is on line 3.
	if got := list.Errors()[0].LineNo; got != 3 {
		t.Errorf("bad URL reported on line %d, want 3", got)
	}
}

func TestParseWarnsAboutPlainHTTP(t *testing.T) {
	list, err := Parse([]byte("repos:\n  - http://dev.azure.com/org/Proj/_git/Repo\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(list.Entries) != 1 {
		t.Fatalf("got %d entries, want 1: http should warn, not reject", len(list.Entries))
	}
	if got := list.Count(CodeInsecure); got != 1 {
		t.Errorf("insecure issues = %d, want 1", got)
	}
	if list.HasErrors() {
		t.Errorf("http should be a warning, not an error: %v", list.Errors())
	}
}

func TestParseEmptyInput(t *testing.T) {
	for _, input := range []string{"", "\n", "   \n\t\n", "repos:\n", "repos: []\n"} {
		list, err := Parse([]byte(input))
		if err != nil {
			t.Fatalf("Parse(%q) = error %v", input, err)
		}
		if len(list.Entries) != 0 {
			t.Errorf("Parse(%q) produced %d entries, want none", input, len(list.Entries))
		}
		if len(list.Issues) != 0 {
			t.Errorf("Parse(%q) produced issues %v, want none", input, list.Issues)
		}
	}
}

func TestCredentialsNeverLeaveTheCloneURL(t *testing.T) {
	// Somebody will eventually paste a personal access token into a list. It may
	// reach git, and nothing else.
	const token = "s3cr3t"
	raw := "https://user:" + token + "@dev.azure.com/org/Proj/_git/Repo"

	list, err := Parse([]byte("repos:\n  - " + raw + "\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(list.Entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(list.Entries))
	}
	entry := list.Entries[0]

	if !strings.Contains(entry.CloneURL(), token) {
		t.Error("CloneURL lost the password; git needs it to authenticate")
	}
	if strings.Contains(entry.Display(), token) {
		t.Errorf("Display leaks the password: %q", entry.Display())
	}
	if !strings.Contains(entry.Display(), "user") {
		t.Errorf("Display dropped the username, which is useful context: %q", entry.Display())
	}
	if strings.Contains(entry.Source(), token) {
		t.Errorf("Source leaks the password: %q", entry.Source())
	}
	if strings.Contains(entry.Source(), "user") {
		t.Errorf("Source should carry no credentials at all: %q", entry.Source())
	}
}

func TestNormalizeURLIgnoresCredentialsAndCase(t *testing.T) {
	a := mustParseURL(t, "https://user:pw@DEV.AZURE.COM/org/Proj/_git/Repo.git")
	b := mustParseURL(t, "https://dev.azure.com/org/Proj/_git/Repo/")
	if NormalizeURL(a) != NormalizeURL(b) {
		t.Errorf("NormalizeURL differs:\n  %q\n  %q", NormalizeURL(a), NormalizeURL(b))
	}

	// Path case is significant: Azure DevOps reports names case-sensitively, and
	// the collision check exists precisely because the filesystem may not.
	c := mustParseURL(t, "https://dev.azure.com/org/Proj/_git/repo")
	if NormalizeURL(b) == NormalizeURL(c) {
		t.Error("NormalizeURL folded the path case; the collision check depends on it not doing that")
	}
}

// test/clonezip-repos.yaml is a small, checked-in repo list - safe to commit,
// always present, so parsing it is asserted outright rather than skipped.
func TestParseCommittedLists(t *testing.T) {
	path := filepath.Join("..", "..", "..", "test", "clonezip-repos.yaml")
	list, err := ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile(%q): %v", path, err)
	}

	if len(list.Entries) != 2 {
		t.Errorf("entries = %d, want 2", len(list.Entries))
	}
	if got := list.Count(CodeDuplicate); got != 0 {
		t.Errorf("duplicates = %d, want 0", got)
	}
	if list.HasErrors() {
		t.Fatalf("the committed list must parse without errors, got: %v", list.Errors())
	}

	// Every entry must be usable as a path and identify itself.
	for _, e := range list.Entries {
		if e.Project == "" || e.Repo == "" {
			t.Errorf("line %d: empty project or repo: %+v", e.LineNo, e)
		}
		if e.Org == "" {
			t.Errorf("line %d: no organization derived from %q", e.LineNo, e.Display())
		}
	}
}

func TestParseEntriesGitHubHasNoProject(t *testing.T) {
	list := ParseEntries([]string{
		"https://github.com/heilingbrunner/dirscan.git",
		"https://codeberg.org/heilingbrunner/kanban.git",
		"https://dev.azure.com/org/Proj/_git/Repo",
	})
	if list.HasErrors() {
		t.Fatalf("unexpected errors: %v", list.Errors())
	}
	if got := list.Entries[0].Project; got != "" {
		t.Errorf("github Project = %q, want empty", got)
	}
	if got := list.Entries[0].Slug(); got != "dirscan" {
		t.Errorf("github Slug = %q, want %q", got, "dirscan")
	}
	if got := list.Entries[1].Slug(); got != "kanban" {
		t.Errorf("codeberg Slug = %q, want %q", got, "kanban")
	}
	if got := list.Entries[2].Slug(); got != "Proj/Repo" {
		t.Errorf("azure Slug = %q, want %q", got, "Proj/Repo")
	}
}

func TestParseEntriesGitHubSameRepoNameCollides(t *testing.T) {
	// Two repos of the same name from different owners have no project to keep
	// them apart, so they would write the same <out>/tool.git.zip - fatal.
	list := ParseEntries([]string{
		"https://github.com/owner-a/tool.git",
		"https://github.com/owner-b/tool.git",
	})
	if got := list.Count(CodeCollision); got != 1 {
		t.Fatalf("collision issues = %d, want 1", got)
	}
}

func TestParseEntriesBitbucketNestsUnderWorkspace(t *testing.T) {
	list := ParseEntries([]string{
		"https://bitbucket.org/acme/widgets.git",
		"https://bitbucket.org/other/widgets.git",
	})
	if list.HasErrors() {
		t.Fatalf("unexpected errors: %v", list.Errors())
	}
	// Same repo name in two different workspaces does not collide: the archive
	// nests under <out>/<workspace>/.
	if got := list.Count(CodeCollision); got != 0 {
		t.Fatalf("collision issues = %d, want 0", got)
	}
	if got := list.Entries[0].Slug(); got != "acme/widgets" {
		t.Errorf("Slug = %q, want %q", got, "acme/widgets")
	}
}

func TestParseEntriesBitbucketSameRepoSameWorkspaceCollides(t *testing.T) {
	list := ParseEntries([]string{
		"https://bitbucket.org/acme/tool.git",
		"https://bitbucket.org/acme/Tool.git",
	})
	if got := list.Count(CodeCollision); got != 1 {
		t.Fatalf("collision issues = %d, want 1", got)
	}
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", raw, err)
	}
	return u
}
