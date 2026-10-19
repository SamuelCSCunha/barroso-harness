# OpenCode v2 TUI — parity specification for bh

Source: `anomalyco/opencode` @ `6bac3b9` (2026-10-19), `packages/tui` v2.0.32, `@opentui/core` 0.6.6.
All paths below are relative to `packages/tui/src/`. `S:` = `routes/session/index.tsx`, `P:` = `component/prompt/index.tsx`.
Sizes are terminal cells. Colors are resolved from the default theme `opencode` (dark, `theme/assets/v2/opencode.json`).

bh side read: `internal/cli/screen.go`, `repl.go`, `clipboard.go`, `tui.go`, `ui.go`, `README.md`.

Legend for "bh" notes: **OMIT** = no bh functionality, leave out. Everything else is in the mapping table (§12).

---

## 0. Theme (default `opencode`, dark) — the colors to copy

`context/theme.tsx:114,116` (default name `opencode`, mode `dark`). Gray scale: 100 `#ffffff`, 200 `#eeeeee`, 300 `#b5b5b5`, 400 `#808080`, 500 `#4c4c4c`, 600 `#1e1e1e`, 700 `#141414`, 800 `#0a0a0a`, 900 `#030303`. `theme.decrease(x)` = one step darker *in the scale toward lower contrast* (raised.base `#141414` → `#1e1e1e`).

| Token | Hex | Used for |
|---|---|---|
| `background.base` | `#0a0a0a` | whole screen, dialog panel |
| `background.raised.base` | `#141414` | sidebar, user message box, block tools, queued dock |
| `background.raised.high` | `#1e1e1e` | autocomplete popup, toast, resize-handle hover |
| `decrease(raised.base)` | `#1e1e1e` | **prompt box background**, hover on user msg / block tool |
| `text.base` | `#eeeeee` | normal text |
| `text.muted` | `#808080` | secondary text, inline tools |
| `text.action.primary.selected` | `#fab283` | `/update` highlight, shell-mode bar |
| `text.action.primary.focused` | `#0a0a0a` | text of the selected list row |
| `background.action.primary.focused` | `#fab283` | selected row background (dialogs, autocomplete) |
| `text.action.secondary` base / hovered | `#808080` / `#eeeeee` | "Jump to latest ↓" |
| `text.formfield.selected` | `#fab283` | "current" row (●) in lists |
| `feedback.error` | `#e06c75` | errors |
| `feedback.warning` | `#f5a742` | Thought/Thinking, variant (effort), `esc again to interrupt`, `[Image N]` bg |
| `feedback.success` | `#7fd88f` | MCP connected dot |
| `feedback.info` | `#56b6c2` | info |
| `border.base` | `#484848` | prompt bar base color, queued dock bar |
| `scrollbar.base` | `#606060` | sidebar scrollbar thumb |
| `hue.accent[200]` | `#9d7cd8` (purple) | dialog category headers |
| Agent color "build" | categorical / blue `#5c9cf5` family | prompt bar, user message bar, footer agent name |
| markdown: text `#eeeeee`, heading `#9d7cd8`, link `#fab283`, code `#7fd88f`, strong `#f5a742`, emphasis `#e5c07b`, listItem `#fab283` | | assistant markdown |
| diff: added bg `#20303b`, removed bg `#37222c`, added sign `#b8db87`, removed sign `#e26a75`, line nr `#8f8f8f` | | edit diffs |

bh today uses ANSI-256 approximations (`tui.go:25-30`: blue 75, orange 215, gray 240, text 252; screen bg 235/234). To be identical bh must switch to 24-bit SGR (`38;2;r;g;b`) with the hex values above (Windows Terminal supports truecolor). Themes switching: **OMIT**.

---

## 1. Session screen layout

Root (`app.tsx:1373`): full-screen column, background `background.base` `#0a0a0a`. No title bar.
Session tabs (horizontal strip / vertical rail, `component/session-tabs.tsx`): **OMIT** (bh = one session per terminal; title goes to OSC 0 tab title, which bh already does).

### 1.1 Horizontal split (`component/session-frame.tsx:261-399`, `ui/layout.ts`)

```
| session pane (flexGrow 1)                                   | sidebar 42 cols (only if width > 120) |
```

- `wide = terminalWidth > 120` (`session-frame.tsx:106`) — **the only breakpoint**. bh uses `>= 110` and a 40-col panel → must become `> 120` and **42**.
- Sidebar width fixed `SESSION_SIDEBAR_WIDTH = 42` (`ui/layout.ts:1`), docked flush right, **no resize handle** for the sidebar (drag-resize exists only for terminal/plugin panes: **OMIT**).
- Sidebar visibility: `config.session.sidebar` `"auto"` (default) → visible when wide. Toggle `session.sidebar.toggle` = `<leader>b` (`ctrl+x` then `b`), persisted as `"hide"`/`"auto"`.
- Narrow (≤120) with sidebar explicitly opened: overlay — full-frame backdrop `RGBA(0,0,0,70)`, sidebar 42 wide flush right (`session-frame.tsx:385-396`); `esc` or `ctrl+c` closes it (`S:251-255`).

### 1.2 Session pane, top to bottom (`S:1388-1536`)

Outer box: `paddingLeft = paddingRight = 2` (1 if width < 44), `paddingBottom = 1`. Content width = paneWidth − 4 (`S:256`). **No max width, no centering.** No top margin row (bh has a blank row 1 — remove it; the first message row has its own `marginTop 1`, so visually row 1 is blank anyway, but it scrolls with content).

1. **Transcript scrollbox** (flexGrow 1). `stickyScroll`, `stickyStart="bottom"`. Scrollbar hidden by default (`config.session.scrollbar ?? false`, `S:239`). Every row has `marginTop={1}` (`S:1558`) → exactly one blank line before every row (message, part, tool, footer).
2. **Status row**, height 1, right-aligned (`S:1446-1466`): `Jump to latest ↓` (paddingLeft 1, `#808080`, hover `#eeeeee`, click → scroll to bottom) shown only when scrolled away from the bottom. Otherwise the row is empty (but always present).
3. **Queued dock** (only if queued prompts) — see §10.
4. **Prompt** (§3), or the permission/form prompt in its place (**OMIT**: bh has no permission prompts).

### 1.3 Exact rows for a terminal of height H (no sidebar, 1-line input)

| Row(s) | Content |
|---|---|
| 1 … H−8 | transcript (each entry preceded by a blank line) |
| H−7 | status row (`Jump to latest ↓` right-aligned, or blank) |
| H−6 | prompt: `┃` + padding row (paddingTop 1) |
| H−5 | prompt: `┃  <text>` (grows upward up to `max(6, floor(H/3))` lines) |
| H−4 | prompt: `┃` + blank (metadata paddingTop 1) |
| H−3 | prompt: `┃  Build · model provider · variant` |
| H−2 | bottom cap: `╹` then `▀` × width in prompt bg color |
| H−1 | footer row (§5) |
| H   | blank (pane paddingBottom 1) |

Columns: pane padding 2 → prompt `┃` at column 3; prompt text at column 3+1+2 = **6**. Transcript content (assistant text, inline tools) has `paddingLeft 3` inside the pane → column **6** as well; user message `┃` at column 3, its text at column 6. bh already puts box bar at column 3 and text at col 6 — matches; but bh has no `▀` cap, has a bottom padding row inside the box, and puts the footer on the very last row.

### 1.4 Home / empty screen (`routes/home.tsx:81-122`)

Shown before the first message (route `home`, no session yet):
- column, `alignItems center`, horizontal padding 2.
- flex spacer · 3-row spacer · **logo** (`component/logo.tsx`, `logo.ts`: two 4×19 blocks "open" muted + "code" bold, gap 1; hidden if H < 12; stacked if W < 44) · 1-row spacer · update notice (`/update to install vX`, `/update` in `#fab283`, rest muted, clickable) · **prompt** (`width 100%`, `maxWidth 75`, paddingTop 1) · flex spacer.
- Placeholder (home only): `Ask anything… "<example>"`, examples `Fix a TODO in the codebase` / `What is the tech stack of this project?` / `Fix broken tests`, chosen at random once (`P:1600-1612`). **Inside a session no placeholder is shown** (session route passes none).
- Home footer (`feature-plugins/home/footer.tsx`), shown if H ≥ 14 and W ≥ 44, padding 1/2: left `⊙ N MCP` (⊙ green if any connected, else muted; `⊙ N MCP failed` red) + muted `/mcps` (W ≥ 64), clickable → MCP dialog; right: version string muted (W ≥ 64).
- bh has no home screen (it starts directly in the session layout). Logo: bh would need its own "bh" logo — the layout (centered prompt max 75 wide) is what matters.

---

## 2. Message rendering

Primitives: left bar = `SplitBorder` vertical `┃` (`ui/border.ts:15-20`). Spinner (`component/spinner.tsx`): braille `⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏` at 80 ms, glyph + 1 space + label same color. Inline icon width = 2 (`routes/session/message-parts.tsx:19`). Every click handler ignores the click if there is a text selection.

### 2.1 User message (`S:2221-2350`)
- Outer: `┃` left border colored with the **agent color** (pending/steer: `border.base`), background `raised.base` `#141414`, **full width**.
- Inner: `paddingTop 1`, `paddingBottom 1`, `paddingLeft 2`, no right padding. Text `#eeeeee`, raw (no markdown, wraps by word). Hover → background `#1e1e1e`.
- Attachments: chips on a new line (paddingTop 1, gap 1): `" file "` tag (bold, bg `#9d7cd8`, fg `#141414`) + `" name "` (bg `#1e1e1e`, fg muted). Images: thumbnails only if `session.image_preview` (default off) — so by default an image attachment shows as a `file` chip with its name. **No `[Image N]` text in the transcript.**
- No timestamp.
- Click → "Message Actions" dialog (`routes/session/dialog-message.tsx`): `Jump to`, `Revert` (OMIT), `Copy` ("message text to clipboard"), `Fork` (OMIT).
- bh today: bar `▌` (half block) blue on bg 235, same paddings → change glyph to `┃`, bg to `#141414`, bar color to agent color (bh: use the primary blue), add hover + click.

### 2.2 Assistant text (`message-parts.tsx:172-199`)
`paddingLeft 3`, markdown (rendered, concealed markers), fg `#eeeeee` on `#0a0a0a`, tables as grid. No label, no bar.

### 2.3 Assistant footer (`S:1888-1938`)
One line after the **last step of each turn** (`rows.ts:403-406`), `paddingLeft 3`:
`Build · <model name> · 3.2s` (+ ` · 12.3 tok/s` only if `config.session.tps`) (+ ` · interrupted` if interrupted).
- `Build` = titlecased agent, in agent color; rest `#808080`. Model hidden if W < 28; duration hidden if 28 ≤ W < 36.
- Duration = from user message creation to step completion.
- Error (non-interrupt): line above `Error: <msg>` in `#e06c75`, footer gets marginTop 1.
- bh today: `model · 8.1s · 45.2 tok/s` with model in text color, no agent → bh has no agents: **use no agent prefix**; put model first in the color that the agent name would take (decision: keep `model` as first element in primary color, rest muted). Keep tok/s (bh feature).

### 2.4 Reasoning (`message-parts.tsx:42-170`, `group-view.tsx:162-193`)
Default `session.thinking = "hide"`:
- running: spinner + `Thinking` (or `Thinking: <bold title from reasoning>`), color `#f5a742` (warning).
- done, collapsed: `+ Thought: <title> · 8.1s` — `+`/`-` prefix, color warning at 60% alpha, full warning on hover; consecutive thoughts merge: ` · N steps`.
- click toggles expanded: `- Thought · 8.1s`, then (marginTop 1) a `┃` bar (color `decrease(background.base)`), paddingLeft 1, the reasoning markdown all in `#808080` (not italic).
- bh today: `+ Thought · 8.1s` in orange 215, not clickable, content not kept → need to keep reasoning text and allow click expand; color → `#f5a742`; add `: <title>` when the reasoning starts with `**Title**`.

### 2.5 Tool calls (`S:2414-3500`)
Name map (`util/tool-display.ts`): `bash`→shell, `task`→subagent, `apply_patch`→patch.

**Inline tool** (`InlineToolRow`): `paddingLeft 3`, `<icon><space><label>`, color `#808080`; hover (if clickable) `#eeeeee`; failed → `#e06c75` and click toggles the error text below (indent 2); permission-denied → strikethrough. While pending: spinner + pending text.

**Block tool** (`BlockTool`, `S:2735-2817`): `┃` colored `background.base` (invisible gap), background `#141414`, `paddingTop 1 / paddingBottom 1 / paddingLeft 2`, `gap 1`; header muted; hover `#1e1e1e` if clickable; error message at bottom in red.

| Tool | Style | Rendering (exact) |
|---|---|---|
| shell/bash | block | `$ <command>` (`cd <wd> && ` prefix if workdir≠.) in `#eeeeee`; output muted below. Collapsed to **10 lines** total (`collapseShellOutput`, `util/collapse-tool-output.ts`): command ≤2 lines, output keeps the **tail** with `(N earlier lines)` first. Click block toggles full output. **No "click to expand" hint.** Running: spinner + command. Errors: `Command exited with code N` / `Command cancelled` / `Command timed out`. |
| read | inline | `→ Read <path>[:from-to]`, pending `Reading file…` |
| glob | inline | `✱ Glob "<pattern>" in <path> (N matches)`, pending `Finding files…` |
| grep | inline | `✱ Grep "<pattern>" in <path> (N matches)`, pending `Searching content…` |
| webfetch | inline | `% WebFetch <url>`, pending `Fetching from the web…` |
| websearch | inline | `◈ Web Search "<query>"` |
| write | block | header `# Wrote <path>`, full content with line numbers (muted, min width 3) |
| edit | block | header `← Edit <path>`, diff: **split if content width > 120 else unified** (`S:3296`), line numbers, diff colors §0 |
| patch | block per file | `# Created` / `# Deleted` / `← Patched` + path + diff |
| task/subagent | inline, clickable → child session | icon `│` running (spinner) / `✓` done; `General Subagent — <description> · <model>`; pending `Delegating…` |
| skill | inline | `→ Skill "<name>"` |
| todowrite & unknown | inline generic | `✓ <tool> k=v, k=v` (`✗` on error), click expands `key: value` lines + `output: …` at indent 5 |

**Grouping** (`routes/session/grouping/session.ts:78-91`, `group-view.tsx:115-157`): consecutive read/glob/grep/webfetch/websearch collapse into one inline row: running `✱ Exploring: 2 reads, 1 search`, done `→ Explored: 2 reads, 1 search, 1 fetch`; click expands to the individual rows.

bh today: every tool is `✓ name args 120ms` + `› first line (+N lines)` preview. Differences: per-tool icons/labels, no duration on tool lines, no `›` preview line for read/grep/glob (OpenCode shows **no output** for inline tools), bash as a block with 10-line tail, edit/write as diff blocks, click to expand.

### 2.6 Other rows
- Retry: `⚠ Retrying in Ns · attempt N · <error>` warning color, paddingLeft 3 (`S:2385-2408`).
- Compaction: full-width `─` rule with centered ` Compaction · X in · Y out ` (`S:2038`). (bh: `◆ context compacted` → map to this.)
- Subagent completion notice: `↳ <Agent> finished · <desc>` in info color / `! … failed` in error color, clickable (`S:1970-2028`). (bh: `⚑ sub-agents finished: …` → map to this.)
- Background hint (shell/subagent > 3 s): muted `Press ctrl+b to move running work to the background` — **OMIT**.

---

## 3. Prompt (input box) — `P:1663-1975`

- Outer: left border only, `┃`, bottom-left char `╹`. Color = `border.base` tinted toward the agent color (i.e., the agent color; `border.base` `#484848` when leader pending or muted). No top/right/bottom border.
- Inner: background `#1e1e1e` (`decrease(raised.base)`), `paddingLeft/Right 2` (1 if W<44), `paddingTop 1`, **no paddingBottom**.
- Textarea: `minHeight 1`, `maxHeight = max(6, floor(H/3))` (`P:1659`), grows downward-in-box/upward-on-screen, scrolls internally after max. Cursor: block, blinking, `#eeeeee`. Text `#eeeeee`. Multi-line with cursor movement (left/right/up/down/word jumps — §7).
- Metadata row (`component/prompt/metadata.tsx`), paddingTop 1, left-aligned: `Build · <model> <provider> · <variant>`
  - agent in agent color; `·` muted; model `#eeeeee`; provider `#808080`; `·` muted; **variant (effort) `#f5a742` bold**.
  - Fit fallbacks for width W−13: full → provider last `/` segment → no provider → truncated model (min 9, `…`).
  - bh: no agent → `<model> <provider> · <effort>`; bh today draws model bold text, provider gray, effort orange 215 → change effort to bold `#f5a742`, model not bold.
- Bottom cap row (height 1): `╹` in bar color, then `▀` × width in `#1e1e1e` (half-block edge) (`P:1857-1884`).
- Attachments in the textarea: virtual extmark tokens, atomic for backspace: `[Image N]` / `[PDF N]` / `[Pasted ~N lines]` styled fg `#0a0a0a`, **bg `#f5a742`**, bold; `@path` fg `#f5a742` bold (`packages/theme/src/tui/syntax.ts:13-20`). bh: yellow fg 221 → change to inverted warning style; make backspace delete whole token.
- Paste: CRLF/CR → LF; text with ≥3 lines **or** >150 chars becomes `[Pasted ~N lines]` (expanded on submit; click on the token expands it inline) (`P:1461-1473`). Empty bracketed paste → read clipboard image (bh already does this).
- Autocomplete popup (`component/prompt/autocomplete.tsx`):
  - position: absolute, **directly above the prompt**, same left/width as the prompt box, `zIndex 100`; height `min(10, items, rows above)`; `┃` left border `#484848`, background `#1e1e1e`→ `raised.high`.
  - triggers: `/` only at offset 0 with no whitespace; `@` at a word start.
  - rows: paddingLeft/Right 1; `/name` padded to longest+2 then description muted; selected row fg `#0a0a0a` on `#fab283`.
  - filter: `fuzzysort` (threshold 0 for `/`, 0.5 for `@`, limit 10, prefix match ×2, × (1+frecency)). Files via server-side fuzzy find, limit 20.
  - keys: `up`/`ctrl+p`, `down`/`ctrl+n` (wrap), `return` select, `tab` complete (dir → `@dir/` keeps open), `esc` hide, `ctrl+c` removes the typed `/token`.
  - mouse: move/down highlights, **mouse-up selects**.
  - empty texts: `No matching commands`, `Searching…`, `No matching files, agents, or references`.
  - insertion: `/name ` replaces the line start; `@path ` inserted as a styled token.

---

## 4. Sidebar — `routes/session/sidebar.tsx`

Box: width 42, height 100% of the pane row, background `#141414`, padding top 1, bottom 1, left 2, right 2 → text at sidebar column 3, 38 usable columns.
1. Title: bold `#eeeeee`, wraps, `paddingRight 2`, `paddingBottom 1`. Fallback title = timestamped `New session - <date>` (`withTimestampedFallback`).
2. Scrollable content (`gap 1` between sections, 1-col scrollbar on the right, track `#141414`, thumb `#606060`), in order:
   - **Context** (`feature-plugins/sidebar/context.tsx`): bold `Context`; muted `12,345 tokens`; muted `37% used`; muted `$0.12 spent` (only if cost > 0). Hidden entirely when there is no usage yet. Not collapsible. (bh shows `—` when empty → hide instead; bh uses `.` thousands → OpenCode `toLocaleString` = `,` in en-US.)
   - **MCP** (`sidebar/mcp.tsx`): header bold `MCP`; collapsible only when > 2 servers: `▼ MCP` / `▶ MCP` + muted ` (N active, M errors)` when closed; **mouse-down on header toggles**, persisted. Rows: `•` (green connected / red failed / warning needs auth / muted disabled) + bold name (truncated) + status `Connected`/`Connecting`/`Error`(red)/`Disabled`/`Sign in`, gap 1. **Click a row → MCP dialog** for that server. Hidden when there are no servers (bh shows `no servers` → hide).
3. Footer (pinned bottom, paddingTop 1): `~/path:branch` muted (hover `#eeeeee`, `maxWidth 38`, clickable → working-dir actions (**OMIT** action, keep text)). "Getting started" card: **OMIT**.

---

## 5. Footer line (under the prompt) — `P:1885-1974`, `feature-plugins/prompt/footer.tsx`

Row, `justifyContent space-between`, `gap 2`, aligned to the prompt box (inside pane padding, i.e. starts at column 3).

- **Left** (`prompt.footer.status`):
  - idle: `~/dir:branch` muted, hover `#eeeeee`, clickable (**OMIT** click).
  - running: spinner at marginLeft 1 — "knight rider" blocks, 8 cells, `■` active / `⬝` inactive, colors from the agent color (alpha trail, min 0.3), 40 ms/frame (`ui/spinner.ts:273-326`) — then `esc interrupt` (`esc` `#eeeeee`, `interrupt` `#808080`); after the first esc: `esc again to interrupt` all in `#f5a742` (`P:108-145`). The location label is hidden while running.
- **Right**:
  - with usage: `[N subagents ·] 12,345 (37%) · $0.12` muted (`util/session.ts:63`: `formatContextUsage` → `"12,345 (37%)"`), then ` ctrl+p commands` if it fits.
  - without usage: `shift+tab agents` (OMIT) and `ctrl+p commands` (key `#eeeeee`, word `#808080`); hidden if W < 44.
  - running subagents: `↓ 2 subagents` (clickable → subagent picker) — bh: map to `/tasks`.
  - Fit rule: available = (W−8) − min(28, (W−8)/2); drop shortcuts first, then usage.
  - When interrupt armed and W < 80, the right side is hidden.

bh today: left `~/cwd` (OK), right `esc interrupt` (wrong side) + `12.3K (5%)` (format differs) + `/help commands` (→ `ctrl+p commands` once the palette exists). bh footer is on row H; OpenCode's is on row H−1 with a blank row H.

---

## 6. Dialogs — `ui/dialog.tsx`, `ui/dialog-select.tsx`

- **Backdrop**: full screen, `RGBA(0,0,0,150)` (≈ 59% black over everything), `zIndex 3000`. **Mouse-up on the backdrop closes** (unless a selection existed at mouse-down).
- **Panel**: horizontally centered; **top at `H/4`** (not vertically centered); width `medium 60` / `large 88` / `xlarge 116`, `maxWidth W−2`; background `#0a0a0a` (`background.base` of the dialog surface); `paddingTop 1`; **no border**.
- Layout: `paddingLeft/Right 3`, `paddingBottom 1`, `gap 1`: header row = bold title `#eeeeee` left + `esc` muted right (clickable, closes).
- **Search input** under the header: placeholder `Search` (per dialog: `Search models`, `Search sessions`, …), muted; autofocused.
- **List**: grouped by category; header bold `#9d7cd8`, paddingLeft 3, blank line before every header except the first. Row paddingLeft 3 (1 + marker when current); active row: bg `#fab283`, fg `#0a0a0a`, bold; current item: `●` and fg `#fab283`; title truncated to 61; ` description` muted; footer right-aligned muted. Visible rows = `min(rows, floor(H/2) − 6)`; scrollbar hidden; wheel scrolls.
- Filter: `fuzzysort` over title (×2) + category + searchText.
- Keys: `up`/`ctrl+p`, `down`/`ctrl+n` (wrap), `pageup`/`pagedown` (±10), `home`/`end`, `return` select, `esc` close (clears selection first), `ctrl+c` clears search text, else closes.
- Mouse: **move highlights**, mouse-down highlights, **mouse-up selects** (`dialog-select.tsx:751-767`).
- Empty: `No items available` / `No results found`.
- Footer hints (actions): `title key` pairs, primary right, secondary left.

Specific dialogs:
| Dialog | Open | Size | Contents |
|---|---|---|---|
| Command palette (`component/command-palette.tsx`) | `ctrl+p` | medium | title `Commands`; `Suggested` group first, then all commands by category (`Session`, `Prompt`, `System`, …), footer = shortcut; threshold 0.7 |
| Model (`dialog-model.tsx`) | `<leader>m`, `/models` | medium | title `Select model`, placeholder `Search models`; sections `Favorites`, `Recent`, then one per provider; row = model name, description = provider, footer `Free`; `ctrl+f` favorite, `ctrl+a` providers; selecting a model with variants opens the variant dialog |
| Variant (`dialog-variant.tsx`) | `/variants` (aliases `thinking`, `effort`), `ctrl+t` cycles | medium | title `Select variant`; `Default` + each variant |
| Sessions (`dialog-session-list.tsx`) | `<leader>l`, `/sessions` (aliases `resume`, `continue`) | **large 88** | title `Sessions`; groups `Pinned`, `Today`, then `Mon Oct 13 2026` (`toDateString()`); row = title; `ctrl+d` delete (2nd press confirm: row reads `Press ctrl+d again to confirm`, destructive colors), `ctrl+r` rename, `ctrl+f` pin; server search debounced 150 ms |
| Rename (`dialog-session-rename.tsx`) | `ctrl+r`, `/rename` | medium | prompt dialog, title `Rename session`, placeholder `Session title`, prefilled |
| MCP (`dialog-mcp.tsx`) | `/mcps`, sidebar click | medium | title `MCP servers`; footer `Connected ✓`/`Failed !`/…; `space` toggles |
| Skills (`dialog-skill.tsx`) | `/skills` | medium | title `Skills`; name padded + description |
| Status (`dialog-status.tsx`) | `<leader>s`, `/status` | medium | `N MCP servers` + rows |
| Help (`ui/dialog-help.tsx`) | `/help` | medium | `Press ctrl+p to see all available actions and commands in any context.`; `esc/enter` |
| Message actions (`routes/session/dialog-message.tsx`) | click user message | medium | `Jump to`, `Copy` (+ Revert/Fork OMIT) |
| Timeline (`dialog-timeline.tsx`) | `<leader>g`, `/timeline` | large | user messages newest first; moving selection scrolls the transcript live |

**Toast** (`ui/toast.tsx`): absolute `top 1`, `right 2`, `maxWidth min(60, W−6)`; `┃` left and right borders in the variant color; background `#1e1e1e`, padding 1 vertical / 2 horizontal; default duration **5000 ms**, hover pauses, click dismisses. Used for "Copied to clipboard", errors, "No variants available". bh uses a 2-second yellow notice in the footer → move to toast.

---

## 7. Keybindings (`config/keybind.ts`), leader = `ctrl+x`, leader timeout 2000 ms (`context/keymap.tsx:143`)

Only bindings relevant to bh features are listed in full; the rest are **OMIT** (tabs, terminal panes, diff viewer, which-key, agents, undo/redo, share, worktrees, stash, export, fork, compact-on-demand*).

| Key | Action | bh |
|---|---|---|
| `return` | submit | has |
| `shift+return`, `ctrl+return`, `alt+return`, `ctrl+j` | newline | missing |
| `up` / `down` | cursor up/down in multi-line text; at offset 0 / end → history prev/next (only if text empty or equals current history item) | has (no multi-line rule) |
| `left`/`ctrl+b`, `right`/`ctrl+f` | cursor | missing (bh appends only) |
| `ctrl+a` / `ctrl+e` | line start / end | missing |
| `alt+f`/`ctrl+right`, `alt+b`/`ctrl+left` | word fwd/back | missing |
| `ctrl+w`, `ctrl+backspace`, `alt+backspace` | delete word back | missing |
| `ctrl+k` / `ctrl+u` | delete to line end / start | bh `ctrl+u` clears the whole line (differs) |
| `ctrl+d`, `delete` | delete char forward (`ctrl+d` exits only when prompt empty, via `app.exit`) | bh `ctrl+d` = exit always |
| `ctrl+-` / `ctrl+.` | undo / redo in input | missing |
| `ctrl+c` | non-empty prompt: clear; empty: **exit** (`app.exit` = `ctrl+c,ctrl+d,<leader>q`) — single press, no "press again" | bh: 2 presses within 2 s |
| `escape` | while running: interrupt (2 presses within **5000 ms**; first press arms `esc again to interrupt`); closes autocomplete/dialog | bh: 1 press interrupts |
| `ctrl+v` | paste (image from clipboard) | has |
| `ctrl+p` | command palette | missing |
| `<leader>m` | model list | missing (`/model`) |
| `ctrl+t` | cycle variant (effort) | missing |
| `<leader>l` | session list | missing (`/resume`) |
| `<leader>n` | new session | missing (`/new`) |
| `ctrl+r` | rename session | missing |
| `<leader>b` | toggle sidebar | missing |
| `<leader>s` | status | OMIT-ish (could show MCP) |
| `<leader>y` | copy last assistant message | missing |
| `<leader>g` | timeline (jump to message) | missing |
| `<leader>q` | exit | missing |
| `<leader>e` | open external editor | optional |
| `pageup`/`ctrl+alt+b`, `pagedown`/`ctrl+alt+f` | scroll **half viewport** (`S:791,798`) | bh: H−10 lines |
| `ctrl+alt+u` / `ctrl+alt+d` | scroll quarter viewport ("half page") | missing |
| `ctrl+alt+y` / `ctrl+alt+e` | 1 line | missing |
| `ctrl+g`, `home`, `alt+home` | first message (home/end only act when the prompt is unfocused for some commands; `session.first` is in the base layer) | missing |
| `ctrl+alt+g`, `end` | last message (bottom) | missing |
| `alt+end` | last user message | missing |
| `ctrl+l` | clear screen (mini only) | bh: redraw (keep) |
| `down` (empty prompt, subagents running) | subagent picker | map to tasks |
| `ctrl+z` | suspend (not on win32) | OMIT |

Dialog / autocomplete keys: §3 and §6.

---

## 8. Mouse

Renderer (`app.tsx:242-253`): `useMouse: config.mouse` (default **true**, `config/index.tsx:317`), `targetFps 60`, `exitOnCtrlC false`, `useKittyKeyboard {}`, alt screen, bracketed paste. `enableMouseMovement` is not set → opentui default `true` (`opentui@v0.6.6 packages/core/src/renderer.ts:1357`) → mode "motion" → `packages/native/src/terminal.zig:1004-1034` writes `\x1b[?1000h \x1b[?1002h \x1b[?1003h \x1b[?1006h` (SGR; 1015 never). Also `?1049h` alt screen, `?2004h` bracketed paste, `?1004h` focus reporting, kitty keyboard flags `0b00101`. bh today: 1000 + 1006 only.

Wheel mechanics (`opentui ScrollBox.ts:550-590`): each SGR wheel event has delta 1 × `accel.tick()` (= 3), accumulated fractionally; Shift+wheel scrolls horizontally. Sticky bottom = `scrollTop >= scrollHeight − viewport.height`; scrolling up un-sticks, reaching the exact bottom re-sticks. `toBottom()` (`S:754-767`) waits 50 ms then `stickyScroll = true; scrollTo(scrollHeight)`.

Clipboard write (`clipboard.ts`): `destination: "all-available"` → native host clipboard (on Windows opentui's native Zig clipboard, not PowerShell) **and** OSC 52. bh reads the clipboard via PowerShell; writing should use OSC 52 + native fallback.

Links: root mouse-up opens an OSC 8 link under the cursor only for a plain left click (no drag, no modifiers) (`app.tsx:1386-1390`, `renderer.getLinkAt`).

| Interaction | Target | Behavior | Source |
|---|---|---|---|
| Wheel | transcript | **3 lines per tick** (`CustomSpeedScroll(3)`, `util/scroll.ts:28`; optional macOS acceleration via `scroll.acceleration`) | `S:1418-1424` |
| Wheel | sidebar, dialogs, autocomplete | scroll own list (3 lines) | |
| Wheel up | transcript | breaks stickiness ("away from bottom" → shows `Jump to latest ↓`) | `S:522-536` |
| Click (up) | `Jump to latest ↓` | scroll to bottom, re-stick; hover color `#eeeeee` | `S:1452-1465` |
| Click | user message | Message Actions dialog; hover bg `#1e1e1e` | `S:2257-2290` |
| Click | inline tool (expandable/failed) | toggle details / error; hover fg `#eeeeee` | `S:2641-2710` |
| Click | block tool (bash collapsed) | toggle expanded; hover bg `#1e1e1e` | `S:2735` |
| Click | Thought line | toggle expanded; hover full warning color | `message-parts.tsx:93` |
| Click | exploration group | expand/collapse | `group-view.tsx` |
| Click | subagent tool row / completion notice | open child session (bh: show `/tasks <id>`) | `S:3147` |
| Click | link in markdown | open URL (mouse-up) | `app.tsx:1386-1390` |
| Mouse-down | sidebar `MCP` header (>2 servers) | toggle collapse | `sidebar/mcp.tsx` |
| Click | sidebar MCP row | MCP dialog | |
| Click | prompt textarea | focus + move cursor; click on `[Pasted ~N lines]` expands it | `P:1813-1836` |
| Click | footer subagent count | subagent picker | `prompt/footer.tsx` |
| Click | queued dock | queued prompts dialog | `S:2352` |
| Move / down / up | dialog rows, autocomplete rows | hover = highlight; mouse-up = select | `dialog-select.tsx:751-767`, `autocomplete.tsx:914-916` |
| Click | dialog backdrop / `esc` label | close | `ui/dialog.tsx:51-60,114-120` |
| Click | toast | dismiss; hover pauses timer | `ui/toast.tsx` |
| Drag | anywhere on text | **in-app text selection** (renderer selection, highlighted), works because mouse is captured | `util/selection.ts` |
| Mouse-up after drag | — | copy-on-select: **on Windows default is `manual`** (`terminal.copy`, `app.tsx:563-564`); elsewhere auto-copy on release; toast `Copied to clipboard` | `app.tsx:1386-1390`, `util/selection.ts` |
| Right-click | anywhere | when copy-on-select is off (Windows default): **copy the current selection** | `app.tsx:1377-1384` |
| `ctrl+c` with selection | — | copy selection (+ toast), selection kept | `util/selection.ts:handleSelectionKey` |
| `esc` with selection | — | clear selection | same |
| Drag | pane resize handle (2-col hit zone, 1-col bar highlighted `#1e1e1e`; double-click within 300 ms resets) | terminal/plugin panes only | `ui/pane-resize.ts` — **OMIT** |

Note: clicks are always ignored when a selection with text exists (so drag-select never triggers a click action).

---

## 9. Scrolling

- `stickyScroll` + `stickyStart="bottom"`: new content keeps the view pinned to the bottom while stuck.
- Scrolling up (wheel or keys) sets "away from bottom" (debounced) → `Jump to latest ↓` appears in the status row; returning to the bottom (wheel down to the end, `end`, click) re-sticks automatically.
- Submitting a prompt calls `toBottom()` (`S:1525`).
- Page keys: half viewport; "half page": quarter viewport; line: 1.
- History is loaded lazily in chunks when scrolling to the top (`Loading session history…`) — bh keeps everything in memory, not needed.
- bh today: `↓ N lines below · PgDn to return` (left, gray) → replace with right-aligned `Jump to latest ↓`; bh resets scroll to bottom on **any** new output (`Adicionar` sets `t.scroll = 0`) — OpenCode does **not** yank the view down while the user is scrolled up. Must fix.

---

## 10. Turn lifecycle

- While running: footer left = block spinner + `esc interrupt` (§5). Inside the transcript, the current part shows its own spinner (`Thinking`, tool pending text, `$ command`). There is **no separate "Thinking 3s" line with elapsed seconds**; elapsed time only appears at the end (`Thought · 8.1s`, footer duration).
- Interrupt: `esc` once → `esc again to interrupt` (warning), resets after 5 s; second `esc` → interrupt. Result: footer gets ` · interrupted`; no `Error:` line.
- Typing while busy: the prompt stays usable. `return` while busy sends as **steer** (delivered at the next step boundary; the turn is not cancelled); `<leader>return` sends as **queue** (after the turn). Steer messages appear inline with a gray `┃` (`border.base`); queued ones in the dock above the prompt: `┃` `#484848`, bg `#141414`, padding 1/1/2/1, `N queued · <first prompt>`; click → "Queued prompts" dialog. Empty `return` promotes the first queued prompt to steer.
- bh today: Enter while busy **cancels** the turn and sends. To match: deliver the new message without cancelling (inject at the next tool-round boundary) and show it with the gray bar until consumed — requires runtime support; at minimum show the queued dock.
- Retry: `⚠ Retrying in Ns · attempt N · <error>`.

---

## 11. Elements flagged OMIT (no bh functionality)

Session tabs (strip/rail), vertical-tab resize, terminal panes & pane resize, plugin panels, diff viewer, agents (Build/Plan, `shift+tab`, agent colors → use one fixed primary color), `auto` permission badge, permission/question prompts, themes and light mode, LSP diagnostics, share, undo/redo/revert, fork, worktrees/`/cd`/move, shell mode (`!`), stash, external editor (optional), which-key, `/btw`, background tools (`ctrl+b`), cost `$ spent` (bh has no pricing → omit unless added), "Getting started" card, update-notification on home (bh has `/update`; could reuse), logo "opencode" (replace with bh logo), image thumbnails (off by default anyway), mini mode.

---

## 12. Mapping table: OpenCode → bh status

| # | OpenCode element / control | bh status | Notes / action |
|---|---|---|---|
| 1 | Truecolor theme `#0a0a0a` / `#141414` / `#1e1e1e` / `#eeeeee` / `#808080` | exists but differs | bh uses ANSI-256 (235/234 backgrounds, 252/240 text) and doesn't paint the screen background; switch to 24-bit and paint `#0a0a0a` |
| 2 | Sidebar breakpoint `W > 120`, width 42, bg `#141414`, padding 1/2 | exists but differs | bh: `W >= 110`, width 40, bg 234 |
| 3 | Sidebar title bold + timestamped fallback | exists and matches (text) | fallback `New session` vs `New session - <date>` |
| 4 | Sidebar Context: `12,345 tokens` / `37% used`, hidden when empty | exists but differs | bh uses `.` separator and shows `—` when empty |
| 5 | Sidebar MCP: dot color by status, collapsible >2, click → dialog | exists but differs | bh: static list, `▼ MCP` always, `no servers` shown; no click |
| 6 | Sidebar footer `~/path:branch` | missing — implement | bh has cwd; add git branch |
| 7 | Sidebar toggle `<leader>b`, overlay when narrow | missing — implement | |
| 8 | Prompt `┃` bar + `╹` + `▀` cap, bg `#1e1e1e`, no bottom padding | exists but differs | bh: `▌` bar, bg 235, extra padding row below, no cap |
| 9 | Prompt growth `max(6, H/3)` | exists but differs | bh: `max(1, H/3)` |
| 10 | Multi-line editing, cursor movement, word ops, newline keys | missing — implement | bh only appends/backspaces at end |
| 11 | Metadata row `model provider · effort` (effort bold warning) | exists but differs | bh: model bold, effort orange 215 not bold |
| 12 | Footer row at H−1, blank row H | exists but differs | bh footer on row H |
| 13 | Footer left: spinner + `esc interrupt` while running; location when idle | exists but differs | bh shows `esc interrupt` on the right, no footer spinner |
| 14 | Double-esc interrupt (5 s window, `esc again to interrupt`) | exists but differs | bh: single esc |
| 15 | Footer right `12,345 (37%) · ctrl+p commands` | exists but differs | bh: `12.3K (5%)  /help commands` |
| 16 | `Jump to latest ↓` status row, right aligned, clickable | exists but differs | bh: `↓ N lines below · PgDn to return`, left, not clickable |
| 17 | Sticky bottom that does not yank while scrolled up | exists but differs | bh resets scroll on every output |
| 18 | Wheel 3 lines/tick | exists and matches | |
| 19 | PgUp/PgDn = half viewport | exists but differs | bh: H−10 |
| 20 | home/end/ctrl+g/ctrl+alt+g, line/half-page keys | missing — implement | |
| 21 | User message `┃` + `#141414` + padding 1/2 + hover + click → actions | exists but differs | glyph/colors; no hover/click |
| 22 | User attachments as `file` chips | exists but differs | bh shows `[Image N]` text inside the message |
| 23 | Assistant markdown at indent 3 | exists and matches (indent) | verify colors vs markdown tokens |
| 24 | Footer `Agent · model · 3.2s [· tok/s] [· interrupted]` | exists but differs | bh: no agent (OK), no `interrupted`; colors |
| 25 | Thinking spinner in transcript, `+ Thought: title · 8.1s`, click to expand | exists but differs | bh: elapsed `Thinking 3s` line, no expand, orange 215 |
| 26 | Per-tool icons/labels (→ ✱ % ← $ …), no output for inline tools | exists but differs | bh: `✓ name args dur` + `›` preview |
| 27 | Bash block with 10-line tail, click to expand | missing — implement | |
| 28 | Edit/write diff blocks (split > 120) | missing — implement | bh tool results contain the info |
| 29 | Exploration grouping `Explored: N reads, …` | missing — implement | |
| 30 | Failed tool red + click shows error | exists but differs | bh `✗` red mark only |
| 31 | Subagent row `General Subagent — desc`, click → child | exists but differs | bh: tool line; click → `/tasks <id>` view |
| 32 | Subagent completion notice `↳ … finished` | exists but differs | bh `⚑ sub-agents finished: …` |
| 33 | Compaction rule `─ Compaction ─` | exists but differs | bh `◆ context compacted` |
| 34 | Retry banner | omit unless runtime retries | bh has no visible retry |
| 35 | Autocomplete `/` popup above prompt | missing — bh has the functionality (implement) | commands list from `printHelp` + skills |
| 36 | Autocomplete `@` files | missing — bh has the functionality (implement) | use glob tool logic; insert `@path` token |
| 37 | `[Image N]` token style (bg warning, bold, atomic delete) | exists but differs | bh: yellow fg, deletes char-by-char |
| 38 | `[Pasted ~N lines]` summarization | missing — implement | |
| 39 | Command palette `ctrl+p` | missing — bh has the functionality (implement) | list all slash commands |
| 40 | Model dialog (`/model`, `<leader>m`) with search, Recent | exists but differs | bh: numbered list + typed answer |
| 41 | Variant/effort dialog (`/effort`, `ctrl+t` cycle) | exists but differs | bh: text command only |
| 42 | Session list dialog (`/resume`, `<leader>l`), day groups, rename/delete | exists but differs | bh: numbered list of 10 |
| 43 | Rename dialog (`ctrl+r`) | exists but differs | bh: `/rename <name>` only |
| 44 | Help dialog | exists but differs | bh prints help into the transcript |
| 45 | Skills dialog | exists but differs | bh prints list |
| 46 | MCP dialog | missing — implement | bh has MCP list |
| 47 | Tasks/subagent picker (`down` on empty prompt, footer count) | exists but differs | bh `/tasks` text |
| 48 | Context (`/context`) | exists — no OpenCode equivalent dialog | render as a dialog (Status-style) |
| 49 | Provider (`/provider`) | exists — OpenCode `/connect` (integration dialog) | render as dialog with prompt fields |
| 50 | Dialog backdrop RGBA(0,0,0,150), panel top H/4, width 60/88 | missing — implement | |
| 51 | Toast top-right 5 s | missing — implement | replace footer notices |
| 52 | Mouse modes 1000+1002+1003+1006 | exists but differs | bh 1000+1006 |
| 53 | Click/hover hit-testing on all elements in §8 | missing — implement | |
| 54 | In-app drag selection + right-click copy (Windows) + ctrl+c copy + toast | missing — implement | bh relies on Shift+drag terminal selection |
| 55 | `ctrl+c` empty prompt = exit immediately | exists but differs | bh needs 2 presses (bh's is safer; decide) |
| 56 | `ctrl+d` = delete forward / exit when empty | exists but differs | bh: exit always |
| 57 | `ctrl+u` delete to line start | exists but differs | bh clears all |
| 58 | Enter while busy = steer (no cancel); queued dock | exists but differs | bh cancels the turn |
| 59 | History ↑/↓ only at text start/end | exists but differs | bh always |
| 60 | Home screen (logo + centered prompt max 75 + footer) | missing — implement | |
| 61 | Copy last message `<leader>y`, timeline `<leader>g` | missing — implement | |
| 62 | Leader key `ctrl+x` (2 s) | missing — implement | needed for 7, 39-42, 61 |
| 63 | Tabs, agents, themes, LSP, share, undo/redo, permissions, terminal panes, shell mode | omit — no bh functionality | §11 |
| 64 | OSC 0 terminal title | exists and matches (bh-specific `BH | name`) | OpenCode `OC | name` |
