package doctorx

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// groupOrder is the order groups appear in the report, from what has to work to what
// is merely worth knowing.
var groupOrder = []string{GroupTools, GroupAuth, GroupStorage, GroupPlatform}

// printer writes report text.
//
// Writing to a terminal has no failure worth acting on - there is nowhere to report a
// broken stdout to - so the error is discarded deliberately, in one place, rather than
// at a dozen call sites where it would only obscure the formatting.
type printer struct{ w io.Writer }

func (p printer) printf(format string, args ...any) {
	_, _ = fmt.Fprintf(p.w, format, args...)
}

func (p printer) println(args ...any) {
	_, _ = fmt.Fprintln(p.w, args...)
}

// Render writes the report as an aligned table.
//
// Plain text on purpose: doctor is not a progress display, and a static table stays
// readable when piped into a file or pasted into an issue.
func Render(w io.Writer, report Report) {
	out := printer{w: w}
	out.printf("clonezip doctor        clonezip %s  %s\n\n", report.CloneZip, report.OS)

	width := 0
	for _, res := range report.Results {
		if n := len(res.Name); n > width {
			width = n
		}
	}

	for _, group := range groupOrder {
		results := resultsIn(report, group)
		if len(results) == 0 {
			continue
		}
		out.println(group)
		for _, res := range results {
			// A result with no Value but a Detail (typical for SKIP rows) would
			// otherwise render an empty first row and push the detail down a line,
			// making it look as if the third column jumped a row. Promote the
			// detail into the value slot so the reason sits next to the status.
			value, detail := res.Value, res.Detail
			if value == "" && detail != "" {
				value, detail = detail, ""
			}
			out.printf("  %-5s %-*s  %s\n", res.Status, width, res.Name, value)
			if detail != "" {
				out.printf("        %-*s  %s\n", width, "", detail)
			}
			// Hints are advice about something that is not right, so showing them on a
			// passing check would just be noise.
			if res.status == StatusOK {
				continue
			}
			for _, hint := range res.Hints {
				out.printf("        %-*s  %s\n", width, "", hint)
			}
		}
		out.println()
	}

	out.printf("%d ok  %d warn  %d fail  %d info  %d skip\n",
		report.Count(StatusOK), report.Count(StatusWarn), report.Count(StatusFail),
		report.Count(StatusInfo), report.Count(StatusSkip))
}

// RenderJSON writes the report as machine-readable output, which is also what makes
// the check list testable without parsing a table.
func RenderJSON(w io.Writer, report Report) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}

func resultsIn(report Report, group string) []Result {
	var out []Result
	for _, res := range report.Results {
		if res.Group == group {
			out = append(out, res)
		}
	}
	return out
}

// Check sets for the two working commands. Only what would make the command fail
// or silently misbehave is included; the rest belongs in the full report.
var (
	// ChecksBackup covers a backup run. The authentication probe is the valuable
	// one: it converts a whole afternoon of silent failures into one failure before
	// anything starts.
	ChecksBackup = []string{
		IDGitPresent, IDLFSPresent, IDDiskWritable, IDDiskFree, IDAuthProbe,
	}

	// ChecksRestore covers a restore. The LFS filters matter here and not during
	// backup, because materialising pointer files is what needs them.
	ChecksRestore = []string{
		IDGitPresent, IDLFSPresent, IDLFSFilters, IDDiskFree,
	}
)

// CheckError reports that the environment is not ready. The caller maps it to the
// usage exit code, because nothing was attempted.
type CheckError struct {
	Failed []Result
}

func (e *CheckError) Error() string {
	names := make([]string, 0, len(e.Failed))
	for _, res := range e.Failed {
		names = append(names, res.Name)
	}
	return "checks failed: " + strings.Join(names, ", ")
}

// Check runs the given subset of checks and reports whether work can start.
//
// On success it prints a single line, so an unattended run records what it verified.
// On failure it prints only the rows that failed, with their hints.
func Check(ctx context.Context, w io.Writer, o Options, ids []string) error {
	wanted := make(map[string]bool, len(ids))
	for _, id := range ids {
		wanted[id] = true
	}

	report := Run(ctx, "", o)

	var failed, passed []Result
	for _, res := range report.Results {
		if !wanted[res.ID] {
			continue
		}
		switch res.status {
		case StatusFail:
			failed = append(failed, res)
		case StatusOK:
			passed = append(passed, res)
		}
	}

	out := printer{w: w}

	if len(failed) > 0 {
		out.println("checks FAILED")
		for _, res := range failed {
			out.printf("  %-5s %s  %s\n", res.Status, res.Name, res.Detail)
			for _, hint := range res.Hints {
				out.printf("        %s\n", hint)
			}
		}
		out.println("run 'clonezip doctor' for the full report")
		return &CheckError{Failed: failed}
	}

	summary := make([]string, 0, len(passed))
	for _, res := range passed {
		if res.Value == "" {
			summary = append(summary, res.Name)
			continue
		}
		summary = append(summary, res.Name+" "+res.Value)
	}
	if len(summary) > 0 {
		out.printf("checks  %s\n", strings.Join(summary, "  "))
	}
	return nil
}
