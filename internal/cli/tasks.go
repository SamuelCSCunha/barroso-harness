// /tasks: o usuário enxerga e controla os sub-agentes que o modelo disparou.
//
// task_spawn / task_status / task_result são ferramentas do MODELO. Sem este
// comando, quem tinha tarefas em background só sabia delas perguntando ao
// próprio modelo — e não tinha como cancelar uma que travou.
package cli

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ruanbarroso/barroso-harness/internal/skills"
	"github.com/ruanbarroso/barroso-harness/internal/subagent"
)

// cmdTasks trata `/tasks`, `/tasks <id>` e `/tasks cancel <id>`. g nil quer
// dizer sub-agentes desligados (subtasks no config.json).
func cmdTasks(out io.Writer, g *subagent.Gerente, arg string) {
	if g == nil {
		avisoFmt(out, `sub-agentes desligados: ligue com "subtasks": true no config.json`)
		return
	}
	campos := strings.Fields(arg)
	switch {
	case len(campos) == 0:
		listarTarefas(out, g.Fotos())
	case campos[0] == "cancel" || campos[0] == "cancelar":
		if len(campos) < 2 {
			avisoFmt(out, "uso: /tasks cancel <id>")
			return
		}
		if g.Cancelar(campos[1]) {
			okFmt(out, "tarefa "+campos[1]+" cancelada")
		} else {
			avisoFmt(out, "nada a cancelar em "+campos[1]+" (inexistente ou já terminada)")
		}
	default:
		detalharTarefa(out, g.Fotos(), campos[0])
	}
}

// listarTarefas imprime uma linha por tarefa e o resumo por estado.
func listarTarefas(out io.Writer, ts []subagent.Tarefa) {
	if len(ts) == 0 {
		fmt.Fprintln(out, dimf("nenhuma tarefa nesta sessão"))
		return
	}
	cont := map[string]int{}
	for i := range ts {
		t := &ts[i]
		cont[t.Estado]++
		fmt.Fprintf(out, "%s %s %s %s\n",
			cianof(fmt.Sprintf("%-4s", t.ID)),
			corEstado(t.Estado),
			dimf(fmt.Sprintf("%6s", duracaoTarefa(t))),
			resumoLinha(t.Prompt, 70),
		)
	}
	partes := []string{}
	for _, e := range []string{subagent.Running, subagent.Queued, subagent.Completed, subagent.Failed, subagent.Cancelled} {
		if cont[e] > 0 {
			partes = append(partes, fmt.Sprintf("%d %s", cont[e], e))
		}
	}
	fmt.Fprintln(out, dimf(strings.Join(partes, " · ")+" — /tasks <id> detalha, /tasks cancel <id> cancela"))
}

// detalharTarefa mostra pedido, estado, resultado/erro e o rastro de tools.
func detalharTarefa(out io.Writer, ts []subagent.Tarefa, id string) {
	for i := range ts {
		t := &ts[i]
		if t.ID != id {
			continue
		}
		fmt.Fprintf(out, "%s %s %s\n", cianof(t.ID), corEstado(t.Estado), dimf(duracaoTarefa(t)))
		fmt.Fprintln(out, dimf("pedido: ")+t.Prompt)
		for _, l := range t.Transcricao() {
			fmt.Fprintln(out, dimf("  "+l))
		}
		if t.Erro != "" {
			fmt.Fprintln(out, vermelhof("erro: ")+t.Erro)
		}
		if t.Resultado != "" {
			fmt.Fprintln(out, t.Resultado)
		}
		return
	}
	avisoFmt(out, "tarefa "+id+" não existe (/tasks lista)")
}

func corEstado(e string) string {
	rot := fmt.Sprintf("%-9s", e)
	switch e {
	case subagent.Running:
		return amarelof(rot)
	case subagent.Completed:
		return verdof(rot)
	case subagent.Failed:
		return vermelhof(rot)
	default:
		return dimf(rot)
	}
}

// duracaoTarefa: quanto rodou (terminada) ou está rodando (em curso).
func duracaoTarefa(t *subagent.Tarefa) string {
	if t.Inicada.IsZero() {
		return "-"
	}
	fim := t.TerminadaEm
	if fim.IsZero() {
		fim = time.Now()
	}
	return fim.Sub(t.Inicada).Round(time.Second).String()
}

func resumoLinha(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}

// listarSkills mostra as skills invocáveis com /<nome>.
func listarSkills(out io.Writer, hab []skills.Skill) {
	if len(hab) == 0 {
		fmt.Fprintln(out, dimf("nenhuma skill: crie .barroso/skills/<nome>/SKILL.md no projeto ou em ~/.barroso-harness/skills"))
		return
	}
	for _, s := range hab {
		desc := resumoLinha(s.Descricao, 70)
		if desc == "" {
			desc = dimf("(sem descrição)")
		}
		fmt.Fprintf(out, "%s %s %s\n", cianof("/"+s.Nome), desc, dimf("("+s.Fontes+")"))
	}
	fmt.Fprintln(out, dimf("/<nome> [argumentos] executa a skill"))
}
