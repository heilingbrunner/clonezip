package ui

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"golang.org/x/term"

	"github.com/heilingbrunner/clonezip/internal/common/pipeline"
)

// Mode selects how progress is rendered.
type Mode int

const (
	// ModeTUI is the interactive view.
	ModeTUI Mode = iota
	// ModePlain is one line per state change.
	ModePlain
)

// SelectMode decides how to render.
//
// Anything other than a real terminal gets plain output. A rendered view piped into a
// file produces escape sequences instead of a readable log, and that log is the only
// record of what a multi-hour unattended run actually did.
func SelectMode(out *os.File, noTUI bool) Mode {
	switch {
	case noTUI:
		return ModePlain
	case !term.IsTerminal(int(out.Fd())):
		return ModePlain
	case os.Getenv("TERM") == "dumb":
		return ModePlain
	case os.Getenv("CI") != "":
		return ModePlain
	}
	return ModeTUI
}

// Glyphs are the status markers. Two sets exist because a Windows console using a
// raster font renders unicode marks as empty boxes.
type Glyphs struct {
	OK      string
	Warn    string
	Fail    string
	Skip    string
	Spinner []string
}

var (
	asciiGlyphs = Glyphs{
		OK: "ok", Warn: "warn", Fail: "FAIL", Skip: "skip",
		Spinner: []string{"-", "\\", "|", "/"},
	}
	unicodeGlyphs = Glyphs{
		OK: "✓", Warn: "!", Fail: "✗", Skip: "·",
		Spinner: []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"},
	}
)

// PickGlyphs chooses a marker set.
//
// ASCII is the default on Windows because the console font, not the encoding, decides
// whether a checkmark is legible - and a box is worse than the word "ok". Elsewhere
// unicode is safe. Both are overridable.
func PickGlyphs(forceASCII, forceUnicode bool) Glyphs {
	if PreferASCII(forceASCII, forceUnicode) {
		return asciiGlyphs
	}
	return unicodeGlyphs
}

// PreferASCII reports whether output should stick to ASCII rather than unicode
// box-drawing and status marks, for anything outside Glyphs too - a bordered
// banner, for instance. Same rule PickGlyphs uses: Windows defaults to ASCII
// because the console font decides legibility, not the encoding; both flags
// override it.
func PreferASCII(forceASCII, forceUnicode bool) bool {
	switch {
	case forceUnicode:
		return false
	case forceASCII:
		return true
	case runtime.GOOS == "windows":
		return true
	}
	return false
}

// Styles, kept muted: this runs for hours and should not be tiring to watch.
var (
	styleDim   = lipgloss.NewStyle().Faint(true)
	styleOK    = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	styleWarn  = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	styleFail  = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	styleTitle = lipgloss.NewStyle().Bold(true)
)

// Run executes work, rendering with the chosen mode, and returns work's error.
//
// The summary is deliberately not printed here: the caller prints it after this
// returns, so it survives an interactive teardown and appears even after a Ctrl-C.
func Run(
	ctx context.Context,
	mode Mode,
	out *os.File,
	verbose bool,
	glyphs Glyphs,
	work func(context.Context, pipeline.Reporter) error,
) error {
	if mode == ModePlain {
		return work(ctx, NewPlain(out, verbose))
	}

	// A separate cancel so Ctrl-C can stop the work without the view being torn down
	// first: it has to stay alive long enough to report that it is finishing the
	// repositories already in flight.
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	model := newModel(cancel, verbose, glyphs)
	// No alt-screen. It clears the screen on exit, which is exactly the behaviour being
	// replaced: the PowerShell script called Clear-Host every iteration and left no
	// record of which repositories failed.
	program := tea.NewProgram(model, tea.WithContext(ctx), tea.WithOutput(out))

	errCh := make(chan error, 1)
	go func() {
		err := work(workCtx, teaReporter{program: program})
		errCh <- err
		program.Send(workDone{})
	}()

	if _, err := program.Run(); err != nil {
		// The view failed, but the work may still be running. Stop it and wait, so no
		// git children are orphaned holding the staging directory open.
		cancel()
		<-errCh
		return err
	}
	return <-errCh
}

// teaReporter forwards events into the program. tea.Program.Send is safe from any
// goroutine, which is what lets several workers report at once.
type teaReporter struct {
	program *tea.Program
}

func (r teaReporter) Report(e pipeline.Event) {
	r.program.Send(e)
}

// workDone tells the model the pipeline has finished.
type workDone struct{}

// tickMsg advances the spinner.
type tickMsg time.Time

// activeJob is one repository currently being worked on.
type activeJob struct {
	Slug   string
	Phase  pipeline.Phase
	Detail string
	Pct    int
	Done   int64
	Total  int64
	Start  time.Time
}

type model struct {
	cancel  func()
	verbose bool
	glyphs  Glyphs

	total   int
	done    int
	ok      int
	warn    int
	failed  int
	bytes   int64
	started time.Time

	active map[int]*activeJob

	width    int
	frame    int
	quitting bool
	finished bool
}

func newModel(cancel func(), verbose bool, glyphs Glyphs) *model {
	return &model{
		cancel:  cancel,
		verbose: verbose,
		glyphs:  glyphs,
		active:  map[int]*activeJob{},
		width:   100,
		started: time.Now(),
	}
}

func (m *model) Init() tea.Cmd {
	return tick()
}

func tick() tea.Cmd {
	return tea.Tick(120*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case tickMsg:
		m.frame++
		if m.finished {
			return m, tea.Quit
		}
		return m, tick()

	case workDone:
		m.finished = true
		return m, tea.Quit

	case pipeline.RunStarted:
		m.total = msg.Total
		m.started = time.Now()
		return m, tea.Println(m.header(msg))

	case pipeline.RepoStarted:
		slug := msg.Repo
		if msg.Project != "" {
			slug = msg.Project + "/" + msg.Repo
		}
		m.active[msg.Index] = &activeJob{
			Slug:  slug,
			Start: time.Now(),
		}
		return m, nil

	case pipeline.RepoPhase:
		if job, ok := m.active[msg.Index]; ok {
			job.Phase, job.Detail, job.Pct = msg.Phase, msg.Detail, 0
			job.Done, job.Total = 0, 0
		}
		return m, nil

	case pipeline.RepoPercent:
		if job, ok := m.active[msg.Index]; ok {
			job.Pct, job.Detail = msg.Pct, msg.Label
		}
		return m, nil

	case pipeline.RepoBytes:
		if job, ok := m.active[msg.Index]; ok {
			job.Done, job.Total = msg.Done, msg.Total
		}
		return m, nil

	case pipeline.RepoRetry:
		return m, tea.Println(styleWarn.Render(fmt.Sprintf("       retry %d/%d in %s: %s",
			msg.Attempt, msg.Of, msg.Wait, msg.Reason)))

	case pipeline.RepoLog:
		return m, m.handleLog(msg)

	case pipeline.RepoDone:
		return m, m.handleDone(msg)
	}
	return m, nil
}

func (m *model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "q":
		if m.quitting {
			// Asked twice: stop waiting and go.
			return m, tea.Quit
		}
		m.quitting = true
		// Cancelling rather than quitting immediately: leaving now would orphan git
		// children, which then hold the staging directory open and make cleanup fail.
		m.cancel()
		return m, tea.Println(styleWarn.Render(
			"  cancelling - finishing the repositories already in flight"))
	}
	return m, nil
}

func (m *model) handleLog(msg pipeline.RepoLog) tea.Cmd {
	// Info lines only update the live detail; printing them would bury the results.
	if msg.Level == pipeline.LevelInfo {
		if !m.verbose {
			return nil
		}
		return tea.Println(styleDim.Render("       " + msg.Line))
	}
	// Warnings and errors persist in the scrollback, because they are what you go
	// looking for afterwards.
	style := styleWarn
	if msg.Level == pipeline.LevelError {
		style = styleFail
	}
	return tea.Println(style.Render("       " + msg.Line))
}

func (m *model) handleDone(msg pipeline.RepoDone) tea.Cmd {
	delete(m.active, msg.Result.Index)
	m.done++
	m.bytes += msg.Result.Bytes

	switch msg.Result.Status {
	case pipeline.StatusOK:
		m.ok++
	case pipeline.StatusWarn:
		m.warn++
	case pipeline.StatusFailed:
		m.failed++
	}

	// Printed rather than rendered, so it scrolls into the terminal's real scrollback
	// above the live block and stays there - selectable, searchable, and still present
	// when the run ends.
	return tea.Println(m.resultLine(msg.Result))
}

func (m *model) header(msg pipeline.RunStarted) string {
	var b strings.Builder
	b.WriteString(styleTitle.Render(fmt.Sprintf("clonezip backup  %d repositories", msg.Total)))
	if msg.Jobs > 1 {
		b.WriteString("  jobs " + strconv.Itoa(msg.Jobs))
	}
	b.WriteString("\n")
	b.WriteString(styleDim.Render("out  " + msg.Out))
	b.WriteString("\n")
	b.WriteString(styleDim.Render("log  " + msg.Log))
	if msg.Depth > 0 {
		b.WriteString("\n")
		b.WriteString(styleWarn.Render(fmt.Sprintf(
			"depth %d - history is truncated; a depth-limited backup is lossy", msg.Depth)))
	}
	return b.String()
}

// resultLine renders one finished repository.
func (m *model) resultLine(r pipeline.Result) string {
	marker, style := m.glyphs.Skip, styleDim
	switch r.Status {
	case pipeline.StatusOK:
		marker, style = m.glyphs.OK, styleOK
	case pipeline.StatusWarn:
		marker, style = m.glyphs.Warn, styleWarn
	case pipeline.StatusFailed:
		marker, style = m.glyphs.Fail, styleFail
	}

	var tail []string
	if r.Bytes > 0 {
		tail = append(tail, Bytes(r.Bytes))
	}
	if r.Dur > 0 {
		tail = append(tail, Duration(r.Dur))
	}
	if r.Empty {
		tail = append(tail, "empty")
	}
	if r.LFSBytes > 0 {
		tail = append(tail, "lfs "+Bytes(r.LFSBytes))
	}
	if r.Reason != "" {
		tail = append(tail, r.Reason)
	}
	tail = append(tail, r.Warnings...)

	line := fmt.Sprintf("  %-4s %-44s %s",
		style.Render(marker), truncateMiddle(r.Slug(), 44), strings.Join(tail, "  "))
	return strings.TrimRight(line, " ")
}

// View renders only the live block: the repositories in flight plus the overall bar.
//
// Everything finished has already been printed into the scrollback, so this stays a
// fixed handful of lines whether the list holds three repositories or three hundred.
func (m *model) View() string {
	if m.finished {
		return ""
	}

	var b strings.Builder
	for _, index := range sortedKeys(m.active) {
		b.WriteString(m.activeLine(m.active[index]))
		b.WriteString("\n")
	}
	b.WriteString(m.progressLine())
	b.WriteString("\n")
	return b.String()
}

func (m *model) activeLine(job *activeJob) string {
	spin := m.glyphs.Spinner[m.frame%len(m.glyphs.Spinner)]

	detail := job.Phase.String()
	switch {
	case job.Total > 0:
		detail = fmt.Sprintf("%s %s / %s", job.Phase, Bytes(job.Done), Bytes(job.Total))
	case job.Pct > 0:
		detail = fmt.Sprintf("%s %s %d%%", job.Phase, job.Detail, job.Pct)
	case job.Detail != "":
		detail = job.Phase.String() + " " + job.Detail
	}

	return fmt.Sprintf("  %s %-44s %s  %s",
		spin, truncateMiddle(job.Slug, 44), detail,
		styleDim.Render(Duration(time.Since(job.Start))))
}

func (m *model) progressLine() string {
	elapsed := time.Since(m.started)

	counts := []string{fmt.Sprintf("%d/%d", m.done, m.total)}
	if m.warn > 0 {
		counts = append(counts, styleWarn.Render(fmt.Sprintf("warn %d", m.warn)))
	}
	if m.failed > 0 {
		counts = append(counts, styleFail.Render(fmt.Sprintf("fail %d", m.failed)))
	}
	counts = append(counts, "elapsed "+Duration(elapsed))
	if eta := m.eta(elapsed); eta != "" {
		counts = append(counts, "eta ~"+eta)
	}
	if m.bytes > 0 {
		counts = append(counts, Bytes(m.bytes))
	}
	if m.quitting {
		counts = append(counts, styleWarn.Render("cancelling"))
	}

	return "  " + m.bar() + "  " + strings.Join(counts, "   ")
}

// eta extrapolates from what has finished so far. Rough by nature, and meaningless
// before anything has completed, so it stays hidden until then.
func (m *model) eta(elapsed time.Duration) string {
	if m.done == 0 || m.total == 0 || m.done >= m.total {
		return ""
	}
	per := elapsed / time.Duration(m.done)
	return Duration(per * time.Duration(m.total-m.done))
}

const barWidth = 28

func (m *model) bar() string {
	if m.total == 0 {
		return "[" + strings.Repeat(".", barWidth) + "]"
	}
	filled := m.done * barWidth / m.total
	return "[" + strings.Repeat("#", filled) + strings.Repeat(".", barWidth-filled) + "]"
}

// truncateMiddle shortens a label from the middle, so both the project and the
// repository stay recognisable: "Contoso/Contoso.TIM…ADSQUERY".
func truncateMiddle(s string, limit int) string {
	if len(s) <= limit || limit < 5 {
		return s
	}
	keep := limit - 1
	head := keep / 2
	tail := keep - head
	return s[:head] + "…" + s[len(s)-tail:]
}

func sortedKeys(m map[int]*activeJob) []int {
	out := make([]int, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	sort.Ints(out)
	return out
}
