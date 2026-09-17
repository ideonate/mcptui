package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/ideonate/mcptui/internal/auth"
	"github.com/ideonate/mcptui/internal/config"
	"github.com/ideonate/mcptui/internal/mcp"
	"github.com/ideonate/mcptui/internal/paths"
)

func printJSON(w io.Writer, raw []byte) error {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		_, err = w.Write(raw)
		fmt.Fprintln(w)
		return err
	}
	buf.WriteByte('\n')
	_, err := w.Write(buf.Bytes())
	return err
}

// explain adds actionable hints to connection errors.
func explain(p *config.Profile, err error) error {
	if errors.Is(err, auth.ErrLoginRequired) && !strings.Contains(err.Error(), "mcptui auth login") {
		return fmt.Errorf("%w\nrun `mcptui auth login %s`", err, profileRef(p))
	}
	return err
}

func profileRef(p *config.Profile) string {
	if p.Temporary && p.Transport == "http" {
		return "--profile-url " + p.URL
	}
	return p.Name
}

// withSession opens a session, runs fn and closes it.
func (g *globals) withSession(p *config.Profile, fn func(ctx context.Context, s sessionLike) error) error {
	ctx, cancel := signalContext()
	defer cancel()
	if d, err := p.TimeoutDuration(); err != nil {
		return err
	} else if d > 0 {
		var c context.CancelFunc
		ctx, c = context.WithTimeout(ctx, d)
		defer c()
	}
	s, err := g.open(ctx, p)
	if err != nil {
		return explain(p, err)
	}
	defer s.Close()
	return explain(p, fn(ctx, s.Client))
}

// sessionLike is the subset of *mcp.Client commands use.
type sessionLike = *mcp.Client

func readArgs(stdin io.Reader, arg string) (json.RawMessage, error) {
	var b []byte
	var err error
	switch {
	case arg == "":
		return nil, nil
	case arg == "-":
		b, err = io.ReadAll(stdin)
	case strings.HasPrefix(arg, "@"):
		b, err = os.ReadFile(arg[1:])
	default:
		b = []byte(arg)
	}
	if err != nil {
		return nil, err
	}
	b = bytes.TrimSpace(b)
	var obj map[string]any
	if err := json.Unmarshal(b, &obj); err != nil {
		return nil, fmt.Errorf("arguments must be a JSON object: %w", err)
	}
	return b, nil
}

func newCallCmd(g *globals) *cobra.Command {
	var raw bool
	cmd := &cobra.Command{
		Use:   "call <profile> <tool> ['{json args}' | @file.json | -]",
		Short: "Call a tool and print the result JSON (exit 1 on isError)",
		Args:  cobra.RangeArgs(1, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, _, rest, err := g.profileAndRest(args, 1)
			if err != nil {
				return err
			}
			if len(rest) > 2 {
				return fmt.Errorf("too many arguments")
			}
			var argJSON json.RawMessage
			if len(rest) == 2 {
				if argJSON, err = readArgs(g.stdin, rest[1]); err != nil {
					return err
				}
			}
			return g.withSession(p, func(ctx context.Context, c sessionLike) error {
				res, ex := c.CallTool(ctx, rest[0], argJSON, nil)
				if ex.Err != nil {
					return ex.Err
				}
				if raw {
					printJSON(g.stdout, ex.Response)
				} else {
					printJSON(g.stdout, ex.Result)
				}
				if res.IsError {
					return &ExitError{Code: 1}
				}
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&raw, "raw", false, "print the full JSON-RPC response")
	return cmd
}

func newPromptCmd(g *globals) *cobra.Command {
	var raw bool
	cmd := &cobra.Command{
		Use:   "prompt <profile> <name> [key=value ...]",
		Short: "Get a prompt and print its messages",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, _, rest, err := g.profileAndRest(args, 1)
			if err != nil {
				return err
			}
			kv := map[string]string{}
			for _, a := range rest[1:] {
				k, v, ok := strings.Cut(a, "=")
				if !ok {
					return fmt.Errorf("prompt arguments must be key=value, got %q", a)
				}
				kv[k] = v
			}
			return g.withSession(p, func(ctx context.Context, c sessionLike) error {
				res, ex := c.GetPrompt(ctx, rest[0], kv)
				if ex.Err != nil {
					return ex.Err
				}
				if raw {
					return printJSON(g.stdout, ex.Response)
				}
				if res.Description != "" {
					fmt.Fprintf(g.stdout, "# %s\n\n", res.Description)
				}
				for i, m := range res.Messages {
					if i > 0 {
						fmt.Fprintln(g.stdout)
					}
					fmt.Fprintf(g.stdout, "[%s]\n%s\n", m.Role, contentText(m.Content))
				}
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&raw, "raw", false, "print the full JSON-RPC response")
	return cmd
}

func contentText(c mcp.Content) string {
	switch c.Type {
	case "text":
		return c.Text
	case "image", "audio":
		return fmt.Sprintf("<%s %s, %d bytes base64>", c.Type, c.MimeType, len(c.Data))
	case "resource_link":
		return fmt.Sprintf("<resource link %s %s>", c.URI, c.Name)
	case "resource":
		if c.Resource != nil {
			if c.Resource.Text != nil {
				return *c.Resource.Text
			}
			return fmt.Sprintf("<resource %s %s, blob>", c.Resource.URI, c.Resource.MimeType)
		}
	}
	b, _ := json.Marshal(c)
	return string(b)
}

func newReadCmd(g *globals) *cobra.Command {
	var raw bool
	var output string
	cmd := &cobra.Command{
		Use:   "read <profile> <uri>",
		Short: "Read a resource and print its contents",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, _, rest, err := g.profileAndRest(args, 1)
			if err != nil {
				return err
			}
			return g.withSession(p, func(ctx context.Context, c sessionLike) error {
				res, ex := c.ReadResource(ctx, rest[0])
				if ex.Err != nil {
					return ex.Err
				}
				if raw {
					return printJSON(g.stdout, ex.Response)
				}
				var out bytes.Buffer
				for _, rc := range res.Contents {
					switch {
					case rc.Text != nil:
						out.WriteString(*rc.Text)
						if !strings.HasSuffix(*rc.Text, "\n") {
							out.WriteByte('\n')
						}
					case rc.Blob != nil:
						b, err := base64.StdEncoding.DecodeString(*rc.Blob)
						if err != nil {
							return fmt.Errorf("decode blob %s: %w", rc.URI, err)
						}
						if output == "" {
							return fmt.Errorf("%s is binary (%s, %d bytes); use -o <file> or --raw", rc.URI, rc.MimeType, len(b))
						}
						out.Write(b)
					}
				}
				if output != "" && output != "-" {
					return os.WriteFile(output, out.Bytes(), 0o644)
				}
				_, err := g.stdout.Write(out.Bytes())
				return err
			})
		},
	}
	cmd.Flags().BoolVar(&raw, "raw", false, "print the full JSON-RPC response")
	cmd.Flags().StringVarP(&output, "output", "o", "", "write contents to a file (needed for binary resources; - for stdout)")
	return cmd
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 100 {
		s = s[:99] + "…"
	}
	return s
}

func newLsCmd(g *globals) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:       "ls <profile> tools|prompts|resources|templates",
		Short:     "List tools, prompts, resources or resource templates",
		Args:      cobra.RangeArgs(1, 2),
		ValidArgs: []string{"tools", "prompts", "resources", "templates"},
		RunE: func(cmd *cobra.Command, args []string) error {
			p, _, rest, err := g.profileAndRest(args, 1)
			if err != nil {
				return err
			}
			what := rest[0]
			return g.withSession(p, func(ctx context.Context, c sessionLike) error {
				tw := tabwriter.NewWriter(g.stdout, 2, 4, 2, ' ', 0)
				defer tw.Flush()
				emit := func(v any) error {
					b, _ := json.Marshal(v)
					return printJSON(g.stdout, b)
				}
				switch what {
				case "tools":
					items, _, err := c.ListTools(ctx)
					if err != nil {
						return err
					}
					if asJSON {
						return emit(items)
					}
					for _, t := range items {
						fmt.Fprintf(tw, "%s\t%s\n", t.Name, firstLine(t.Description))
					}
				case "prompts":
					items, _, err := c.ListPrompts(ctx)
					if err != nil {
						return err
					}
					if asJSON {
						return emit(items)
					}
					for _, t := range items {
						fmt.Fprintf(tw, "%s\t%s\n", t.Name, firstLine(t.Description))
					}
				case "resources":
					items, _, err := c.ListResources(ctx)
					if err != nil {
						return err
					}
					if asJSON {
						return emit(items)
					}
					for _, t := range items {
						fmt.Fprintf(tw, "%s\t%s\t%s\n", t.URI, t.MimeType, t.Name)
					}
				case "templates":
					items, _, err := c.ListResourceTemplates(ctx)
					if err != nil {
						return err
					}
					if asJSON {
						return emit(items)
					}
					for _, t := range items {
						fmt.Fprintf(tw, "%s\t%s\t%s\n", t.URITemplate, t.MimeType, t.Name)
					}
				default:
					return fmt.Errorf("unknown list %q: use tools, prompts, resources or templates", what)
				}
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func newInfoCmd(g *globals) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "info <profile>",
		Short: "Show serverInfo, capabilities and instructions",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, _, _, err := g.profileAndRest(args, 0)
			if err != nil {
				return err
			}
			return g.withSession(p, func(ctx context.Context, c sessionLike) error {
				init := c.InitializeResult()
				if asJSON {
					b, _ := json.Marshal(map[string]any{
						"protocolVersion": init.ProtocolVersion,
						"serverInfo":      init.ServerInfo,
						"capabilities":    init.RawCapabilities,
						"instructions":    init.Instructions,
					})
					return printJSON(g.stdout, b)
				}
				fmt.Fprintf(g.stdout, "server:    %s %s\n", init.ServerInfo.Name, init.ServerInfo.Version)
				if init.ServerInfo.Title != "" {
					fmt.Fprintf(g.stdout, "title:     %s\n", init.ServerInfo.Title)
				}
				fmt.Fprintf(g.stdout, "protocol:  %s\n", init.ProtocolVersion)
				fmt.Fprintf(g.stdout, "target:    %s\n", p.Target())
				fmt.Fprintln(g.stdout, "capabilities:")
				var caps bytes.Buffer
				_ = json.Indent(&caps, init.RawCapabilities, "  ", "  ")
				fmt.Fprintf(g.stdout, "  %s\n", caps.String())
				if init.Instructions != "" {
					fmt.Fprintf(g.stdout, "instructions:\n%s\n", init.Instructions)
				}
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

// cliLoginUI prints the authorize URL and accepts a pasted redirect URL.
type cliLoginUI struct{ g *globals }

func (u *cliLoginUI) ShowLogin(p auth.LoginPrompt) {
	w := u.g.stderr
	fmt.Fprintf(w, "\nOpen this URL to log in:\n\n%s\n\n", p.AuthURL)
	if err := paths.EnsureDir(paths.StateDir()); err == nil {
		f := filepath.Join(paths.StateDir(), "login-url.txt")
		if os.WriteFile(f, []byte(p.AuthURL+"\n"), 0o600) == nil {
			fmt.Fprintf(w, "(also saved to %s)\n\n", f)
		}
	}
	fmt.Fprintf(w, "Waiting for the redirect to %s\n", p.RedirectURI)
	fmt.Fprintf(w, "Browser can't reach the callback? Paste the URL from the address bar here and press Enter:\n")
	go func() {
		buf := make([]byte, 0, 4096)
		one := make([]byte, 1)
		for {
			n, err := u.g.stdin.Read(one)
			if n == 1 {
				if one[0] == '\n' {
					if line := strings.TrimSpace(string(buf)); line != "" {
						p.Submit(line)
						return
					}
					buf = buf[:0]
					continue
				}
				buf = append(buf, one[0])
			}
			if err != nil {
				return
			}
		}
	}()
}

func (u *cliLoginUI) LoginFinished(scope string, err error) {
	if err != nil {
		fmt.Fprintf(u.g.stderr, "Login failed: %v\n", err)
		return
	}
	if scope != "" {
		fmt.Fprintf(u.g.stderr, "Logged in (scope: %s)\n", scope)
	} else {
		fmt.Fprintln(u.g.stderr, "Logged in")
	}
}

func formatExpiry(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	d := time.Until(t).Round(time.Second)
	if d < 0 {
		return fmt.Sprintf("expired %s ago (%s)", -d, t.Local().Format(time.RFC3339))
	}
	return fmt.Sprintf("in %s (%s)", d, t.Local().Format(time.RFC3339))
}
