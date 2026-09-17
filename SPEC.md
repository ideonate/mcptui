# mcptui — an interactive terminal client for MCP servers

Status: v1 implemented (see README.md for what is and isn't done). Written 2026-09-16.

A general-purpose tool for exploring and exercising any MCP server by hand from a terminal. It must not depend on, or assume, any particular server.

## 1. Why

- **`npx @modelcontextprotocol/inspector`**: a browser UI plus a local proxy process. Awkward in devcontainers and over SSH: extra ports, proxy auth tokens, and a browser pointed at a forwarded port.
- **`f/mcptools` (`mcpt`)**: a single binary, but one command per call. You have to paste a bearer token into every command, the flag parsing is inconsistent, it has no OAuth, and the released build lacks streamable HTTP.
- **Agent clients (Claude Code, Claude Desktop)**: good for agent use, but they hide the raw protocol.

`mcptui` is a single binary with an interactive TUI. It handles OAuth itself, remembers sessions per server, and lets you browse and call everything a server exposes.

### Goals

1. One command to connect: `mcptui <profile-or-url>` opens the TUI, logs in through the browser if the server needs it, and shows the server.
2. Browse tools, prompts, resources and resource templates, with readable docs.
3. Call anything from a form generated from its schema, or from a raw JSON editor.
4. Show results readably: pretty JSON, rendered markdown, and the raw JSON-RPC.
5. Built-in MCP authorization (OAuth 2.1: discovery, dynamic registration, PKCE, refresh, revoke) that works in devcontainers and over SSH.
6. Saved profiles for multiple servers and environments, over streamable HTTP or stdio.
7. A history of calls you can re-run, plus a non-interactive mode for scripts.

### Non-goals (v1)

- Being an LLM agent. There's no model in the loop; it's a protocol explorer.
- Answering server→client requests (sampling, elicitation, roots). Show them in the log and reply with a JSON-RPC error. Could be added later.
- The legacy HTTP+SSE transport (optional later).
- Server-specific UI. Anything like that should come later as a generic extension point (section 9), not be built in.

## 2. Language / stack

Either works. Pick one.

| | Go (recommended) | Python |
|---|---|---|
| TUI | Bubble Tea + Bubbles + Lip Gloss; Glamour for markdown | Textual (markdown, tree, and data-table widgets built in) |
| MCP client | `github.com/modelcontextprotocol/go-sdk` (official), or `mark3labs/mcp-go` | `mcp` (official SDK) |
| Distribution | Single static binary: `go install`, or a release asset | `uv tool install` / `pipx`; needs a Python runtime |

**Recommendation: Go.** The point of the tool is "a binary we don't have to think about".

Whatever the language, **implement the OAuth flow ourselves** (section 4) instead of relying on an SDK's auth helper. The flow is small, and we need full control over the headless and devcontainer fallback, token storage, and compatibility with imperfect servers. The SDK transport only needs to accept a custom HTTP client or a per-request header hook.

The code should be self-contained, with no dependencies on any particular server or application.

## 3. Protocol support

**Transports**
- **Streamable HTTP**, the primary transport. Send `Accept: application/json, text/event-stream`. Handle both JSON and SSE response bodies. Track `Mcp-Session-Id` when the server issues one, and send `MCP-Protocol-Version` after initialize. On a `404` for an expired session, re-initialize transparently. Send `DELETE` to end the session on quit when a session id exists.
- **Stdio**: launch a command with a configurable working directory and environment. Show stderr in a log pane, and surface a crash together with the last stderr lines.
- Show both the negotiated protocol version and the server's `serverInfo` (name, version) from initialize.

**Features**
- `tools/list`, `tools/call` (including `structuredContent`, `outputSchema`, and annotations).
- `prompts/list`, `prompts/get`.
- `resources/list`, `resources/templates/list`, `resources/read`; `resources/subscribe` if the server advertises it.
- Pagination (`cursor`/`nextCursor`) on every list call. Fetch all pages.
- Notifications: `notifications/progress` and `notifications/message` shown live; `*/list_changed` triggers a refresh of that list when it arrives.
- **Stateless servers are common.** Many never send `list_changed` and have no stream, so a manual refresh (`R`) must always be available.
- `completion/complete` for prompt and resource-template arguments, if the server advertises completions (nice to have).
- Cancellation (`notifications/cancelled`) for in-flight requests.
- `ping`.

## 4. Authorization (OAuth 2.1, per the MCP authorization spec)

### 4.1 Login flow

1. Connect with any stored access token. On `401`, or with no token, try a refresh (4.4). If refresh fails, start a login.
2. **Protected resource metadata (RFC 9728).** Use `resource_metadata` from the `401`'s `WWW-Authenticate` header if present. Otherwise try `<origin>/.well-known/oauth-protected-resource<path>`, then `<origin>/.well-known/oauth-protected-resource`.
   - Use the metadata's `resource` as the RFC 8707 `resource` parameter. It may differ from the URL you connected to (a loopback port versus a public hostname behind a proxy); that's allowed.
   - Take the authorization server from `authorization_servers[0]`. If there are several, let the user choose.
3. **Authorization server metadata.** Try RFC 8414 path-insertion (`<origin>/.well-known/oauth-authorization-server<path>`), then root, then OpenID Connect discovery (`/.well-known/openid-configuration`, both variants).
   - **Normalize trailing slashes** when comparing issuer URLs; servers are inconsistent about them.
   - If no protected resource metadata is found at all (older servers), fall back to the auth server URL set in the profile, else to the MCP server's origin.
4. **Client identity**, in order:
   - a `client_id` set in the profile (pre-registered), optionally with a `client_secret` for `client_secret_post`;
   - a cached dynamic registration for this authorization server;
   - dynamic client registration (RFC 7591) if `registration_endpoint` exists (4.3);
   - otherwise stop with a clear message: "this server needs a pre-registered client_id; set `client_id` in the profile".
   - (Later: client ID metadata documents, when servers support them.)
5. Generate a PKCE verifier and an S256 challenge, plus a random `state`. If the metadata doesn't list S256 in `code_challenge_methods_supported`, warn but continue.
6. Start the loopback callback listener (4.3).
7. Build the authorize URL: `response_type=code`, `client_id`, `redirect_uri`, `code_challenge`, `code_challenge_method=S256`, `state`, `resource`, plus `scope` if known. Scope comes from the profile, else `scopes_supported` in the protected resource metadata, else the `WWW-Authenticate` `scope`, else it's omitted. **Some servers let the user pick scopes on the consent page, so never require a scope.**
8. **Always show the URL in the TUI**, with a copy key (OSC 52 clipboard). Also try to open it: `$BROWSER`, then `xdg-open`/`open`/`start`, then `code --openExternal` in VS Code terminals. Failing to open a browser is not an error.
9. Wait for whichever comes first:
   - the loopback callback. Reply with a small "You can close this tab" page, or the error if `error=` came back.
   - the user pastes the redirect URL, or just the code, into the TUI (4.2).
   - cancel or timeout. The timeout message should mention the common causes: redirect URI not registered, or callback port not forwarded.
10. Check `state`, then exchange the code at the token endpoint (form-encoded: `grant_type=authorization_code`, `client_id`, `code`, `redirect_uri`, `code_verifier`, `resource`).
11. Store the tokens (4.5), retry the MCP request, and show the granted `scope` from the token response.

### 4.2 Headless, devcontainer and SSH

If the browser runs on another machine than the TUI, the loopback redirect only arrives when the port is forwarded. So:

- The **paste fallback is required.** The login screen always says: "Browser can't reach the callback? Paste the URL from the address bar here." Parse `code` and `state` (and `error`) from the pasted URL.
- `--no-browser` skips opening a browser.
- `--callback-host` sets the bind address (default `127.0.0.1`). The registered redirect URI stays loopback.
- Don't use the device authorization grant in v1, because few MCP servers support it. Worth adding later if `device_authorization_endpoint` is advertised.

### 4.3 Redirect URIs, ports and registration

- **Assume exact redirect URI matching.** Many servers don't accept arbitrary loopback ports as RFC 8252 suggests. So use a **fixed default port** (for example `33418`, configurable per profile), and register `redirect_uris` with that port plus two alternates (`33419`, `33420`) in one registration. At login, use the first free port.
- Path: `/callback`. Host: `127.0.0.1`. Add `http://localhost:<port>/callback` variants to the registration too, for servers that only accept `localhost`.
- Registration body: `client_name` = `mcptui (<user>@<hostname>)`, `redirect_uris`, `grant_types: ["authorization_code", "refresh_token"]`, `response_types: ["code"]`, `token_endpoint_auth_method: "none"`.
- **Register once and cache per authorization server issuer.** Registration endpoints are often rate-limited; never register on every login. Store any returned `client_secret` and use it.
- If the server rejects the cached client (`invalid_client`, or an unregistered redirect URI), `mcptui auth reset-client <profile>` drops the cache and registers again. Also offer this interactively after such a failure.

### 4.4 Refresh

- Refresh proactively when fewer than 5 minutes remain (from `expires_in`), and reactively once on any `401`.
- **Assume refresh token rotation.** Persist the new token pair atomically (temp file + rename) before retrying the original request. If the response has no new refresh token, keep the old one.
- Only one refresh at a time within the process (mutex). Across processes, use a lock file: take an exclusive lock, re-read the store (another instance may have refreshed already), refresh only if the token is still stale, write, release.
- If refresh fails with `invalid_grant`, do a full login.
- Pass the `resource` parameter on refresh too.

### 4.5 Token storage

- Default: `$XDG_CONFIG_HOME/mcptui/credentials.json`, mode `0600`, directory `0700`.
- Keyed by protected resource `resource` URL (falling back to the MCP URL). Store: `client_id`, `client_secret?`, `access_token`, `refresh_token`, `expires_at`, `scope`, `authorization_server`.
- Optional OS keyring backend (`--keyring`). Not the default, because containers usually have none.
- Never log tokens. Redact `Authorization` headers in raw views and history down to the first 6 characters.

### 4.6 Commands

- `mcptui auth login <profile>`: forces a new login.
- `mcptui auth logout <profile>`: revokes at `revocation_endpoint` if one exists (refresh token then access token), then deletes locally.
- `mcptui auth status [profile]`: shows scope, expiry, authorization server and client id.
- `mcptui auth token <profile>`: prints a valid access token, refreshing if needed. Handy for `curl`.
- `mcptui auth reset-client <profile>`
- In the TUI, `L` logs in again and `O` logs out.

### 4.7 Other auth modes

- `auth = "bearer"` with `token = "..."` or `token = "env:VAR"`.
- `headers = { ... }` for arbitrary extra headers (API keys and similar), also supporting `env:` values.
- `auth = "none"`.

## 5. Profiles and config

`$XDG_CONFIG_HOME/mcptui/config.toml`:

```toml
default_profile = "local"

[profiles.local]
url = "http://127.0.0.1:8001/mcp"
# auth = "oauth" (default for http: used only if the server returns 401) | "bearer" | "none"
env_badge = "dev"            # dev | staging | prod: header colour and safety defaults

[profiles.some-remote]
url = "https://mcp.example.com/mcp"
client_id = "abc123"         # only when dynamic registration isn't available
scope = "read write"         # optional
callback_port = 33418        # optional
env_badge = "prod"

[profiles.local-stdio]
transport = "stdio"
command = ["python", "-m", "my_server"]
cwd = "/path/to/project"
env = { LOG_LEVEL = "debug" }
```

CLI:
- `mcptui [profile]`
- `mcptui https://host/mcp` (a temporary profile)
- `mcptui -- <command...>` (temporary stdio)
- `mcptui profiles` lists profiles; `mcptui profiles add` saves the current temporary connection.

## 6. TUI

### 6.1 Layout

```
┌ mcptui ─ local [dev] ─ my-server 1.4.0 (2025-06-18) ─ scope: read write ─ token 52m ─ ● connected ┐
│ [Tools] Prompts  Resources  History  Log                                                         │
├──────────────────────────────┬───────────────────────────────────────────────────────────────────┤
│ / filter                     │ create_item                               ⚠ destructive           │
│   get_item                   │ ─────────────────────────────────────────────────────────────── │
│ ▸ create_item                │ Create a new item. …                                              │
│   delete_item                │                                                                   │
│   search                     │ name *   [ Widget                                     ]           │
│                              │ tags     [ + add ]                                                │
│                              │                                                                   │
│                              │ [ Call ⏎ ]  [ Edit JSON e ]  [ Copy as command y ]                │
│                              │ ─────────────────────────────────────────────────────────────── │
│                              │ Result ✓ 184ms    (p)retty (m)arkdown (r)aw                       │
│                              │ { "id": 42, "name": "Widget" }                                    │
└──────────────────────────────┴───────────────────────────────────────────────────────────────────┘
 ? help  tab pane  / filter  ⏎ call  e json  R refresh  L login  q quit
```

- Header: profile plus environment badge (dev green, staging amber, prod red), server name, version and protocol version, granted scope, token expiry countdown, and connection state.
- Keyboard first; mouse is nice to have. Must work at 100×30 and stack the panes at narrow widths.
- Show the server's `instructions` (from initialize) in a panel you can open (`i`).

### 6.2 Tabs

- **Tools**: list with a filter over names and descriptions. Annotations shown as badges (read-only, destructive, idempotent, open-world). Detail shows the description as markdown, the input schema, and the output schema.
- **Prompts**: form from the arguments. "Get" renders the messages as a transcript (role label plus content rendered by type). Copy prompt text.
- **Resources**: resources and templates. "Read" renders by MIME type: markdown rendered, JSON pretty-printed, text as is, binary offered as a save-to-file. Templates get a form built from their URI variables (with completions if supported). Subscribe/unsubscribe if supported.
- **History**: every request this session; optionally persisted to `$XDG_STATE_HOME/mcptui/history.jsonl`, redacted. Columns: time, profile, kind, name, status, duration. Actions: view, re-run (reopens the form with the same arguments), copy arguments, copy as command.
- **Log**: server log notifications, stdio stderr, transport and auth events (discovery steps, refreshes). This is essential for debugging auth.

### 6.3 Forms from JSON Schema

- Supported types: `string` (multi-line toggle), `integer`/`number`, `boolean`, `enum`, arrays of scalars, `anyOf [X, null]` (optional X), defaults, descriptions, required markers.
- Anything more complex (nested objects, real `oneOf`/`anyOf` unions, arrays of objects, `$ref`) opens that field in a JSON editor, pre-filled with a skeleton built from the schema.
- Validate against the schema before sending, and show errors inline. Leave unset optional fields out of the arguments.
- `e` opens the whole argument object as JSON in a built-in editor; `E` opens it in `$EDITOR`. The form and JSON stay in sync where they can.
- Remember the last arguments per tool for the session. Optionally save named argument presets per profile.

### 6.4 Results

- Show `isError` clearly. Render each content block by type: text (JSON pretty-printed with folding, or markdown; toggle with `m`), image (metadata plus save), audio (metadata plus save), resource links, and embedded resources. Show `structuredContent` and validate it against `outputSchema` when one exists.
- Views: pretty, markdown, raw (the exact JSON-RPC request and response, redacted).
- Actions: copy (`y`), save to file (`s`), JSON path filter over results (`.`; nice to have).
- Show a progress bar and messages for requests that report progress. `ctrl+c` cancels an in-flight call. No timeout by default (configurable).

### 6.5 Copy as command

For any request: `curl` against the HTTP endpoint (with `Authorization: Bearer $(mcptui auth token <profile>)` and the right `Accept` header), or the equivalent `mcptui call …` (section 7).

### 6.6 Safety

- In profiles with `env_badge = "prod"`, confirm before any tool not annotated `readOnlyHint: true`.
- Always confirm tools annotated `destructiveHint: true`, in any profile.
- Profile option `confirm = "never" | "writes" | "all"` overrides this.

## 7. Non-interactive mode

These use the same profiles and credential store, so they work in scripts and for coding agents:

```
mcptui call <profile> <tool> ['{json args}' | @file.json]   # result JSON to stdout; exit 1 on isError
mcptui prompt <profile> <name> [key=value ...]              # rendered messages
mcptui read <profile> <uri>
mcptui ls <profile> tools|prompts|resources|templates [--json]
mcptui info <profile>                                       # serverInfo, capabilities, instructions
```

Add `--raw` for the JSON-RPC result, `--profile-url <url>` for a temporary connection, and the non-interactive login rule: if the command has no valid token and stdin isn't a terminal, fail with "run `mcptui auth login <profile>`" rather than blocking.

## 8. Testing and acceptance

**Automated**
- OAuth against an in-process mock authorization server and resource server, covering:
  - every discovery fallback path, including trailing-slash issuer mismatch;
  - registering once and then using the cache;
  - a pre-registered `client_id`;
  - PKCE check and `state` mismatch rejected;
  - the paste fallback;
  - rotation persisted before retry;
  - two processes refreshing at once (lock file);
  - `invalid_grant` leading to a new login;
  - revoke on logout.
- Transport: session id handling and re-initializing after `404`, SSE and JSON response bodies, pagination, and cancellation. Use the official SDK's example servers (Python `mcp` "everything"-style servers, or `@modelcontextprotocol/server-everything`) as stdio and HTTP fixtures.
- JSON Schema to form to arguments round-trip for each supported type.
- Snapshot tests of the main views with the framework's test harness (Bubble Tea `teatest` / Textual `Pilot`).

**Manual acceptance**
1. `mcptui -- npx -y @modelcontextprotocol/server-everything` lists tools, prompts and resources. Calls work, including a progress-reporting tool with a live progress bar and cancellation.
2. Against an OAuth-protected streamable HTTP server with dynamic registration: first run opens consent, then calls work. A second run reuses the stored tokens with no browser.
3. Force expiry by editing `expires_at`: the next call refreshes silently, and the store holds the rotated refresh token.
4. Two instances running at once both survive a refresh.
5. With the callback port unreachable, the paste-URL fallback completes the login.
6. A server without dynamic registration fails with the pre-registered `client_id` message, then works once `client_id` is set.
7. `mcptui call <profile> <tool> '{…}'` prints JSON. `mcptui auth token <profile>` prints a token usable with `curl`.
8. A prod-badged profile asks for confirmation before non-read-only tools.

## 9. Later / extension ideas (not v1)

- **Discovery adapters.** Some servers hide most operations behind meta-tools (for example a "list operations in category" tool plus a "call operation by name" proxy tool). A per-profile adapter config could map these into a virtual tool tree: which tool lists groups, which lists operations, how to parse the result, and which proxy tool to call. It should be declarative config, not code.
- **Identity panel.** A per-profile setting naming a tool to call after connecting (for example `whoami`) whose result is summarized in the header.
- Device authorization grant, client ID metadata documents, and the legacy SSE transport.
- Answering sampling and elicitation requests (manual text entry by the user).
- Recording and replaying sessions as test fixtures.

## 10. Milestones

1. **Connect and call**: profiles, streamable HTTP and stdio, bearer/header auth, Tools tab, JSON-editor calls, results, raw view, Log tab.
2. **OAuth**: the full section 4 flow, including the paste fallback, rotation with locking, and the `auth` subcommands.
3. **Forms, Prompts, Resources**: schema forms, prompt and resource tabs, pagination, notifications, cancellation.
4. **History and polish**: history and re-run, copy-as-command, safety confirmations, instructions panel.
5. **Non-interactive subcommands** and distribution (release binaries or `uv tool install`), plus a README.
