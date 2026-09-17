# CLAUDE.md

Guidance for working on mcptui, a terminal UI and CLI for exploring MCP servers. User-facing docs are in README.md; the original design spec is SPEC.md (still the reference for protocol and OAuth behaviour).

## Principles

- **Server-agnostic.** Never add behaviour, names or examples tied to a particular MCP server or private project. Test fixtures use `internal/testserver` or the public `@modelcontextprotocol/server-everything`.
- **Show the protocol honestly.** Keep every exchange's raw JSON-RPC available (`mcp.Exchange`). Don't hide errors; surface them in the UI and the Log tab.
- **Never log or display tokens.** Use `auth.Redact`. Credentials live only in `credentials.json` (0600).
- **Keyboard and mouse both.** Every action should have a key and, where it's visible, a clickable control. Keep the footer hints, the `?` help overlay (`internal/tui/overlays.go`) and README's key tables in sync when keys change.

## Commands

```sh
go build ./...                       # Go 1.27+
go vet ./...
go test -race ./...                  # CI runs this on Linux and macOS
go test ./internal/tui -update       # regenerate TUI golden snapshots, then review the diff
go build -o /tmp/mcptui . && /tmp/mcptui -- npx -y @modelcontextprotocol/server-everything
```

Releases: push a `vX.Y.Z` tag; `.github/workflows/release.yml` runs goreleaser (`.goreleaser.yaml`) for darwin/linux/windows × amd64/arm64. The version comes from `-X main.version`.

## Layout

| Package | What it is |
|---|---|
| `main.go`, `internal/cli` | cobra commands: root (TUI), `call/prompt/read/ls/info`, `auth …`, `profiles …` |
| `internal/mcp` | Our own MCP client (not the SDK): JSON-RPC over `Transport`; `HTTPTransport` (streamable HTTP, SSE, sessions, 404 re-init, DELETE on close, GET stream) and `StdioTransport`. `Client.Request` returns an `*Exchange` with raw request/response. Progress, cancellation, notifications and server→client requests are hooks in `ClientOptions`. |
| `internal/auth` | OAuth 2.1 per the MCP auth spec: discovery, dynamic registration cached per issuer, PKCE, loopback + paste fallback, refresh with rotation under a mutex + flock, revoke. `Manager` implements `mcp.Authorizer`. |
| `internal/session` | Builds transport + auth + client from a `config.Profile`. |
| `internal/config`, `internal/paths` | `config.toml` profiles (`env:` expansion, confirm rules), XDG paths (overridable with `MCPTUI_CONFIG_DIR` / `MCPTUI_STATE_DIR`). |
| `internal/schemaform` | JSON Schema → Bubble Tea form widget; validation (santhosh-tekuri/jsonschema); `Skeleton`. |
| `internal/uritemplate` | RFC 6570 levels 1–3. |
| `internal/tui` | The Bubble Tea app (see below). |
| `internal/history`, `internal/copycmd` | history.jsonl persistence; curl / `mcptui` command rendering. |
| `internal/testserver` | In-process MCP server (HTTP and stdio) used by tests. |

## TUI architecture (`internal/tui`)

- **Bubble Tea v2** (`charm.land/bubbletea/v2`, `bubbles/v2`, `lipgloss/v2`, `glamour/v2`). APIs differ from v1 (e.g. `View() tea.View`, `tea.KeyPressMsg`, `SetWidth` methods). When unsure, read the source in `$(go env GOMODCACHE)/charm.land/...` rather than guessing.
- **`App` is a pointer model.** Goroutines talk to it only through `bridge.send` (→ `Program.Send`, or a queue in tests). Async results carry a connection generation (`gen`) and stale ones are dropped.
- **Tabs and focus.** `tabTools/Prompts/Resources` share list + detail rendering (`detail.go`); `tabChat` is `chat.go`; `tabLog`. `focusArea` is `focusTabs` (tab bar; ←/→), `focusList`, `focusForm`, `focusResult`, `focusComposer` (Chat message box). `esc` steps outward, `tab`/`shift+tab` cycle. `typing()` decides whether letters are input or shortcuts.
- **Calls.** Everything goes through `invoke(tab, key, args)` → `startCall`, which registers a `resultState` in `a.runs[run]` and a `chatItem`. Item keys: `tool:<name>`, `prompt:<name>`, `res:<uri>`, `tmpl:<uriTemplate>`, `ping:`. `a.results[tab]` is what a tab's result pane shows.
- **Chat.** `chatState.items` holds calls plus non-call entries (`init`, `notify`, `server-request`, `event`, hidden `list`). Notifications never take the selection. The message box's `parseCommand` accepts `name {json}`, `name k=v` (typed from the schema), `call/get/read <x>`, `ping`.
- **Mouse.** Each render calls `resetZones`; renderers register `zone`s (rectangles with click/wheel handlers) and `addButton` labels that `resolveButtons` finds in the final screen text. Later registrations win, so overlays and buttons sit on top. Use `renderButton(label, key)` + `buttonLabel` so the text and the hit area match.
- **Overlays** implement the `overlay` interface; optional `buttons()`, `clickOutside()`, `wheel()`.
- **Profile picker** (`picker.go`) appears when started without a profile, or on `ctrl+p`.

## Testing conventions

- TUI tests drive `App` through a harness (`tui_test.go`: `newHarness`, `key`, `typeText`, `click`, `waitFor`) against `testserver` over httptest. Always render (`h.screen()`) before synthesising mouse events, as the real program does.
- Golden snapshots mask durations, times, ports and truncation points (`volatile` in `tui_test.go`); make new snapshots deterministic the same way.
- Auth tests use an in-process mock authorization server (`internal/auth/mock_test.go`) and cover every discovery path, registration caching, PKCE/state, paste fallback, rotation and concurrent refresh.
- Stdio tests re-exec the test binary as a server (`MCPTUI_TEST_STDIO=serve`).
- For manual checks, run the binary in tmux (`tmux new-session -d -x 100 -y 30 ...`, `tmux capture-pane -p`). Mouse clicks can be sent as SGR sequences: `tmux send-keys -l $'\e[<0;X;YM'` then `$'\e[<0;X;Ym'` (1-based).
