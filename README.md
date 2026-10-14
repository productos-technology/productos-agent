# ProductOS Agent

`productos-agent` is a small executable you install on your own machine. It signs in as you (OAuth 2.1, scope
`agent`), links your local git checkouts to the repositories your ProductOS workspaces connect, detects the coding
tools on the machine (`claude`, `codex`) and, when you press **Run** in the Deliver dialog of a pain, executes the
change definition in the linked checkout: edits, tests, a branch, a commit with the trailer, a pull request. The run
streams to a drawer in ProductOS, where you follow it and answer the tool's questions.

One static binary; Linux, macOS and Windows, amd64 and arm64.

## Install

```sh
brew install productos-technology/tap/productos-agent                     # macOS
curl -fsSL https://productos.technology/install.sh | sh        # Linux (and macOS)
winget install ProductOS.Agent                                 # Windows
```

Or download an archive from the [releases](https://github.com/productos-technology/productos-agent/releases) and put
`productos-agent` on your PATH. Checksums are in `checksums.txt`.

You also need `git`, one of [Claude Code](https://docs.anthropic.com/claude-code) (`claude`) or
[Codex](https://github.com/openai/codex) (`codex`) signed in on this machine, and optionally the GitHub CLI (`gh`)
to open pull requests.

## Use

```sh
productos-agent login                                # opens the browser; approve the agent; registers this machine
cd ~/code/mobile && productos-agent link             # the checkout's origin must be a repository your workspace connects
productos-agent run                                  # stay connected (foreground); Ctrl-C to stop
productos-agent service install                      # or keep it running in the background
```

Then open a pain in ProductOS, **Deliver**, pick this machine and the tool, **Run**.

| Command | What it does |
|---|---|
| `login [server] [--name NAME] [--no-browser]` | Signs in to productos.technology, or to the self-hosted ProductOS given as `server`. OAuth sign-in with PKCE on a loopback port, dynamic client registration, tokens in the OS keychain (fallback: a 0600 file under the config dir, with a warning); registers the device. |
| `logout` | Revokes the device (cancels its runs, drops its links), revokes the tokens, forgets the credentials. |
| `status` | Server, login, tools, daemon state, device and linked checkouts (reads the device through a heartbeat). |
| `link [path] [--branch B]` | Validates the git checkout, reads `origin` and the default branch, registers it. An unknown remote lists what ProductOS expected. |
| `unlink [path]` | Drops the link (a run in that checkout is cancelled). |
| `run [--once]` | The daemon: register/refresh the device, connect the channel, heartbeat every 30 s, execute runs. `--once` exits after the first run. |
| `service install\|uninstall\|start\|stop` | systemd user unit (Linux), launchd agent (macOS), Task Scheduler logon task (Windows). The service starts with none of your shell's environment, so `install` copies PATH and where the tools keep their sign-in (`CLAUDE_CONFIG_DIR`, `CODEX_HOME`, the XDG directories) from the shell that runs it; run it again after changing them. |
| `doctor` | Checks `claude`, `codex`, `git`, `gh`, the keychain, the login, the device and the server. |
| `version` | Version, commit and date. |

`--verbose` on any command also logs to stderr. The structured log is `agent.log` in the state directory
(`~/.local/state/productos-agent` on Linux, `~/Library/Application Support/productos-agent` on macOS,
`%LocalAppData%\productos-agent` on Windows). Tokens and change definitions are never logged.

## What a run does

1. `run.start` arrives on the channel; the agent answers `starting` within 15 s.
2. The checkout must be clean (else the run fails with a clear reason). `git fetch origin <default_branch>`, then a
   branch named by ProductOS (`productos/mob-p7-offline-drafts-are-lost`, suffixed `-2`, `-3` if it exists) is created
   from `origin/<default_branch>`. Status `running`.
3. The tool runs unattended with the change definition as its prompt:
   - Claude Code: `claude -p --output-format stream-json --verbose --permission-mode acceptEdits --allowedTools …`
   - Codex: `codex exec --json --full-auto -`
   Its stream becomes drawer events: `message`, `tool_use` (with output and duration), `file_change` (from
   `git diff --numstat` against the base commit), `test` (commands recognised as test runners, with pass/fail), `log`.
   Events go out in batches (50 or 500 ms) with an acknowledged outbox; nothing acknowledged is ever resent.
4. A question (Claude Code's `AskUserQuestion`, or a final message that ends with a question and no changes) becomes a
   `question` event; the run waits for your answer in the drawer and resumes the tool's session with it
   (`claude --resume <session>` / `codex exec resume <thread>`).
5. When the tool is done, uncommitted work is committed as `MOB-P7: <title>` with the trailer `ProductOS: MOB-P7`, the
   branch is pushed (`"push": false` in `config.json` disables it), and with `gh` on PATH a pull request is opened.
   Status `succeeded` with the branch, head commit, PR URL and a Markdown summary; any error ends in `failed` with a
   reason; `run.cancel` kills the tool's process group and ends in `cancelled`. Runs time out after 2 hours.

## Configuration

`config.json` in the config directory (`~/.config/productos-agent` on Linux, `~/Library/Application Support/productos-agent`
on macOS, `%AppData%\productos-agent` on Windows) is written by `login`; you may add:

```json
{
  "push": true,
  "claude_permission_mode": "acceptEdits",
  "claude_args": ["--model", "opus"],
  "codex_args": ["--model", "o3"]
}
```

Environment: `PRODUCTOS_AGENT_CONFIG_DIR`, `PRODUCTOS_AGENT_STATE_DIR`, `PRODUCTOS_AGENT_NO_KEYRING=1` (skip the
keychain), `PRODUCTOS_AGENT_CLAUDE_BIN` / `PRODUCTOS_AGENT_CODEX_BIN` (tool binaries).

## Security

- The machine is bound to you: the device belongs to the account that signed in; only you can start runs on it.
  Revoking it in ProductOS (Account, Devices) or `logout` cancels its runs and unlinks its checkouts.
- Security lives on repositories: a run happens only in a checkout you linked, and a checkout links only when its
  `origin` matches a GitHub repository connected to one of your workspaces. The server enforces this.
- The agent never receives cookies, CSRF tokens or anyone's secrets; the change definition names no customer.
- Access tokens live one hour and are refreshed with rotation; refresh failures that cannot be fixed sign you out.

## Protocol

The wire contract is `docs/reference/agent-protocol.md` in the ProductOS repository (`protocol_version: 1`): REST under
`/api/v1/agent`, the `AgentChannel` over Action Cable (`/cable?token=…`), events with server-assigned `seq`, and the
`queued → starting → running ⇄ waiting_input → succeeded | failed | cancelled` lifecycle.

## Development

```sh
go build ./...  && go vet ./... && gofmt -l . && go test ./...
go test -race ./internal/daemon/           # the integration test against a fake server
GOOS=darwin GOARCH=arm64 go build .        # cross-compile
goreleaser release --snapshot --clean      # local release dry run
```

Dependencies: `github.com/coder/websocket` (the maintained `nhooyr.io/websocket`; context-aware, dependency-free),
`github.com/spf13/cobra`, `github.com/zalando/go-keyring`. Everything else is the standard library.

Layout: `internal/cli` (commands), `internal/daemon` (the socket loop, heartbeats, dispatch, the Reporter with REST
fallback), `internal/runner` (one run: checkout, tool, events, question/answer, commit/push/PR), `internal/stream/{claude,codex}`
(stream parsers; fixtures in `testdata/`), `internal/cable` (Action Cable client), `internal/api` (REST),
`internal/oauth` (discovery, registration, PKCE, loopback, refresh), `internal/secrets`, `internal/config`,
`internal/paths`, `internal/gitx`, `internal/tools`, `internal/service`, `internal/logx`, `internal/protocol`.

## Release

Tag and push: `git tag v0.1.0 && git push origin v0.1.0`. The `release` workflow runs goreleaser: archives per OS/arch,
`checksums.txt`, the GitHub release, the Homebrew formula in `productos-technology/homebrew-tap`, and a winget pull request.
Secrets: `HOMEBREW_TAP_GITHUB_TOKEN`, `WINGET_GITHUB_TOKEN`. `install.sh` here is the real installer the server
serves at `/install.sh`.

## License

MIT.
