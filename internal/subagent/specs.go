// Specs (tools) that the subagent package exposes to the PARENT agent — and
// only to it. See the package doc: runtime.Session.Send builds each turn's tool
// list with tools.SpecsBuiltins(), so a child session NEVER receives these
// specs; there is no wiring to do and no filter to apply.
package subagent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/tools"
)

// TetoResultado caps the text returned by <prefix>_result: 16 KiB. A
// sub-agent may produce a lot; the tool truncates with a marker and the parent
// asks for the rest some other way (or refines the request).
const TetoResultado = 16 << 10

// Especificacoes returns the model's delegation tool: <prefix>_spawn
// {prompts[]}, which launches the sub-agents and returns immediately. The model
// does NOT get <prefix>_status or <prefix>_result: with them at hand it kept
// polling the status in a loop instead of moving on or ending the turn. Each
// sub-agent's result arrives on its own (AvisoTerminadas); the user follows
// along via /tasks. statusSpec and resultSpec remain for the lifecycle tests.
//
// parentCfg is the parent's configuration, inherited by the tasks (same
// provider, model and key). Spawn does not get a long-lived ctx: the tool
// caller passes the turn ctx and, for tasks that must outlive it, the wiring
// calls Vincular (see subagent.go) with a long-lived context, and then a nil
// ctx inherits that one.
func Especificacoes(g *Gerente, parentCfg config.Config) []tools.Spec {
	if g == nil {
		return nil
	}
	g.ConfigPai(parentCfg)
	p := g.Prefixo()
	return []tools.Spec{spawnSpec(g, p)}
}

// ── <prefix>_spawn ─────────────────────────────────────────────────────────

func spawnSpec(g *Gerente, prefix string) tools.Spec {
	return tools.Spec{
		Name: prefix + "_spawn",
		Description: "Call it ONCE per turn, with all requests in prompts[]. Launches one or SEVERAL sub-agents at once, in the BACKGROUND, and returns their ids immediately. " +
			"Each sub-agent runs autonomously with the built-in tools (terminal, files), but CANNOT " +
			"create other sub-agents. There is no limit on count or concurrency: they all start together. " +
			"There is no tool to check on or wait for them: when each sub-agent finishes or fails, its result arrives " +
			"on its own as a new message, in a new turn. A second call in the same turn is REJECTED with an error. " +
			"Never launch tasks to pass the time or to wait. After launching, only do work that does not " +
			"depend on the results, or END THE TURN telling the user what you launched and that you are waiting.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"prompts": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "one complete, self-contained instruction per sub-agent; N items = N sub-agents in parallel",
				},
			},
			"required": []string{"prompts"},
		},
		Exec: func(ctx context.Context, _ string, raw json.RawMessage) (string, error) {
			var c struct {
				Prompt  string   `json:"prompt"`
				Prompts []string `json:"prompts"`
			}
			if err := json.Unmarshal(raw, &c); err != nil {
				return "", fmt.Errorf("invalid input: %w", err)
			}
			var requests []string
			if strings.TrimSpace(c.Prompt) != "" {
				requests = append(requests, c.Prompt)
			}
			for _, p := range c.Prompts {
				if strings.TrimSpace(p) != "" {
					requests = append(requests, p)
				}
			}
			if len(requests) == 0 {
				return "", fmt.Errorf("provide prompts (or prompt)")
			}
			// One launch per turn. With no tool to wait, the model once
			// launched "dummy" sub-agents just to pass the time; a second call
			// in the same turn is rejected and told to end the turn.
			if !g.firstSpawnOfTurn(ctx) {
				return "", fmt.Errorf("%s", "sub-agents were already launched this turn; put them all in a single "+
					prefix+"_spawn with prompts[]. Do not launch tasks to wait: END THE TURN now "+
					"telling the user — the results arrive on their own in a new turn")
			}
			var ids []string
			for _, p := range requests {
				// nil ctx: the task lives in the manager's long-lived context,
				// not the turn's — the turn may end and the sub-agent keeps going.
				id, err := g.Spawn(nil, p)
				if err != nil {
					return "", err
				}
				ids = append(ids, id)
			}
			return fmt.Sprintf("%d sub-agent(s) launched: %s. Each result arrives on its own as a new message "+
				"when it finishes. Do not call %s_spawn again this turn: only do work that does not depend on the "+
				"results, or end the turn now telling the user what you launched.",
				len(ids), strings.Join(ids, ", "), prefix), nil
		},
	}
}

// ── <prefix>_status ────────────────────────────────────────────────────────

func statusSpec(g *Gerente, prefix string) tools.Spec {
	return tools.Spec{
		Name: prefix + "_status",
		Description: "Lists the sibling tasks with id, state (queued/running/completed/failed/cancelled) " +
			"and age. Does not return results — for the text use " + prefix + "_result.",
		Schema: map[string]any{"type": "object", "properties": map[string]any{}},
		Exec: func(context.Context, string, json.RawMessage) (string, error) {
			list := g.Tarefas()
			if len(list) == 0 {
				return "no tasks yet", nil
			}
			now := time.Now()
			var sb strings.Builder
			for i, t := range list {
				fmt.Fprintf(&sb, "%d. %s %s %s\n", i+1, t.ID, t.Estado, ageText(now, t))
			}
			return strings.TrimRight(sb.String(), "\n"), nil
		},
	}
}

// ageText summarizes the relevant time: for finished tasks, from creation to
// the end; for pending ones, from creation until now.
func ageText(now time.Time, t *Tarefa) string {
	ref := t.TerminadaEm
	if ref.IsZero() {
		ref = now
	}
	d := ref.Sub(t.Criada).Round(time.Second)
	if d < 0 {
		d = 0
	}
	return d.String()
}

// ── <prefix>_result ────────────────────────────────────────────────────────

func resultSpec(g *Gerente, prefix string) tools.Spec {
	return tools.Spec{
		Name: prefix + "_result",
		Description: "Returns the FINAL result of a sibling task. completed: the sub-agent's text " +
			"(truncated at 16 KiB); failed: one line with the error; queued/running/cancelled: one line with the state.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id": map[string]any{"type": "string", "description": "task id, as returned by " + prefix + "_status"},
			},
			"required": []string{"id"},
		},
		Exec: func(_ context.Context, _ string, raw json.RawMessage) (string, error) {
			var c struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(raw, &c)
			id := strings.TrimSpace(c.ID)
			if id == "" {
				return "", fmt.Errorf("id is required")
			}
			cp, ok := g.snapshot(id)
			if !ok {
				return "", fmt.Errorf("task %s not found", id)
			}
			g.markDelivered(id)
			switch cp.Estado {
			case Completed:
				if strings.TrimSpace(cp.Resultado) == "" {
					return fmt.Sprintf("task %s completed with no final text", id), nil
				}
				return truncate(cp.Resultado, TetoResultado, id), nil
			case Failed:
				return fmt.Sprintf("task %s failed: %s", id, orEmpty(cp.Erro)), nil
			case Cancelled:
				return fmt.Sprintf("task %s cancelled%s", id, suffix(cp.Erro)), nil
			case Queued:
				return fmt.Sprintf("task %s starting", id), nil
			case Running:
				return fmt.Sprintf("task %s running", id), nil
			default:
				return fmt.Sprintf("task %s in state %q", id, cp.Estado), nil
			}
		},
	}
}

// truncate applies the cap with a marker at the end.
func truncate(s string, limit int, id string) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + fmt.Sprintf("\n\n[result of task %s truncated at %d bytes; ask for a more specific excerpt]", id, limit)
}

func orEmpty(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(no detail)"
	}
	return s
}

func suffix(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	return ": " + s
}

// AvisoTerminadas builds the message the parent receives when sub-agents
// finish: one block per task with the result (or the error) and, at the end,
// how many are still running. The "[subagents]" prefix lets the screen and
// the session title recognize the message as a notice, not user speech.
func AvisoTerminadas(ts []Tarefa, active int) string {
	var sb strings.Builder
	sb.WriteString("[subagents] Sub-agents finished:\n")
	for i := range ts {
		t := &ts[i]
		dur := t.TerminadaEm.Sub(t.Criada).Round(time.Second)
		fmt.Fprintf(&sb, "\n## %s %s (%s)\n", t.ID, t.Estado, dur)
		switch t.Estado {
		case Completed:
			if strings.TrimSpace(t.Resultado) == "" {
				sb.WriteString("(no final text)\n")
			} else {
				sb.WriteString(truncate(t.Resultado, TetoResultado, t.ID) + "\n")
			}
		default:
			sb.WriteString("error: " + orEmpty(t.Erro) + "\n")
		}
	}
	if active > 0 {
		fmt.Fprintf(&sb, "\n%d sub-agent(s) still running; their results will arrive the same way.\n", active)
	} else {
		sb.WriteString("\nNo sub-agents running now.\n")
	}
	return sb.String()
}
