package cli

import (
	"fmt"
	"os"
	"sort"

	"github.com/spf13/cobra"

	"github.com/ideonate/mcptui/internal/auth"
	"github.com/ideonate/mcptui/internal/config"
	"github.com/ideonate/mcptui/internal/session"
)

func (g *globals) authManager(p *config.Profile, interactive bool) (*auth.Manager, error) {
	if p.Transport != "http" {
		return nil, fmt.Errorf("profile %q uses stdio; OAuth only applies to HTTP servers", p.Name)
	}
	if p.Auth != config.AuthOAuth {
		return nil, fmt.Errorf("profile %q uses auth = %q, not oauth", p.Name, p.Auth)
	}
	return session.NewAuthManager(p, session.Options{
		Interactive:  interactive,
		NoBrowser:    g.noBrowser,
		CallbackHost: g.callbackHost,
		LoginUI:      &cliLoginUI{g: g},
		Log:          g.logf(),
	}), nil
}

func (g *globals) oneProfile(args []string) (*config.Profile, error) {
	p, _, rest, err := g.profileAndRest(args, 0)
	if err != nil {
		return nil, err
	}
	if len(rest) > 0 {
		return nil, fmt.Errorf("unexpected arguments: %v", rest)
	}
	return p, nil
}

func newAuthCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{Use: "auth", Short: "Manage OAuth credentials"}

	login := &cobra.Command{
		Use:   "login <profile>",
		Short: "Log in (forces a new login)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := g.oneProfile(args)
			if err != nil {
				return err
			}
			m, err := g.authManager(p, true)
			if err != nil {
				return err
			}
			ctx, cancel := signalContext()
			defer cancel()
			return m.Login(ctx)
		},
	}

	logout := &cobra.Command{
		Use:   "logout <profile>",
		Short: "Revoke tokens (if supported) and delete them locally",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := g.oneProfile(args)
			if err != nil {
				return err
			}
			m, err := g.authManager(p, false)
			if err != nil {
				return err
			}
			ctx, cancel := signalContext()
			defer cancel()
			if err := m.Logout(ctx); err != nil {
				return err
			}
			fmt.Fprintln(g.stderr, "Logged out.")
			return nil
		},
	}

	status := &cobra.Command{
		Use:   "status [profile]",
		Short: "Show scope, expiry, authorization server and client id",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var profiles []*config.Profile
			if len(args) == 0 && !g.hasTempProfile() {
				cfg, err := config.Load(g.configPath)
				if err != nil {
					return err
				}
				names := cfg.ProfileNames()
				sort.Strings(names)
				for _, n := range names {
					p, _ := cfg.Resolve(n)
					if p.Transport == "http" && p.Auth == config.AuthOAuth {
						profiles = append(profiles, p)
					}
				}
				if len(profiles) == 0 {
					fmt.Fprintln(g.stdout, "No OAuth profiles configured.")
					return nil
				}
			} else {
				p, err := g.oneProfile(args)
				if err != nil {
					return err
				}
				profiles = []*config.Profile{p}
			}
			for i, p := range profiles {
				if i > 0 {
					fmt.Fprintln(g.stdout)
				}
				m, err := g.authManager(p, false)
				if err != nil {
					return err
				}
				fmt.Fprintf(g.stdout, "%s (%s)\n", p.Name, p.URL)
				c, err := m.Status()
				if err != nil {
					return err
				}
				if c == nil {
					fmt.Fprintln(g.stdout, "  not logged in")
					continue
				}
				fmt.Fprintf(g.stdout, "  resource:             %s\n", c.Resource)
				fmt.Fprintf(g.stdout, "  authorization server: %s\n", c.AuthorizationServer)
				fmt.Fprintf(g.stdout, "  client id:            %s\n", c.ClientID)
				fmt.Fprintf(g.stdout, "  scope:                %s\n", orDash(c.Scope))
				fmt.Fprintf(g.stdout, "  access token:         %s, expires %s\n", auth.Redact(c.AccessToken), formatExpiry(c.ExpiresAt))
				if c.RefreshToken != "" {
					fmt.Fprintf(g.stdout, "  refresh token:        %s\n", auth.Redact(c.RefreshToken))
				} else {
					fmt.Fprintln(g.stdout, "  refresh token:        none")
				}
			}
			return nil
		},
	}

	token := &cobra.Command{
		Use:   "token <profile>",
		Short: "Print a valid access token (refreshing if needed)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := g.oneProfile(args)
			if err != nil {
				return err
			}
			m, err := g.authManager(p, false)
			if err != nil {
				return err
			}
			ctx, cancel := signalContext()
			defer cancel()
			tok, err := m.ValidToken(ctx)
			if err != nil {
				return explain(p, err)
			}
			fmt.Fprintln(g.stdout, tok)
			return nil
		},
	}

	reset := &cobra.Command{
		Use:   "reset-client <profile>",
		Short: "Forget the cached dynamic client registration",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := g.oneProfile(args)
			if err != nil {
				return err
			}
			m, err := g.authManager(p, false)
			if err != nil {
				return err
			}
			if err := m.ResetClient(); err != nil {
				return err
			}
			fmt.Fprintln(g.stderr, "Client registration cleared; the next login registers again.")
			return nil
		},
	}

	cmd.AddCommand(login, logout, status, token, reset)
	return cmd
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func newProfilesCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "profiles",
		Short: "List profiles",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(g.configPath)
			if err != nil {
				return err
			}
			if len(cfg.Profiles) == 0 {
				fmt.Fprintf(g.stdout, "No profiles in %s\n", cfg.Path())
				return nil
			}
			for _, n := range cfg.ProfileNames() {
				p, _ := cfg.Resolve(n)
				mark := " "
				if n == cfg.DefaultProfile {
					mark = "*"
				}
				badge := ""
				if p.EnvBadge != "" {
					badge = " [" + p.EnvBadge + "]"
				}
				fmt.Fprintf(g.stdout, "%s %s%s  %s  (%s, auth %s)\n", mark, n, badge, p.Target(), p.Transport, p.Auth)
			}
			return nil
		},
	}
	var (
		setDefault bool
		envBadge   string
		authMode   string
		scope      string
		clientID   string
	)
	add := &cobra.Command{
		Use:   "add <name> <url> | add <name> -- <command...>",
		Short: "Save a connection as a profile",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(g.configPath)
			if err != nil {
				return err
			}
			name := args[0]
			if _, exists := cfg.Profiles[name]; exists {
				return fmt.Errorf("profile %q already exists in %s", name, cfg.Path())
			}
			p := &config.Profile{Name: name, EnvBadge: envBadge, Auth: authMode, Scope: scope, ClientID: clientID}
			dash := cmd.ArgsLenAtDash()
			switch {
			case dash >= 0:
				if dash != 1 || len(args) < 2 {
					return fmt.Errorf("usage: mcptui profiles add <name> -- <command...>")
				}
				p.Transport = "stdio"
				p.Command = args[1:]
				if wd, err := os.Getwd(); err == nil {
					p.Cwd = wd
				}
			case g.profileURL != "":
				p.URL = g.profileURL
			case g.stdioCmd != "":
				argv, err := splitCommand(g.stdioCmd)
				if err != nil {
					return err
				}
				p.Transport = "stdio"
				p.Command = argv
			case len(args) == 2:
				p.URL = args[1]
			default:
				return fmt.Errorf("usage: mcptui profiles add <name> <url>  or  mcptui profiles add <name> -- <command...>")
			}
			if err := func() error {
				cp, _ := (&config.Config{Profiles: map[string]*config.Profile{name: p}}).Resolve(name)
				return cp.Validate()
			}(); err != nil {
				return err
			}
			cfg.Profiles[name] = p
			if setDefault || cfg.DefaultProfile == "" {
				cfg.DefaultProfile = name
			}
			if err := cfg.Save(); err != nil {
				return err
			}
			fmt.Fprintf(g.stderr, "Saved profile %q to %s\n", name, cfg.Path())
			return nil
		},
	}
	add.Flags().BoolVar(&setDefault, "default", false, "make this the default profile")
	add.Flags().StringVar(&envBadge, "env", "", "environment badge: dev, staging or prod")
	add.Flags().StringVar(&authMode, "auth", "", "auth mode: oauth, bearer or none")
	add.Flags().StringVar(&scope, "scope", "", "OAuth scope to request")
	add.Flags().StringVar(&clientID, "client-id", "", "pre-registered OAuth client_id")
	cmd.AddCommand(add)
	return cmd
}
