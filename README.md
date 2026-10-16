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
`productos-agent` on your PATH. Checksums are in `checksums.txt`, signed as `checksums.txt.sig` (Ed25519; the public key is compiled into the agent and pasted into `install.sh`, which verifies it with openssl).

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
| `status` | Server, version and install method, login, tools, daemon state, device, linked checkouts and whether an update is available (reads the device through a heartbeat). |
| `link [path] [--branch B]` | Validates the git checkout, reads `origin` and the default branch, registers it. An unknown remote lists what ProductOS expected. |
| `unlink [path]` | Drops the link (a run in that checkout is cancelled). |
| `run [--once]` | The daemon: register/refresh the device, connect the channel, heartbeat every 30 s, execute runs. `--once` exits after the first run. |
| `service install\|uninstall\|start\|stop` | systemd user unit (Linux), launchd agent (macOS), Task Scheduler logon task (Windows). The service starts with none of your shell's environment, so `install` copies PATH and where the tools keep their sign-in (`CLAUDE_CONFIG_DIR`, `CODEX_HOME`, the XDG directories) from the shell that runs it; run it again after changing them. A Homebrew install gets Homebrew's stable path (`/opt/homebrew/opt/productos-agent/bin/productos-agent`), so `brew upgrade` does not break the service. |
| `update [--check] [--to vX.Y.Z]` | Updates the agent to the latest release (from the running agent or the server, else GitHub's latest-release redirect), or to the version given (to roll back or pin). Downloads the archive and `checksums.txt` over HTTPS, checks the SHA-256, tries the new binary (`version`), swaps it in atomically and restarts the background service when it runs that binary. Homebrew and winget installs print `brew upgrade productos-agent` / `winget upgrade ProductOS.Agent` instead. |
| `doctor` | Checks `claude`, `codex`, `git`, `gh`, the keychain, the login, the device, the server, the install method and whether an update is available. |
| `version` | Version, commit and date. |

`--verbose` on any command also logs to stderr. The structured log is `agent.log` in the state directory
(`~/.local/state/productos-agent` on Linux, `~/Library/Application Support/productos-agent` on macOS,
`%LocalAppData%\productos-agent` on Windows). Tokens and change definitions are never logged.

## Seeding a repository

A product started from scratch in ProductOS can be planted on this machine (`seed.plant`). The agent creates the
directory (it must not exist, or be empty), writes the seed's files or unpacks its zip safely (no absolute or `..`
paths, no symlinks, at most 300 entries of 2 MB), runs `git init -b main`, makes one commit that ends with the
`ProductOS:` trailer and reports the path and the commit. When the dialog asks for a GitHub repository, it also runs
`gh repo create … --push`; that needs `gh` installed **and signed in** (`gh auth login`), which the agent announces as
`tools.gh`. If GitHub refuses, the checkout stays and ProductOS says why. A seed never blocks a run and never deletes
anything you had.

## What a run does

1. `run.start` arrives on the channel; the agent answers `starting` within 15 s.
2. The checkout must be clean (else the run fails with a clear reason). `git fetch origin <default_branch>`, then a
   branch named by ProductOS (`productos/mob-p7-offline-drafts-are-lost`, suffixed `-2`, `-3` if it exists) is created
   from `origin/<default_branch>`. Status `running`.
3. The tool runs unattended with the change definition as its prompt. The prompt asks it to check first whether the
   change is already in place (the commits on `origin/<default_branch>` that carry the trailer are listed), never to
   make cosmetic changes just to have something to commit, to put what it did not do in `left_out`, and to end with a
   structured report in the language of the change definition:
   - Claude Code: `claude -p --output-format stream-json --verbose --permission-mode acceptEdits --allowedTools … --json-schema <report schema>`
     (the report comes back as the result's `structured_output`)
   - Codex: `codex exec --json --sandbox workspace-write --skip-git-repo-check --output-schema <file> -` (the final message is the report)
   Both run with `CI=1`, `CLAUDE_CODE_DISABLE_AUTO_MEMORY=1`, `DISABLE_AUTOUPDATER=1`, `GIT_TERMINAL_PROMPT=0`,
   `GH_PROMPT_DISABLED=1` and `NO_COLOR=1`. Its stream becomes drawer events: `message`, `tool_use` (with output and duration), `file_change` (from
   `git diff --numstat` against the base commit), `test` (commands recognised as test runners, with pass/fail), `log`.
   Events go out in batches (50 or 500 ms) with an acknowledged outbox; nothing acknowledged is ever resent.
4. A question (Claude Code's `AskUserQuestion`, a report with claim `needs_input` and a `question`, or a final message
   that ends with a question when there is no report) becomes a `question` event; the run waits for your answer in the drawer and resumes the tool's session with it
   (`claude --resume <session>` / `codex exec resume <thread>`).
5. When the tool is done, uncommitted work is committed as `MOB-P7: <title>` with the trailer `ProductOS: MOB-P7`, and
   git decides the outcome, the tool's words only explain it:
   - new commits: `succeeded` with outcome `changed`, the branch is pushed (`"push": false` in `config.json` disables
     it) and with `gh` on PATH a pull request is opened. A push or PR failure never fails the run: it becomes a
     report note (`push_failed`, `push_disabled`, `gh_missing`, `pr_failed`, `pr_exists` with the existing URL);
   - nothing changed and the tool claimed `already_done` with evidence (file:line, a commit, a PR): `succeeded` with
     outcome `no_change` (ProductOS shows it in pencil until someone confirms it);
   - nothing changed otherwise: `failed` with `reason_code` `no_changes` (or `blocked` when the tool said it could not).
   Every final status carries the tool's whole final prose as `summary` and a `report`: the tool's answer (claim,
   headline, customer change, changes, verification, left out, evidence) plus what the agent knows (files, tests,
   prior commits, notes such as `permission_denied` and `tests_failing`, tool and agent versions).
6. A failure has a `reason_code` (`checkout_dirty`, `fetch_failed`, `tool_auth`, `tool_limit`, `tool_crashed`,
   `timeout`, …) and a one-line `reason`. When the tool had edited files and the run fails or is cancelled, the work is
   committed on the run's branch as `WIP: MOB-P7: <title>` (hooks skipped; stashed if even that fails), pushed when
   push is on, and the branch and commit go out with the final status: nothing is lost and the checkout is left clean.
   `run.cancel` kills the tool's process group and ends in `cancelled`. Runs time out after 110 minutes (under the
   server's 2-hour stuck sweep).
7. Against a server from before outcomes (its `hello` has no `features`), a `no_change` is sent as the old failure
   ("the tool made no changes").

## Configuration

`config.json` in the config directory (`~/.config/productos-agent` on Linux, `~/Library/Application Support/productos-agent`
on macOS, `%AppData%\productos-agent` on Windows) is written by `login`; you may add:

```json
{
  "push": true,
  "auto_update": true,
  "claude_permission_mode": "acceptEdits",
  "codex_sandbox": "workspace-write",
  "claude_args": ["--model", "opus"],
  "codex_args": ["--model", "o3"]
}
```

Environment: `PRODUCTOS_AGENT_CONFIG_DIR`, `PRODUCTOS_AGENT_STATE_DIR`, `PRODUCTOS_AGENT_NO_KEYRING=1` (skip the
keychain), `PRODUCTOS_AGENT_CLAUDE_BIN` / `PRODUCTOS_AGENT_CODEX_BIN` (tool binaries), `PRODUCTOS_AGENT_NO_UPDATE=1`
(no automatic updates).

## Updates

ProductOS is the authority on versions: its `hello` and every `run.start` carry the latest release, the minimum
version, where archives live and whether automatic updates are on (the server's kill switch). The agent never calls
GitHub's API.

- **Before a run**, once the run is acknowledged and before the checkout is touched, an agent that is behind updates
  itself when it installed itself (not Homebrew, not winget), `auto_update` is on and no other run is in progress: a
  log line in the drawer, `checksums.txt` and its Ed25519 signature from the release (verified with the key compiled into
  the agent), the archive, the SHA-256 checked, the new binary tried
  (`version`), swapped in atomically, and the process re-executes itself (same PID on Linux and macOS, so systemd and
  launchd do not notice; on Windows a new process replaces it). The runs it had acknowledged are handed over in
  `handoff.json` (0600, state directory) and continue from the checkout step. A failed update is a warning and an
  `update_failed` note: the run goes on with the current version. An update never happens in the middle of a run.
- **When idle** (on `hello`, and when the hourly REST heartbeat reports a newer release), the same update runs without
  a handoff.
- Homebrew and winget installs are left to their package manager (`brew upgrade productos-agent`,
  `winget upgrade ProductOS.Agent`); `status`, `doctor` and a run's `update_available` note say when one is due.
- Development builds (`0.0.0-dev`) never update themselves. `productos-agent update` does it by hand.

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
`internal/oauth` (discovery, registration, PKCE, loopback, refresh), `internal/update` (versions, install method,
download, checksum, swap, re-exec), `internal/secrets`, `internal/config`, `internal/paths`, `internal/gitx`,
`internal/seed` (planting a seed), `internal/tools`, `internal/service`, `internal/logx`, `internal/protocol`. The report schema both tools answer is
`internal/runner/report_schema.json` (embedded).

## Release

Tag and push: `git tag v0.1.0 && git push origin v0.1.0`. The `release` workflow runs goreleaser: archives per OS/arch,
`checksums.txt`, the GitHub release, the Homebrew formula in `productos-technology/homebrew-tap`, and a winget pull request.
Secrets: `HOMEBREW_TAP_GITHUB_TOKEN`, `WINGET_GITHUB_TOKEN`. `install.sh` here is the real installer the server
serves at `/install.sh`.

## License

MIT.
