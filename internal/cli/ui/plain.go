// Package ui renders what the pipeline reports.
//
// The plain renderer is the reference implementation, and the one used whenever
// output is not a terminal: a redirected interactive view produces escape sequences
// instead of a readable log.
package ui

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/heilingbrunner/clonezip/internal/common/pipeline"
	"github.com/heilingbrunner/clonezip/internal/common/repolist"
)

// printer writes rendered output.
//
// Writing to a terminal or a redirected log has no failure worth acting on - there is
// nowhere left to report a broken stdout to - so the error is discarded deliberately,
// in one place, rather than at thirty call sites where it would only obscure the
// formatting.
type printer struct{ w io.Writer }

func (p printer) printf(format string, args ...any) {
	_, _ = fmt.Fprintf(p.w, format, args...)
}

func (p printer) println(args ...any) {
	_, _ = fmt.Fprintln(p.w, args...)
}

// Plain writes one line per state change.
type Plain struct {
	w       io.Writer
	verbose bool

	mu    sync.Mutex
	total int
	done  int
}

// NewPlain returns a reporter that writes plain lines to w. With verbose set, git's
// own output is included; without it, only warnings and errors are.
func NewPlain(w io.Writer, verbose bool) *Plain {
	return &Plain{w: w, verbose: verbose}
}

// Report implements pipeline.Reporter. It holds a lock for the duration of each
// line, so concurrent workers cannot interleave mid-line.
func (p *Plain) Report(event pipeline.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()

	switch e := event.(type) {
	case pipeline.RunStarted:
		p.total = e.Total
		p.printf("clonezip backup  %d repositories, jobs %d", e.Total, e.Jobs)
		p.printf("out  %s", e.Out)
		p.printf("log  %s", e.Log)
		if e.Depth > 0 {
			p.printf("depth %d - history is truncated; a depth-limited backup is lossy", e.Depth)
		}

	case pipeline.RepoPhase:
		if p.verbose {
			p.printf("      %s %s", e.Phase, e.Detail)
		}

	case pipeline.RepoRetry:
		p.printf("      retry %d/%d in %s: %s", e.Attempt, e.Of, e.Wait, e.Reason)

	case pipeline.RepoLog:
		if e.Level == pipeline.LevelInfo && !p.verbose {
			return
		}
		p.printf("      %s", e.Line)

	case pipeline.RepoDone:
		p.done++
		p.printResult(e.Result)
	}
}

func (p *Plain) printResult(r pipeline.Result) {
	// A counter on every line is what makes a long redirected run readable: you can
	// tell at a glance how far it got before it stopped.
	head := fmt.Sprintf("%s  %*d/%d  %-6s %s",
		time.Now().Format("15:04:05"), width(p.total), p.done, p.total, r.Status, r.Slug())

	var tail []string
	if r.Bytes > 0 {
		tail = append(tail, Bytes(r.Bytes))
	}
	if r.Dur > 0 {
		tail = append(tail, Duration(r.Dur))
	}
	if r.Empty {
		tail = append(tail, "empty, 0 refs")
	}
	if r.LFSBytes > 0 {
		tail = append(tail, "lfs "+Bytes(r.LFSBytes))
	}
	// Reason, not Err: the full error carries the git argv and the stderr tail,
	// which belong in the log rather than on a line next to 300 others.
	if r.Reason != "" {
		tail = append(tail, r.Reason)
	}
	tail = append(tail, r.Warnings...)

	if len(tail) == 0 {
		p.printf("%s", head)
		return
	}
	p.printf("%s  %s", head, strings.Join(tail, "  "))
}

func (p *Plain) printf(format string, args ...any) {
	printer{w: p.w}.printf(format+"\n", args...)
}

// RenderSummary writes the closing report.
//
// It runs after the renderer has finished, from the same code in both plain and
// interactive mode, so the outcome survives an interactive teardown and is printed
// even when the run was interrupted.
func RenderSummary(w io.Writer, s pipeline.Summary) {
	out := printer{w: w}
	out.printf("\nDone in %s.\n", Duration(s.Elapsed))

	counts := []string{fmt.Sprintf("%d ok", s.Count(pipeline.StatusOK))}
	if n := s.Count(pipeline.StatusWarn); n > 0 {
		counts = append(counts, fmt.Sprintf("%d warn", n))
	}
	if n := s.Count(pipeline.StatusFailed); n > 0 {
		counts = append(counts, fmt.Sprintf("%d failed", n))
	}
	if n := s.Count(pipeline.StatusSkipped); n > 0 {
		counts = append(counts, fmt.Sprintf("%d not attempted", n))
	}
	if n := s.IssueCount(repolist.CodeDuplicate); n > 0 {
		counts = append(counts, fmt.Sprintf("%d duplicate", n))
	}
	out.printf("  %d entries   %s   %s written\n",
		len(s.Results), strings.Join(counts, "   "), Bytes(s.TotalBytes()))

	if slowest, ok := s.Slowest(); ok && slowest.Dur > 0 {
		line := fmt.Sprintf("  slowest: %s %s", slowest.Slug(), Duration(slowest.Dur))
		if largest, ok := s.Largest(); ok && largest.Bytes > 0 {
			line += fmt.Sprintf("   largest: %s %s", largest.Slug(), Bytes(largest.Bytes))
		}
		out.println(line)
	}

	if warned := s.Warned(); len(warned) > 0 {
		out.printf("\nWarnings (%d):\n", len(warned))
		for _, r := range warned {
			out.printf("  %-40s %s\n", r.Slug(), strings.Join(r.Warnings, "; "))
		}
	}

	if failed := s.Failed(); len(failed) > 0 {
		out.printf("\nFailed (%d):\n", len(failed))
		for _, r := range failed {
			out.printf("  %-40s %s\n", r.Slug(), r.Reason)
		}
		if s.Out != "" {
			out.printf("  full git output: %s\n", filepath.Join(s.Out, "_failures"))
		}
	}

	if s.StuckDirs > 0 {
		out.printf("\n%d staging directory(ies) could not be deleted; remove them under %s by hand.\n",
			s.StuckDirs, s.Out)
	}

	out.printf("\n  out  %s\n", s.Out)
	if s.Log != "" {
		out.printf("  log  %s\n", s.Log)
	}
}

// Bytes renders a size the way a person reads it, in decimal (SI) units so it
// matches what a file manager or disk-properties dialog reports for the same
// files rather than reading ~7% smaller.
func Bytes(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	value := float64(n)
	for _, suffix := range []string{"KB", "MB", "GB", "TB"} {
		value /= unit
		if value < unit {
			if value < 10 {
				return fmt.Sprintf("%.1f %s", value, suffix)
			}
			return fmt.Sprintf("%.0f %s", value, suffix)
		}
	}
	return fmt.Sprintf("%.0f PB", value/unit)
}

// Duration renders an elapsed time compactly: seconds for short work, hours and
// minutes for a run that takes all afternoon.
func Duration(d time.Duration) string {
	switch {
	case d <= 0:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// width is how many columns a counter needs.
func width(total int) int {
	n := 1
	for total >= 10 {
		total /= 10
		n++
	}
	return n
}
