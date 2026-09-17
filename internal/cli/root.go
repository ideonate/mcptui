// Package cli implements the mcptui command line.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	"github.com/ideonate/mcptui/internal/config"
	"github.com/ideonate/mcptui/internal/mcp"
	"github.com/ideonate/mcptui/internal/session"
)

// ExitError carries a process exit code.
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("exit %d", e.Code)
	}
	return e.Err.Error()
}

func (e *ExitError) Unwrap() error { return e.Err }

// globals holds persistent flags.
type globals struct {
	configPath   string
	noBrowser    bool
	callbackHost string
	profileURL   string
	stdioCmd     string
	verbose      bool
	noHistory    bool
	noMouse      bool
	keyring      bool

	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer

	// runTUI is swappable for tests.
	runTUI func(g *globals, p *config.Profile, cfg *config.Config) error
}

var errKeyring = errors.New("--keyring is not supported yet; credentials are stored in credentials.json (mode 0600)")

// Main runs the CLI and returns the exit code.
func Main(args []string) int {
	g := &globals{stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr, runTUI: runTUI}
	root := newRoot(g)
	root.SetArgs(args)
	err := root.Execute()
	if err == nil {
		return 0
	}
	var ee *ExitError
	if errors.As(err, &ee) {
		if ee.Err != nil {
			fmt.Fprintln(g.stderr, "mcptui:", ee.Err)
		}
		return ee.Code
	}
	fmt.Fprintln(g.stderr, "mcptui:", err)
	return 1
}

func newRoot(g *globals) *cobra.Command {
	root := &cobra.Command{
		Use:   "mcptui [profile | url] [-- command args...]",
		Short: "An interactive terminal client for MCP servers",
		Long: `mcptui explores and exercises MCP servers from the terminal.

  mcptui                      choose, create or edit a profile
  mcptui local                open a saved profile
  mcptui https://host/mcp     connect to a URL (temporary profile)
  mcptui -- npx -y @modelcontextprotocol/server-everything
                              launch a stdio server (temporary profile)`,
		Version:       session.Version,
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(g.configPath)
			if err != nil {
				return err
			}
			var p *config.Profile
			if dash := cmd.ArgsLenAtDash(); dash >= 0 {
				if dash > 0 {
					return fmt.Errorf("use either a profile or -- command, not both")
				}
				if len(args) == 0 {
					return fmt.Errorf("missing command after --")
				}
				p = config.TemporaryCommand(args)
			} else {
				if len(args) > 1 {
					return fmt.Errorf("too many arguments (to launch a stdio server use: mcptui -- command args...)")
				}
				// A bare `mcptui` opens the profile picker (p stays nil).
				if len(args) == 1 || g.hasTempProfile() {
					arg := ""
					if len(args) == 1 {
						arg = args[0]
					}
					p, err = g.resolveProfile(cfg, arg)
					if err != nil {
						return err
					}
				}
			}
			if !term.IsTerminal(os.Stdin.Fd()) || !term.IsTerminal(os.Stdout.Fd()) {
				return fmt.Errorf("the TUI needs a terminal; for scripts use `mcptui call|prompt|read|ls|info`")
			}
			return g.runTUI(g, p, cfg)
		},
	}
	root.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		if g.keyring {
			return errKeyring
		}
		return nil
	}
	root.SetOut(g.stdout)
	root.SetErr(g.stderr)
	pf := root.PersistentFlags()
	pf.StringVar(&g.configPath, "config", "", "config file (default $XDG_CONFIG_HOME/mcptui/config.toml)")
	pf.BoolVar(&g.noBrowser, "no-browser", false, "don't open a browser for login; just show the URL")
	pf.StringVar(&g.callbackHost, "callback-host", "", "bind address for the OAuth callback listener (default 127.0.0.1)")
	pf.StringVar(&g.profileURL, "profile-url", "", "use a temporary HTTP profile for this URL")
	pf.StringVar(&g.stdioCmd, "stdio", "", "use a temporary stdio profile running this command")
	pf.BoolVarP(&g.verbose, "verbose", "v", false, "log transport and auth events to stderr (non-interactive commands)")
	pf.BoolVar(&g.noHistory, "no-history", false, "don't persist history to disk")
	pf.BoolVar(&g.noMouse, "no-mouse", false, "disable mouse support (lets the terminal handle text selection)")
	pf.BoolVar(&g.keyring, "keyring", false, "store credentials in the OS keyring (not yet supported)")

	root.AddCommand(
		newCallCmd(g), newPromptCmd(g), newReadCmd(g), newLsCmd(g), newInfoCmd(g),
		newAuthCmd(g), newProfilesCmd(g),
	)
	return root
}

// resolveProfile honours --profile-url / --stdio before a profile argument.
func (g *globals) resolveProfile(cfg *config.Config, arg string) (*config.Profile, error) {
	switch {
	case g.profileURL != "":
		return config.TemporaryURL(g.profileURL), nil
	case g.stdioCmd != "":
		argv, err := splitCommand(g.stdioCmd)
		if err != nil {
			return nil, err
		}
		return config.TemporaryCommand(argv), nil
	}
	return cfg.Resolve(arg)
}

// hasTempProfile reports whether a flag supplies the profile, so the first
// positional argument is not a profile name.
func (g *globals) hasTempProfile() bool { return g.profileURL != "" || g.stdioCmd != "" }

// profileAndRest splits positional args into the profile and the remainder.
func (g *globals) profileAndRest(args []string, minRest int) (*config.Profile, *config.Config, []string, error) {
	cfg, err := config.Load(g.configPath)
	if err != nil {
		return nil, nil, nil, err
	}
	if g.hasTempProfile() {
		p, err := g.resolveProfile(cfg, "")
		if len(args) < minRest {
			return nil, nil, nil, fmt.Errorf("expected at least %d argument(s)", minRest)
		}
		return p, cfg, args, err
	}
	if len(args) == 0 && minRest == 0 {
		p, err := cfg.Resolve("")
		return p, cfg, nil, err
	}
	if len(args) < minRest+1 {
		return nil, nil, nil, fmt.Errorf("expected a profile (or --profile-url/--stdio) and %d more argument(s)", minRest)
	}
	p, err := cfg.Resolve(args[0])
	return p, cfg, args[1:], err
}

func (g *globals) interactive() bool {
	return term.IsTerminal(os.Stdin.Fd())
}

func (g *globals) logf() mcp.Logf {
	if !g.verbose {
		return nil
	}
	return func(e mcp.Event) {
		fmt.Fprintf(g.stderr, "[%s] %s\n", e.Source, e.Text)
	}
}

// open connects a non-interactive session.
func (g *globals) open(ctx context.Context, p *config.Profile) (*session.Session, error) {
	interactive := g.interactive()
	return session.Open(ctx, session.Options{
		Profile:      p,
		Interactive:  interactive,
		NoBrowser:    g.noBrowser,
		CallbackHost: g.callbackHost,
		LoginUI:      &cliLoginUI{g: g},
		Log:          g.logf(),
	})
}

// signalContext cancels on Ctrl+C.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt)
}

// splitCommand splits a command string with simple shell quoting rules.
func splitCommand(s string) ([]string, error) {
	var (
		out   []string
		cur   strings.Builder
		quote rune
		inArg bool
		esc   bool
	)
	for _, r := range s {
		switch {
		case esc:
			cur.WriteRune(r)
			esc = false
		case r == '\\' && quote != '\'':
			esc = true
			inArg = true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
			inArg = true
		case r == ' ' || r == '\t' || r == '\n':
			if inArg {
				out = append(out, cur.String())
				cur.Reset()
				inArg = false
			}
		default:
			cur.WriteRune(r)
			inArg = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated quote in command")
	}
	if inArg {
		out = append(out, cur.String())
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("empty command")
	}
	return out, nil
}
