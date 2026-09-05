// Package repolist reads the YAML files that list the repositories to back up
// (Azure DevOps, GitHub, Codeberg and Bitbucket), and derives organization,
// project and repository names from their URLs.
//
// A list is a single "repos:" key holding a sequence of clone URLs. Grouping
// or annotating entries is left to YAML's own "#" comment syntax and blank
// lines - clonezip never reads a comment, so nothing about a run depends on
// wording that can drift out of sync with the URLs it sits next to.
//
// ParseEntries additionally treats any entry whose own value starts with "#"
// as a disabled repo: skipped like a blank line, with no validation and no
// effect on a run. This is not comment-reading - it is a list *value*
// produced and consumed entirely by clonezip itself (the web UI's per-repo
// checkbox), distinct from a human-authored YAML comment sitting beside the
// list, which remains invisible to clonezip as before.
package repolist

import (
	"fmt"
	"net/url"
	"os"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Severity ranks an issue found while reading a list.
type Severity int

const (
	// SeverityNotice is informational: the run proceeds unchanged.
	SeverityNotice Severity = iota
	// SeverityWarning flags something suspicious that does not stop the run.
	SeverityWarning
	// SeverityError means the list must be fixed before any work can start.
	SeverityError
)

func (s Severity) String() string {
	switch s {
	case SeverityNotice:
		return "notice"
	case SeverityWarning:
		return "warning"
	case SeverityError:
		return "error"
	}
	return "unknown"
}

// Issue codes, stable enough to assert on in tests and to group by in output.
const (
	CodeDuplicate = "dup"       // the same repository listed twice
	CodeBadURL    = "badurl"    // unparseable, or not a recognized clone URL
	CodeInsecure  = "insecure"  // http:// rather than https://
	CodeCollision = "collision" // two entries would write the same archive path
)

// Issue records something noteworthy about one line, or about the list overall.
type Issue struct {
	LineNo   int // 1-based; 0 when the issue concerns the list as a whole
	Severity Severity
	Code     string
	Text     string // the offending line or name
	Msg      string
}

func (i Issue) String() string {
	if i.LineNo > 0 {
		return fmt.Sprintf("line %d: %s: %s", i.LineNo, i.Code, i.Msg)
	}
	return fmt.Sprintf("%s: %s", i.Code, i.Msg)
}

// Entry is one repository to back up.
type Entry struct {
	LineNo   int
	URL      *url.URL // as listed, including any credentials
	Org      string
	Project  string
	Repo     string
	Provider string // "azure", "github", "codeberg", "bitbucket" or "local"
}

// Slug identifies an entry in output. Azure repository names repeat across
// projects - 25 of them do in the committed lists - so the project is part of
// the identity there; Bitbucket entries are identified by "workspace/repo" for
// the same reason; github.com and codeberg.org entries have no project and are
// identified by the repository name alone.
func (e Entry) Slug() string {
	if e.Project == "" {
		return e.Repo
	}
	return e.Project + "/" + e.Repo
}

// CloneURL is the URL to hand to git, credentials included.
func (e Entry) CloneURL() string { return e.URL.String() }

// Display is the URL for logs and the console, with any password masked.
func (e Entry) Display() string { return DisplayURL(e.URL) }

// Source is the URL to record in a manifest and to restore origin to, with all
// credentials removed.
func (e Entry) Source() string { return SanitizedURL(e.URL) }

// List is the result of reading a repo list.
type List struct {
	Entries []Entry
	Issues  []Issue
}

// Errors returns the issues that must be fixed before the run can start.
func (l List) Errors() []Issue {
	var out []Issue
	for _, issue := range l.Issues {
		if issue.Severity == SeverityError {
			out = append(out, issue)
		}
	}
	return out
}

// HasErrors reports whether the list is unusable as it stands.
func (l List) HasErrors() bool { return len(l.Errors()) > 0 }

// Count returns how many issues carry the given code.
func (l List) Count(code string) int {
	n := 0
	for _, issue := range l.Issues {
		if issue.Code == code {
			n++
		}
	}
	return n
}

// file is the YAML document shape: a single "repos:" sequence of clone URLs.
type file struct {
	Repos []yaml.Node `yaml:"repos"`
}

// ParseFile reads and parses the list at path.
func ParseFile(path string) (List, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return List{}, err
	}
	return Parse(data)
}

// Parse reads a repo list.
//
// Parsing never fails on a bad entry: a list of 300 entries with one typo
// should still back up the other 299, so problems are collected as issues and
// the caller decides what to do. Only a collision between two entries is
// fatal, because it would silently lose one of them.
func Parse(data []byte) (List, error) {
	if len(strings.TrimSpace(string(data))) == 0 {
		return List{}, nil
	}

	var f file
	if err := yaml.Unmarshal(data, &f); err != nil {
		return List{}, fmt.Errorf("parse repo list: %w", err)
	}

	lines := make([]sourceLine, len(f.Repos))
	for i, node := range f.Repos {
		lines[i] = sourceLine{LineNo: node.Line, Value: node.Value, BadShape: node.Kind != yaml.ScalarNode}
	}
	return parseLines(lines), nil
}

// ParseEntries validates a plain list of clone URLs - the shape a job's
// inline "repos:" list takes in memory, whether just loaded from the
// unified service config file or being validated by a web API request
// before it is ever written to disk. Issue line numbers are the 1-based
// position within urls, since there is no YAML document to point a real
// line at.
func ParseEntries(urls []string) List {
	lines := make([]sourceLine, len(urls))
	for i, u := range urls {
		lines[i] = sourceLine{LineNo: i + 1, Value: u}
	}
	return parseLines(lines)
}

// sourceLine is one candidate URL with the line number to report issues
// against - a real YAML line via Parse, or a synthesized 1-based position
// via ParseEntries when there is no document to point to.
type sourceLine struct {
	LineNo   int
	Value    string
	BadShape bool // came from a non-scalar YAML node (Parse only)
}

func parseLines(lines []sourceLine) List {
	var list List
	seen := make(map[string]Entry)

	for _, l := range lines {
		lineNo := l.LineNo

		if l.BadShape {
			list.Issues = append(list.Issues, Issue{
				LineNo: lineNo, Severity: SeverityError, Code: CodeBadURL,
				Text: l.Value, Msg: "not a URL string",
			})
			continue
		}

		line := strings.TrimSpace(l.Value)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		u, err := url.Parse(line)
		if err != nil {
			list.Issues = append(list.Issues, Issue{
				LineNo: lineNo, Severity: SeverityError, Code: CodeBadURL,
				Text: line, Msg: err.Error(),
			})
			continue
		}

		target, err := ParseRepoURL(line)
		if err != nil {
			list.Issues = append(list.Issues, Issue{
				LineNo: lineNo, Severity: SeverityError, Code: CodeBadURL,
				Text: DisplayURL(u), Msg: err.Error(),
			})
			continue
		}

		if strings.EqualFold(u.Scheme, "http") {
			list.Issues = append(list.Issues, Issue{
				LineNo: lineNo, Severity: SeverityWarning, Code: CodeInsecure,
				Text: DisplayURL(u), Msg: "http:// sends credentials in the clear",
			})
		}

		key := NormalizeURL(u)
		if first, dup := seen[key]; dup {
			list.Issues = append(list.Issues, Issue{
				LineNo: lineNo, Severity: SeverityNotice, Code: CodeDuplicate,
				Text: target.Slug(),
				Msg:  fmt.Sprintf("already listed on line %d, skipped", first.LineNo),
			})
			continue
		}

		entry := Entry{
			LineNo:   lineNo,
			URL:      u,
			Org:      target.Org,
			Project:  target.Project,
			Repo:     target.Repo,
			Provider: target.Provider,
		}
		seen[key] = entry
		list.Entries = append(list.Entries, entry)
	}

	list.Issues = append(list.Issues, findCollisions(list.Entries)...)
	return list
}

// findCollisions reports distinct repositories whose archive paths differ only
// by case.
//
// This matters on both platforms, for opposite reasons. On Windows the second
// archive would overwrite the first. On Linux both would be written happily -
// and the resulting backup set could then not be restored on Windows, which is
// where it is needed. So the check is unconditional.
//
// Note that the same repository name under two different Azure projects is fine
// and common (VisiWinPowerToys appears under both Inosoft and Tools): the output
// layout nests by project, so those do not collide. The same holds for Bitbucket,
// which nests by workspace. github.com and codeberg.org repositories have no
// project, so two of the same name - from different owners, or differing only by
// case - would collide and are reported here.
func findCollisions(entries []Entry) []Issue {
	byKey := make(map[string][]Entry)
	var order []string
	for _, e := range entries {
		key := strings.ToLower(e.Slug())
		if _, ok := byKey[key]; !ok {
			order = append(order, key)
		}
		byKey[key] = append(byKey[key], e)
	}

	var issues []Issue
	for _, key := range order {
		group := byKey[key]
		if len(group) < 2 {
			continue
		}
		where := make([]string, 0, len(group))
		for _, e := range group {
			where = append(where, fmt.Sprintf("line %d (%s)", e.LineNo, e.Slug()))
		}
		issues = append(issues, Issue{
			LineNo:   group[1].LineNo,
			Severity: SeverityError,
			Code:     CodeCollision,
			Text:     group[0].Slug(),
			Msg: "entries would write the same archive path (same name, or differing only by case): " +
				strings.Join(where, ", "),
		})
	}
	return issues
}
