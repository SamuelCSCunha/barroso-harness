// /tasks: the user sees and controls the sub-agents the model spawned.
//
// task_spawn / task_status / task_result are MODEL tools. Without this
// command, background tasks were only visible by asking the model itself —
// and there was no way to cancel a stuck one.
package cli

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ruanbarroso/barroso-harness/internal/skills"
	"github.com/ruanbarroso/barroso-harness/internal/subagent"
)

// cmdTasks handles `/tasks`, `/tasks <id>` and `/tasks cancel <id>`. A nil g
// means a path without wiring (the default Montar always creates the
// manager) — so explain instead of failing silently.
func cmdTasks(out io.Writer, g *subagent.Gerente, arg string) {
	if g == nil {
		warnFmt(out, "sub-agents unavailable in this session: the tool catalog did not go through the default wiring")
		return
	}
	fields := strings.Fields(arg)
	switch {
	case len(fields) == 0:
		listTasks(out, g.Fotos())
	case fields[0] == "cancel" || fields[0] == "cancelar": // "cancelar": legacy alias
		if len(fields) < 2 {
			warnFmt(out, "usage: /tasks cancel <id>")
			return
		}
		if g.Cancelar(fields[1]) {
			okFmt(out, "task "+fields[1]+" canceled")
		} else {
			warnFmt(out, "nothing to cancel in "+fields[1]+" (unknown or already finished)")
		}
	default:
		showTask(out, g.Fotos(), fields[0])
	}
}

// listTasks prints one line per task and a per-state summary.
func listTasks(out io.Writer, ts []subagent.Tarefa) {
	if len(ts) == 0 {
		fmt.Fprintln(out, dimf("no tasks in this session"))
		return
	}
	counts := map[string]int{}
	for i := range ts {
		t := &ts[i]
		counts[t.Estado]++
		fmt.Fprintf(out, "%s %s %s %s\n",
			cyanf(fmt.Sprintf("%-4s", t.ID)),
			stateColor(t.Estado),
			dimf(fmt.Sprintf("%6s", taskDuration(t))),
			oneLine(t.Prompt, 70),
		)
	}
	parts := []string{}
	for _, e := range []string{subagent.Running, subagent.Queued, subagent.Completed, subagent.Failed, subagent.Cancelled} {
		if counts[e] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[e], e))
		}
	}
	fmt.Fprintln(out, dimf(strings.Join(parts, " · ")+" — /tasks <id> for details, /tasks cancel <id> to cancel"))
}

// showTask shows the prompt, state, result/error and the tool trail.
func showTask(out io.Writer, ts []subagent.Tarefa, id string) {
	for i := range ts {
		t := &ts[i]
		if t.ID != id {
			continue
		}
		fmt.Fprintf(out, "%s %s %s\n", cyanf(t.ID), stateColor(t.Estado), dimf(taskDuration(t)))
		fmt.Fprintln(out, dimf("prompt: ")+t.Prompt)
		for _, l := range t.Transcricao() {
			fmt.Fprintln(out, dimf("  "+l))
		}
		if t.Erro != "" {
			fmt.Fprintln(out, redf("error: ")+t.Erro)
		}
		if t.Resultado != "" {
			fmt.Fprintln(out, t.Resultado)
		}
		return
	}
	warnFmt(out, "task "+id+" does not exist (/tasks lists them)")
}

func stateColor(e string) string {
	label := fmt.Sprintf("%-9s", e)
	switch e {
	case subagent.Running:
		return yellowf(label)
	case subagent.Completed:
		return greenf(label)
	case subagent.Failed:
		return redf(label)
	default:
		return dimf(label)
	}
}

// taskDuration: how long it ran (finished) or has been running (in progress).
func taskDuration(t *subagent.Tarefa) string {
	if t.Inicada.IsZero() {
		return "-"
	}
	end := t.TerminadaEm
	if end.IsZero() {
		end = time.Now()
	}
	return end.Sub(t.Inicada).Round(time.Second).String()
}

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}

// listSkills shows the skills invocable with /<name>.
func listSkills(out io.Writer, skillList []skills.Skill) {
	if len(skillList) == 0 {
		fmt.Fprintln(out, dimf("no skills: create .barroso/skills/<name>/SKILL.md in the project or in ~/.barroso-harness/skills"))
		return
	}
	for _, s := range skillList {
		desc := oneLine(s.Descricao, 70)
		if desc == "" {
			desc = dimf("(no description)")
		}
		fmt.Fprintf(out, "%s %s %s\n", cyanf("/"+s.Nome), desc, dimf("("+s.Fontes+")"))
	}
	fmt.Fprintln(out, dimf("/<name> [arguments] runs the skill"))
}
