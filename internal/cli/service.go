package cli

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/heilingbrunner/clonezip/internal/cli/ui"
	"github.com/heilingbrunner/clonezip/internal/common/lockx"
	"github.com/heilingbrunner/clonezip/internal/service"
	"github.com/heilingbrunner/clonezip/internal/service/webapi"
)

// serviceConfigTemplate is the starter config written by "clonezip service init":
// a minimal, commented example any user can adapt, as opposed to the
// personal working configs under assets/ used for development and testing.
//
//go:embed templates/clonezip-service.yaml
var serviceConfigTemplate []byte

// minBannerWidth is the box's inner content width when the terminal size
// can't be detected (output redirected to a file/pipe) or is narrower than
// this - keeps the banner from looking cramped with only a couple of groups.
const minBannerWidth = 70

// serviceOptions holds the flags of the service command.
type serviceOptions struct {
	global *globalOptions

	ConfigPath  string
	Listen      string
	CheckConfig bool
}

func newServiceCmd(global *globalOptions) *cobra.Command {
	opts := &serviceOptions{global: global}

	cmd := &cobra.Command{
		Use:   "service",
		Short: "Run scheduled backups in the background with a monitoring web page",
		Long: "Reads a service config file listing named backup groups, each with its\n" +
			"own repo list and cron schedule, and runs them unattended: firing groups\n" +
			"on their schedule, and serving a web page at --listen to watch progress,\n" +
			"schedule and run history, and to trigger a group on demand.\n\n" +
			"clonezip service runs in the foreground; use systemd, a Windows Service or\n" +
			"Task Scheduler to keep it running in the background.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runService(cmd.Context(), opts)
		},
	}

	f := cmd.Flags()
	f.StringVar(&opts.ConfigPath, "config", "clonezip-service.yaml", "path to the service config file")
	f.StringVar(&opts.Listen, "listen", "", "override the listen address from the config file")
	f.BoolVar(&opts.CheckConfig, "check-config", false, "validate the config file and exit")

	cmd.AddCommand(newServiceInitCmd())

	return cmd
}

// newServiceInitCmd builds "clonezip service init", which writes the starter
// clonezip-service.yaml template so a user has something to edit instead of
// writing a config from scratch.
func newServiceInitCmd() *cobra.Command {
	var path string
	var interactive bool

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Write a starter clonezip-service.yaml config file",
		Long: "Writes the clonezip-service.yaml template to disk. An existing file is\n" +
			"never overwritten - edit it, or remove it first, then run init again.\n\n" +
			"With --interactive, the service settings and the per-group defaults are\n" +
			"filled in through a form first; the example groups are still written for\n" +
			"you to edit afterwards.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if interactive {
				return runServiceInitInteractive(cmd.Context(), cmd.OutOrStdout(), path)
			}
			return runServiceInit(cmd.OutOrStdout(), path)
		},
	}

	f := cmd.Flags()
	f.StringVar(&path, "config", "clonezip-service.yaml", "path to write the config file to")
	f.BoolVarP(&interactive, "interactive", "i", false,
		"prompt for the service settings and defaults with an interactive form")

	return cmd
}

func runServiceInit(w io.Writer, path string) error {
	exists, err := serviceConfigExists(w, path)
	if err != nil || exists {
		return err
	}

	if err := os.WriteFile(path, serviceConfigTemplate, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	_, err = fmt.Fprintf(w, "clonezip: wrote %s\n", path)
	return err
}

// serviceConfigExists reports whether path is already there, having told the
// user so - init never overwrites, whichever way it was invoked. A true
// result means the caller should stop without writing anything.
func serviceConfigExists(w io.Writer, path string) (bool, error) {
	_, err := os.Stat(path)
	switch {
	case err == nil:
		_, err := fmt.Fprintf(w, "clonezip: %s already exists, leaving it unchanged\n", path)
		return true, err
	case os.IsNotExist(err):
		return false, nil
	default:
		return false, fmt.Errorf("check %s: %w", path, err)
	}
}

func runService(ctx context.Context, opts *serviceOptions) error {
	cfg, err := service.LoadConfig(opts.ConfigPath)
	if err != nil {
		if verr, ok := errors.AsType[*service.ValidationError](err); ok {
			printConfigErrors(os.Stderr, opts.ConfigPath, verr)
			return &ConfigInvalidError{}
		}
		return usagef("load %s: %w", opts.ConfigPath, err)
	}

	// listenAddr is what the HTTP server actually binds to; --listen is a
	// one-off runtime override and must never be written back into cfg,
	// since a config edit made through the web page persists cfg.Listen
	// as-is - baking the override in permanently on the next edit otherwise.
	listenAddr := cfg.Listen
	if opts.Listen != "" {
		listenAddr = opts.Listen
	}

	if opts.CheckConfig {
		unit := "groups"
		if len(cfg.Groups) == 1 {
			unit = "group"
		}
		_, err := fmt.Fprintf(os.Stdout, "clonezip: %s is valid, %d %s, listening on %s\n",
			opts.ConfigPath, len(cfg.Groups), unit, listenAddr)
		return err
	}

	lockPath, err := filepath.Abs(opts.ConfigPath)
	if err != nil {
		return usagef("resolve %s: %w", opts.ConfigPath, err)
	}
	lock, err := lockx.Acquire(lockPath + ".lock")
	if errors.Is(err, lockx.ErrLocked) {
		return &PreconditionError{Err: fmt.Errorf("service already running for %s", opts.ConfigPath)}
	}
	if err != nil {
		return usagef("lock %s: %w", opts.ConfigPath, err)
	}
	// Best-effort: the process is exiting either way, and the OS releases the
	// underlying lock on close regardless, so a failed Release here has
	// nothing further to affect.
	defer func() { _ = lock.Release() }()

	svc := service.New(cfg, Version, opts.ConfigPath)
	handler := webapi.NewServer(svc.Store, svc.Scheduler, svc.ConfigStore, svc.Restore)

	printBanner(os.Stdout, cfg, listenAddr, opts.ConfigPath, ui.PreferASCII(opts.global.ASCII, opts.global.Unicode))

	if err := svc.Run(ctx, listenAddr, handler); err != nil {
		if ctx.Err() != nil {
			return errInterrupted
		}
		return err
	}
	return nil
}

// printConfigErrors lists every problem service.Validate found, one per line,
// instead of the single semicolon-joined line ValidationError.Error() returns
// - a config with several mistakes (e.g. many groups whose "out" escapes the
// backup root) is otherwise an unreadable run-on sentence.
func printConfigErrors(w io.Writer, configPath string, verr *service.ValidationError) {
	unit := "problems"
	if len(verr.Fields) == 1 {
		unit = "problem"
	}
	// Best-effort: this only reports validation failures already carried in
	// verr, so a write failure here has nothing further to surface as an error.
	_, _ = fmt.Fprintf(w, "clonezip: %s has %d %s:\n", configPath, len(verr.Fields), unit)
	for _, f := range verr.Fields {
		_, _ = fmt.Fprintf(w, "  - %s\n", f.Error())
	}
}

// printBanner shows what runService is about to do: the running binary,
// the dashboard address, the config it loaded, every group's schedule, and
// how to stop. listenAddr
// is what the server actually binds to - cfg.Listen with any --listen override
// already applied.
func printBanner(w io.Writer, cfg service.Config, listenAddr, configPath string, asciiOnly bool) {
	bold := lipgloss.NewStyle().Bold(true)
	dim := lipgloss.NewStyle().Faint(true)
	accent := lipgloss.NewStyle().Foreground(lipgloss.Color("4")).Bold(true)

	groups := cfg.ResolvedGroups()
	nameWidth, schedWidth := 0, 0
	for _, g := range groups {
		nameWidth = max(nameWidth, len(g.Name))
		schedWidth = max(schedWidth, len(g.Schedule))
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n", bold.Render("clonezip service "+Version))
	if binaryPath, err := os.Executable(); err == nil {
		fmt.Fprintf(&b, "binary    : %s\n", binaryPath)
	}
	fmt.Fprintf(&b, "config    : %s\n", configPath)
	fmt.Fprintf(&b, "backups   : %s\n", cfg.BackupRoot())
	fmt.Fprintf(&b, "dashboard : %s\n", accent.Render(dashboardURL(listenAddr)))
	fmt.Fprintf(&b, "actions   : %s\n", strings.Join(cfg.EffectiveAllowActionsFrom(), ", "))
	bullet := "•"
	if asciiOnly {
		bullet = "-"
	}
	fmt.Fprintf(&b, "\n%s\n", dim.Render(fmt.Sprintf("%d group(s):", len(groups))))
	for _, g := range groups {
		state := "enabled"
		if !g.Enabled {
			state = dim.Render("disabled")
		}
		fmt.Fprintf(&b, "  %s %-*s  %-*s  %s\n", bullet, nameWidth, g.Name, schedWidth, g.Schedule, state)
	}
	fmt.Fprintf(&b, "\n%s\n", dim.Render("Press Ctrl+C to stop."))

	border := lipgloss.RoundedBorder()
	if asciiOnly {
		border = lipgloss.NormalBorder()
	}
	// lipgloss wraps text at Width minus its own horizontal padding (2+2 here),
	// so the box must be at least maxLineWidth+4 wide or a long content line
	// (e.g. the binary path) wraps instead of the box simply overflowing the
	// terminal, which looks worse.
	maxLineWidth := 0
	for line := range strings.SplitSeq(b.String(), "\n") {
		if lineWidth := lipgloss.Width(line); lineWidth > maxLineWidth {
			maxLineWidth = lineWidth
		}
	}
	contentWidth := max(minBannerWidth, maxLineWidth+4)
	if termWidth := bannerWidth(w); termWidth > contentWidth {
		contentWidth = termWidth
	}
	box := lipgloss.NewStyle().Border(border).Padding(0, 2).Width(contentWidth)
	// Best-effort: a banner is cosmetic, so a write failure to w here is not
	// worth surfacing as an error from a function that otherwise returns none.
	_, _ = fmt.Fprintln(w, box.Render(strings.TrimRight(b.String(), "\n")))
}

// bannerWidth reports the usable inner width of w's terminal, or 0 if w
// isn't a terminal (piped/redirected output) or its size can't be read.
func bannerWidth(w io.Writer) int {
	f, ok := w.(*os.File)
	if !ok {
		return 0
	}
	width, _, err := term.GetSize(int(f.Fd()))
	if err != nil {
		return 0
	}
	// Border (2 cols) + horizontal padding (4 cols).
	return width - 6
}

// dashboardURL turns a listen address such as ":8091" or "0.0.0.0:8091" into
// a URL a browser can actually open - binding to every interface is not
// itself a reachable hostname.
func dashboardURL(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return listen
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		if lanIP := outboundIP(); lanIP != "" {
			host = lanIP
		} else {
			host = "localhost"
		}
	}
	return "http://" + net.JoinHostPort(host, port)
}

// outboundIP returns the local IP address that would be used to reach the
// network, without sending any traffic - dialing UDP just asks the OS to
// pick a route. Used to show a LAN-reachable dashboard URL when the service
// binds to a wildcard address (0.0.0.0/::), since that address itself isn't
// something a browser can connect to.
func outboundIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return ""
	}
	defer func() { _ = conn.Close() }()

	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return ""
	}
	return addr.IP.String()
}
