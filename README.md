# Barroso Harness

An agent harness written in **Go** from scratch: a single binary, no third-party base, for Windows/macOS/Linux.

- **Permanent YOLO execution**: tools run without approval prompts, allowlists or allow/ask/deny. Technical limits (timeout, output cap, cancellation) are operational — not permissions.
- **Configurable provider** following the **OpenAI Chat Completions** standard (`base_url` + key + model), SSE streaming with `include_usage`.
- **Interactive session** with an in-memory transcript and the commands `/model`, `/effort`, `/provider`, `/new`, `/resume`, `/save`, `/update`, `/help`, `/exit`. In a terminal, output uses an ANSI-256 palette and glyphs (`▶` tool, `✗` error, `✓` ok, `◆` guardrail) with a live status line and a per-turn token footer; in a pipe/`-p` it emits raw text, with no escapes.
- **`BARROSO.md`** becomes the system instruction: from the project directory (and its parents) and/or the global one at `~/.barroso-harness/BARROSO.md`.
- **Context with a guardrail**: a budget per model window, an estimator calibrated by the real `usage`, and monotonic proactive/reactive compaction. Each turn's transcript is only committed after a complete round.
- **Persistent sessions** in append-only SQLite (`sessions.db`), with `/resume` and blobs for large tool results.
- **Subsystems**: MCP (stdio and streamable HTTP), skills (`SKILL.md`), `web_fetch`/`web_search`, async sub-agents (`task_spawn`/`task_status`/`task_result`, with no recursive delegation) and **ACP** (`bh -acp`) for editors.
- **Self-update from the CLI**: `/update` downloads, verifies the SHA256 and installs the newest release.
- **Installation and recipes**: https://harness.barroso.tec.br

## Install

Release binaries (5 platforms + `SHA256SUMS`): the website has the ready-made commands. Or build from source:

```bash
git clone https://github.com/ruanbarroso/barroso-harness
cd barroso-harness && go build -o barroso-harness ./cmd/barroso-harness
```

On Windows, `build-local.cmd [version]` builds into `dist\bh.exe` inside the repo; installing is the job of `/update`.

## Configure and use

On first use, run `bh` (or `barroso-harness`) with no arguments. The CLI asks for:

1. **Base URL** — default `https://keys.barroso.tec.br/v1`; press Enter to keep it or enter another OpenAI Chat Completions compatible endpoint.
2. **API key** — read without echo in the terminal.
3. **Model** — fetches `GET /models`, filters chat models, sorts them alphabetically and lets you pick by number or ID.
4. **Reasoning effort** — starts disabled; you can enable it during the conversation if the provider/model accepts it.

The active key, base URL, provider and model are stored locally in `~/.barroso-harness/config.json` (file mode `0600` where supported; directory `0700`). The key must never be copied into code, README, Git or logs. `BARROSO_HARNESS_HOME` can change only the data directory. `-init` creates the layout non-destructively.

```bash
bh                                  # onboarding (first run) or REPL
bh -p "list the files and tell me what you see"  # one non-interactive turn; uses the saved config
bh -version
bh -init
```

In-session commands:

```text
/model                 lists the text models, full catalog in alphabetical order
/model barroso-chat    switches directly if the ID is in the catalog
/effort                shows the current value and common levels (low, medium, high, xhigh, max)
/effort high           sends reasoning_effort=high on the next requests
/effort -              clears it; reasoning_effort is omitted (no silent fallback)
/provider              changes base URL + key; validates /models before saving
/new                   clears the session transcript (new conversation, config intact)
/update                downloads and installs the newest release from the channel
/skills                lists the skills; /<name> [args] runs one
/tasks                 lists the sub-agents; /tasks <id> shows details; /tasks cancel <id>
/context               active compaction policy and what each one has done
/help
/exit                  also /quit
```

### `/update`

The channel is the **private repository's releases**, published by the deployer. Because they are private, `/update` requires a token **in the environment** — `GITHUB_TOKEN` (or `TOKEN_GITHUB`), in that order. The token never goes into `config.json` or the URL: it only travels in the `Authorization` header.

```bash
export GITHUB_TOKEN=ghp_...      # or TOKEN_GITHUB
bh                               # inside the REPL:
/update
```

Before installing anything, the cycle proves three times that the file is what it claims to be:

1. **SHA256** checked against the `SHA256SUMS` published in the same release — it hashes while downloading and deletes the staged file on mismatch.
2. **Size** checked against the asset metadata.
3. **Exec-check**: the staged binary is executed (`-version`) and its output must contain the expected tag. This is what prevents a channel error HTML page from being installed as an executable.

If the release does not publish `SHA256SUMS`, or it does not list this platform's binary, the update **aborts** instead of proceeding on "trust". On any failure before installation, the running executable is left intact — a failed update never leaves the machine without a harness.

The chosen version is the **highest number** among stable releases, not the first in the list: the API returns them in creation order, and a republish can put an older version first. Trusting the position would offer a downgrade. `v0.1.0` (stable) is newer than `v0.1.0-bh.10`, and the suffix compares numerically — `bh.10` > `bh.9`.

On Windows, the running executable cannot be renamed. When that happens, the command prints the PowerShell `Move-Item` to run afterwards; it also swaps the `bh.exe`/`barroso-harness.exe` alias, which are two files and need two staged copies.

Whether effort levels are accepted depends on the provider/model. If a request is rejected, the error is shown with no automatic downgrade; use `/effort -` to clear it. Model/provider switches keep the committed text messages; pending tool calls are never left orphaned in the transcript.

`bh -p` asks nothing: if the provider/key/model is missing, it exits with an instruction to start `bh` interactively.

## ACP — editor mode

`bh -acp` serves the **Agent Client Protocol** (JSON-RPC 2.0, newline-delimited) over stdio, so an editor or client can drive the turns:

```bash
bh -acp
```

`stdout` is the protocol wire — no banner or log on it; diagnostics go to `stderr`. Methods: `initialize`, `session/new`, `session/prompt`, `session/cancel` and the `session/update` notification (including `tool_call`, so the client can render tools in real time).

`session/new` returns a `sessionId` that is **always filled in** and stores the session in the same `sessions.db` as the REPL — a conversation started in an editor can be resumed in the terminal with `/resume`. Each session gets the same subsystem wiring (MCP, sub-agents) that the REPL builds, so the client sees the full tool catalog, and the child never gets `task_spawn` (delegation is not recursive).

A failing MCP server becomes a warning on `stderr` and does not bring the protocol down; the config is captured once, so `session/new` does not re-read the disk.

## Terminal experience

The interactive session (`bh` with no arguments, in a real terminal) uses the TUI look:

```text
Barroso Harness
type /help for commands · plain text goes to the model

provider keys-barroso · effort high · model barroso-chat · window 250k
> barroso-chat ·high
▶ terminal_run 120ms ok
  exit_code: 0
tokens ↑1200 ↓45 · 890ms
```

As for what you type: the prompt has a space before the effort marker, and the status line is **recomputed on every iteration** — `/effort high` and `/model` show up immediately, without relying on the previous command's summary.

The ANSI-256 palette and the glyphs (`▶` tool, `✗` error, `✓` ok, `⚠` warning, `◆` guardrail) are only emitted when `Out` and `ErrOut` are real terminals — `x/term.IsTerminal`, no TUI framework. `NO_COLOR` (with a value) and `TERM=dumb` turn color off even in a terminal.

What changes when output is **not** a terminal — pipe (`bh | cat`), redirection (`bh > log`), input from a file (`bh < cmds`) and `-p`: it falls back to the **plain** renderer, raw text, not a single escape character. That is why the website recipes and the deploy log stay clean, and why tests always see the same literal substring (`[tool terminal_run ok`).

## Development

```bash
go vet ./... && go test ./... && go build ./...
```

Releases are automatic: push to `main` → `deploy.barroso.tec.br` webhook → binaries published as a release (no GitHub Actions — the account forbids them; the contract is `.deploy/release.json`).

## Context: who decides to compact

`"context_policy"` in `config.json` selects the policy:

- `agente` (default): the model has the `ctx_compact` tool and compacts at a task milestone, writing the working state that replaces the history before the current request. The budget guardrail remains as a safety net.
- `limiar`: no `ctx_compact`; only the guardrail compacts, when the window overflows.

Each compaction is stored in `sessions.db` with the policy that produced it, and `/context` (old alias `/contexto`) aggregates by policy (agent compactions, proactive guardrail, **reactive** guardrail — the sign that the policy came too late — and how much the history shrank). `/resume` and `session/load` re-apply the compaction, so the resumed session comes back with the state, not with the already-summarized conversation.

## Search

`web_search` uses the SearXNG at `search_url` when configured; without it, it falls back to DuckDuckGo lite. Search works out of the box.

## ACP: `session/load`

With `sessions.db` available, `initialize` announces `loadSession: true`. `session/load` reopens the session by id, replays the conversation to the client as `session/update` (`user_message_chunk`, `tool_call`, `tool_call_update`, `agent_message_chunk`) and leaves the session ready for `session/prompt`. An unknown id is `-32602`.

## MCP and skill management by the agent

The model manages its own MCP servers and skills, without hand-editing `config.json`:

- `mcp_list` — configured servers, whether they connected, how many tools, and the error for those that did not connect.
- `mcp_add` — adds (or replaces) a `stdio` server (`command`, `args`, `env`) or an `http` one (`url`, `headers`), connects immediately and saves it to `config.json`. The server's tools are available from the next round of the same turn.
- `mcp_remove` — disconnects, removes the tools from the catalog and deletes it from `config.json`.
- `skill_create` / `skill_remove` — creates or deletes `<scope>/<name>/SKILL.md` (scope `project` = `.barroso/skills`, `global` = `~/.barroso-harness/skills`); the created skill can be invoked with `/<name>`.

Saving re-reads `config.json` from disk and replaces only the server list, so it does not undo a provider/model switch made in the REPL.

## Full screen

In a real terminal, `bh` opens full screen in the OpenCode layout: conversation in the middle, a side panel (title, context used, connected MCP servers), an input box pinned at the bottom and a footer with the directory and context. The panel appears at 110 columns or more.

Keys: `Enter` sends · `↑`/`↓` history of what you typed · mouse wheel and `PgUp`/`PgDn` scroll the conversation · `Esc` or `Ctrl+C` interrupts the response in progress · typing and sending during a response interrupts the turn and sends the new message · `Ctrl+C` twice exits · `Ctrl+U` clears the line · `Ctrl+L` redraws.

The session name appears in the terminal tab (`BH | <name>`) and in the panel: it is the first message, or whatever you set with `/rename new name`.

With the mouse wheel captured, selecting text to copy is `Shift` + drag (Windows Terminal and most terminals).

`BH_SIMPLES=1 bh` goes back to scrolling mode. Pipes, redirection and `bh -p` never use full screen.
