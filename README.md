# mcptui

**A terminal UI for exploring and testing [Model Context Protocol](https://modelcontextprotocol.io) (MCP) servers.** Connect to any MCP server, over streamable HTTP or stdio, then browse its tools, prompts and resources and call them from generated forms. See the results rendered or as raw JSON-RPC. OAuth login is built in and works over SSH and in devcontainers.

mcptui is for people building or debugging MCP servers, or checking what a server exposes before wiring it into an agent. It's a protocol explorer, not an agent: there's no LLM in the loop. You make the calls and see exactly what goes over the wire. It's a single Go binary with no browser, proxy or Node runtime, and doesn't assume anything about the server.

Tools tab: a form generated from the tool's input schema, and the result.

```
mcptui ─ everything  dev  ─ mcp-servers/everything 2.0.0 (2025-11-25)                   ● connected
 1 Tools (13)   2 Prompts (4)   3 Resources (9)   4 Chat (3)   5 Log
/ filter                      │ get-sum Get Sum Tool  [read-only] [idempotent]
  echo ro                     │ ────────────────────────────────────────────────────────────────────
  get-annotated-message ro    │   Returns the sum of two numbers
  get-env ro                  │
  get-resource-links ro       │   a * 3
  get-resource-reference ro   │     First number
  get-structured-content ro   │   b * 4
▸ get-sum ro                  │     Second number
  get-tiny-image ro           │
  gzip-file-as-resource       │
  toggle-simulated-logging    │ [ Call ⏎ ] [ Edit JSON e ] [ Copy cmd c ] [ Docs d ]
  toggle-subscriber-updates   │━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
  trigger-long-running-operat…│ Result get-sum  ✓ 3ms   (p)retty (m)arkdown (r)aw  [ Copy y ] [ Sav…
  simulate-research-query     │ The sum of 3 and 4 is 7.
                              │
                              │
 p/m/r view  y copy  s save  c/C copy cmd  e json  tab list  esc form
```

Chat tab: every request, server notification and log message in the session, with a message box to send new calls (`get-sum a=3 b=4`, `read <uri>`) and a browsable list of everything the server offers.

```
mcptui ─ everything  dev  ─ mcp-servers/everything 2.0.0 (2025-11-25)                   ● connected
 1 Tools (13)   2 Prompts (4)   3 Resources (9)   4 Chat (5)   5 Log
                                        │ ▤ demo://resource/static/document/features.md resources/r…
                                        │ 2026-09-17 07:20:30 · 2ms · ok
                                        │ [ Rerun ctrl+r ] [ Edit e ] [ Copy cmd c ] [ curl C ]
                                        │──────────────────────────────────────────────────────────
  ● Connected to Everything Reference … │ Result demo://resource/static/document/features.md  ✓ 2ms…
    Everything Server – Server Instruct…│ ▤ demo://resource/static/document/features.md  text/markd…
    Audience: These instructions are wr…│    Everything Server - Features
                                        │
  ◂ notifications/tools/list_changed    │   Architecture /architecture.md | Project Structure
    list changed · refreshed            │   /structure.md | Startup Process /startup.md | Server
                                        │   Features | Extension Points /extension.md | How It
  ⚒ get-sum {"a":3,"b":4}               │   Works /how-it-works.md
    The sum of 3 and 4 is 7.            │
                                        │   ## Tools
  ⚒ echo {"message":"hello from mcptui… │
    Echo: hello from mcptui             │   •  echo  (tools/echo.ts): Echoes the provided
                                        │    message: string . Uses Zod to validate inputs.
▌ ▤ demo://resource/static/document/fe… │   •  get-annotated-message  (tools/get-annotated-
 ↑↓ move · tab/⏎ pick · esc back
 ⚒ get-sum                 Returns the sum of two numbers
 ⚒ get-structured-content  Returns structured content along with an output schema for client data v…
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
 › get-s
 ⏎ send  tab complete  ↑ previous  shift+tab transcript  esc tabs
```

## What it does

- **Connects to any MCP server:** streamable HTTP (JSON or SSE responses, sessions, transparent re-initialize when a session expires) or a local stdio command. Saved profiles hold URLs, commands, headers and auth settings; a picker lets you switch between them.
- **Handles authorization for you:** the full MCP OAuth 2.1 flow (protected resource and authorization server discovery, dynamic client registration, PKCE, token refresh with rotation, revocation), plus a paste-the-redirect-URL fallback for headless machines. Bearer tokens and custom headers work too.
- **Lets you browse everything the server exposes:** tools (with annotations such as read-only or destructive), prompts, resources and resource templates, with docs, schemas and filtering.
- **Makes calling things easy:** forms generated from JSON Schema, a JSON editor or `$EDITOR`, argument completion, live progress and cancellation. Destructive tools, and anything not read-only on a production profile, ask for confirmation first.
- **Shows results readably:** pretty JSON, rendered markdown, `structuredContent` checked against `outputSchema`, images and binary resources you can save, and the raw JSON-RPC for every exchange.
- **Keeps a session transcript:** the Chat tab records the connection, every call, server notifications and log messages. You can rerun or edit any entry, or copy it as a `curl` or `mcptui` command.
- **Works in scripts:** `mcptui call|prompt|read|ls|info` print JSON and share profiles and credentials with the TUI, and `mcptui auth token` feeds `curl`.

## Install

Prebuilt binaries for macOS, Linux and Windows are attached to each [release](https://github.com/ideonate/mcptui/releases). The commands below use the [GitHub CLI](https://cli.github.com) (`gh`), which fetches the latest release and also works while the repository is private.

### macOS

```sh
# Apple Silicon; use darwin_amd64 on an Intel Mac
gh release download -R ideonate/mcptui -p '*darwin_arm64.tar.gz' -O - | tar -xzf - mcptui

# /usr/local/bin is owned by root on macOS, so this needs sudo
sudo mkdir -p /usr/local/bin
sudo mv mcptui /usr/local/bin/
mcptui --version
```

No sudo? Put it in your home directory instead:

```sh
mkdir -p ~/.local/bin && mv mcptui ~/.local/bin/
echo 'export PATH="$HOME/.local/bin:$PATH"' >> ~/.zshrc && source ~/.zshrc
```

The binaries aren't notarized. Files fetched with `gh` or `curl` run as-is, but if you downloaded the archive in a browser and macOS refuses to open `mcptui`, clear the quarantine flag:

```sh
xattr -d com.apple.quarantine "$(command -v mcptui)"
```

### Ubuntu / Linux

```sh
# x86_64; use linux_arm64 on ARM machines
gh release download -R ideonate/mcptui -p '*linux_amd64.tar.gz' -O - | tar -xzf - mcptui
sudo install -m 0755 mcptui /usr/local/bin/mcptui
mcptui --version
```

Or without sudo: `install -D -m 0755 mcptui ~/.local/bin/mcptui` (`~/.local/bin` is on the PATH by default on Ubuntu; open a new shell if it didn't exist before).

### Windows

Download `mcptui_*_windows_amd64.zip` from the release page and put `mcptui.exe` somewhere on your `PATH`.

### Upgrading

Run the same commands again: `gh` downloads the latest release and the copy step replaces the old binary.

### From source

```sh
go install github.com/ideonate/mcptui@latest
```

(For a private repository this needs `GOPRIVATE=github.com/ideonate/*` and git credentials for GitHub.)

## Quick start

```sh
# A stdio server (temporary profile)
mcptui -- npx -y @modelcontextprotocol/server-everything

# A streamable HTTP server; logs in through the browser if the server returns 401
mcptui https://mcp.example.com/mcp

# Pick, create, edit or delete saved profiles in the TUI
mcptui

# Open a saved profile directly
mcptui example

# ...or save one from the command line
mcptui profiles add example https://mcp.example.com/mcp --env prod
```

A bare `mcptui` opens the profile picker (the default profile is preselected, so ⏎ connects). With no profiles yet, it goes straight to the new-profile form: a name, a URL or stdio command, and optional badge, auth, token, scope and client id. Press `ctrl+p` in the app to switch profiles, or to save a temporary `mcptui <url>` connection as a profile.

## Using it

The mouse works like in Textual apps: click tabs, list rows, form fields, `[ buttons ]` and the key hints in the footer; double-click a list item to open it (or a profile to connect); use the wheel to move through lists and scroll results. Hold shift (option on some macOS terminals) to select text, or run with `--no-mouse`.

Keyboard: `esc` steps back out (result → form → list → tab bar); in the tab bar `←`/`→` switch tabs and `↓`/`⏎` go back in. `tab` / `shift+tab` cycle focus (tabs → list → form fields → result), `⏎` runs, and `1`–`5` jump straight to a tab. Press `?` for everything.

| Where | Keys |
|---|---|
| Anywhere | `1`–`5` tabs · `ctrl+p` profiles · `R` refresh (or reconnect) · `L` / `O` log in / out · `P` ping · `i` server info & instructions · `?` help · `ctrl+c` cancel a running request, or quit |
| Lists | `j`/`k` move · `/` filter · `⏎` open form (runs it directly if it has no arguments) · `e` / `E` arguments as JSON / in `$EDITOR` · `d` full docs & schemas · `S` subscribe to resource |
| Form | `tab` / `shift+tab` fields · `⏎` or `ctrl+s` call · `space` `←` `→` toggle/cycle · `ctrl+n` / `ctrl+d` add/remove array item · `ctrl+t` multi-line · `ctrl+e` JSON · `ctrl+o` `$EDITOR` · `ctrl+space` complete · `esc` back |
| Result | `p` / `m` / `r` pretty / markdown / raw JSON-RPC · `y` copy (OSC 52) · `s` save to file · `c` / `C` copy as `mcptui` command / `curl` |
| Chat | type `add {"a":1}`, `add a=1`, `greet name=Ada`, `read <uri>` or `ping` and press `⏎` · `tab` completes names · `↑` recalls commands · `shift+tab` into the transcript, then `r` rerun, `e` edit, `a` show list/initialize requests too |
| Log | `t` show raw traffic · `G` follow · `c` clear |

**Chat** (tab 4) is a running transcript of the session: the connection (server info and instructions), every request including calls made from the other tabs, server notifications and log messages, requests the server sends to mcptui, disconnects and logins. Each entry has a short preview on the left and the full detail on the right. The message box runs along the bottom. With it empty, a **Tools / Prompts / Resources & templates / Ping** bar lets you browse everything the server offers (`↓` then `←`/`→`, or click); picking a category lists all of its items, and typing narrows the list. Or just type a command: a tool, prompt or resource name with JSON or `key=value` arguments. If something needs arguments you didn't give, its form opens on the right. Select any entry to see the full request and response on the right, and rerun or edit it from there.

**Forms** are generated from each tool's JSON Schema: strings, numbers, booleans, enums, arrays of scalars, and optional (`anyOf [X, null]`) fields. Anything more complex (nested objects, unions, `$ref`) becomes an inline JSON field, pre-filled with a skeleton. Arguments are validated before sending, and unset optional fields are left out.

**Safety:** tools annotated `destructiveHint: true` always ask for confirmation. In profiles with `env_badge = "prod"`, every tool not marked `readOnlyHint: true` asks too. Override this with `confirm = "never" | "writes" | "all"`.

## Configuration

`$XDG_CONFIG_HOME/mcptui/config.toml` (usually `~/.config/mcptui/config.toml`):

```toml
default_profile = "local"
# history = false              # don't write ~/.local/state/mcptui/history.jsonl

[profiles.local]
url = "http://127.0.0.1:8001/mcp"
# auth = "oauth"               # default for http; only used if the server returns 401
env_badge = "dev"              # dev | staging | prod: header colour and safety defaults

[profiles.some-remote]
url = "https://mcp.example.com/mcp"
client_id = "abc123"           # only when dynamic registration isn't available
# client_secret = "env:SECRET"
scope = "read write"           # optional
callback_port = 33418          # optional; 33418-33420 are registered
env_badge = "prod"
timeout = "2m"                 # optional request timeout; default none

[profiles.api-key]
url = "https://api.example.com/mcp"
auth = "bearer"
token = "env:MY_MCP_TOKEN"
headers = { "X-Org" = "env:MY_ORG" }

[profiles.local-stdio]
transport = "stdio"
command = ["python", "-m", "my_server"]
cwd = "/path/to/project"
env = { LOG_LEVEL = "debug" }
```

Values of the form `env:NAME` are read from the environment.

## Authorization

mcptui implements MCP authorization (OAuth 2.1) itself:

- **Discovery:** protected resource metadata (RFC 9728), then authorization server metadata (RFC 8414, with OpenID Connect fallbacks and tolerance for trailing slashes).
- **Client identity:** a pre-registered `client_id`, else a cached dynamic registration (RFC 7591, registered once per authorization server), else a new dynamic registration.
- **Flow:** PKCE (S256), `state`, and the RFC 8707 `resource` parameter.
- **Refresh:** proactive and on `401`, with refresh-token rotation saved atomically and a lock file so several mcptui processes can refresh safely.

The login URL is always shown: press `c` to copy it (native clipboard tool if available, plus OSC 52), `o` to open a browser, or `cat ~/.local/state/mcptui/login-url.txt` for an unbroken copy; in terminals with OSC 8 support the wrapped URL is also ctrl/cmd-clickable. If the browser runs on another machine (devcontainer, SSH) and can't reach the loopback callback, **paste the URL from the browser's address bar** into mcptui (press `p`, or just paste). Use `--no-browser` to skip opening a browser, and `--callback-host` to change the bind address.

Credentials are stored in `$XDG_CONFIG_HOME/mcptui/credentials.json` (mode 0600). Tokens are never logged.

```sh
mcptui auth login <profile>         # force a new login
mcptui auth status [profile]        # scope, expiry, authorization server, client id
mcptui auth token <profile>         # print a valid access token (refreshes if needed)
mcptui auth logout <profile>        # revoke (if supported) and delete
mcptui auth reset-client <profile>  # forget the dynamic registration and register again
```

For example: `curl -H "Authorization: Bearer $(mcptui auth token prod)" …`.

## Non-interactive use

These commands share the TUI's profiles and credentials, so they work in scripts and for coding agents:

```sh
mcptui call <profile> <tool> ['{json}' | @args.json | -]   # result JSON; exit 1 on isError
mcptui prompt <profile> <name> [key=value ...]
mcptui read <profile> <uri> [-o file]
mcptui ls <profile> tools|prompts|resources|templates [--json]
mcptui info <profile> [--json]
```

- `--raw` prints the whole JSON-RPC response.
- `--profile-url <url>` or `--stdio '<command>'` replaces the profile argument with a temporary connection.
- `-v` logs transport and auth events to stderr.
- When no valid token exists and stdin isn't a terminal, commands fail with a hint to run `mcptui auth login` rather than waiting for a browser.

## Protocol support

- **Transports:** streamable HTTP (JSON and SSE bodies, `Mcp-Session-Id`, `MCP-Protocol-Version`, transparent re-initialize after a `404`, `DELETE` on exit, and the optional GET stream) and stdio (stderr in the Log tab; a crash is reported with the last stderr lines).
- **Tools:** `tools/*`, including `structuredContent` validated against `outputSchema`, and annotations.
- **Prompts and resources:** `prompts/*`; `resources/*` including templates and subscribe.
- **Also supported:**
  - Automatic pagination.
  - Live progress and cancellation (`notifications/cancelled`).
  - `notifications/message` log messages.
  - Refresh on `list_changed` notifications.
  - Argument completion (`completion/complete`) and `ping`.
- **Server-to-client requests** (sampling, elicitation, roots) are logged and answered with a JSON-RPC error.

## Not yet implemented

- An OS keyring backend (`--keyring`).
- Saved argument presets.
- JSON folding and JSON path filtering in results.
- The device authorization grant and client ID metadata documents.
- The legacy HTTP+SSE transport.
- Answering sampling and elicitation requests.

See [SPEC.md](SPEC.md) for the design and later ideas.

## Development

```sh
go test -race ./...
go test ./internal/tui -update   # refresh TUI snapshots
```

`internal/testserver` is an in-process MCP server (HTTP and stdio) used as a test fixture. The OAuth tests run against an in-process mock authorization server.

## License

MIT, see [LICENSE](LICENSE).
