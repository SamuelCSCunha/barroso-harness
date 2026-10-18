// Especificações (tools) que o pacote subagent expõe ao agente PAI — e somente
// a ele. Ver o doc do pacote: runtime.Session.Send monta a lista de ferramentas
// de cada turno com tools.SpecsBuiltins(), então uma sessão filha NUNCA recebe
// estas specs; não há wiring a fazer nem filtro a aplicar.
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

// TetoResultado é o teto do texto devolvido por <prefixo>_result: 16 KiB. Um
// sub-agente pode produzir muito; a tool corta com marcador e o pai pede o
// resto por outro caminho (ou refina o pedido).
const TetoResultado = 16 << 10

// Especificacoes devolve as três tools de tarefas irmãs, nomeadas com o prefixo
// do gerente:
//
//   - <prefixo>_spawn   {prompt}          → inicia tarefa em segundo plano
//   - <prefixo>_status  {}                → lista id/estado/idade de todas
//   - <prefixo>_result  {id}              → resultado da tarefa (ou estado/erro)
//
// cfgPai é a configuração do pai, herdada pelas tarefas (mesmo provider, modelo
// e chave). O spawn não recebe ctx longo: quem chama a tool passa o ctx do turno
// e, para tarefas que sobrevivam a ele, o wiring chama Vincular (ver subagent.go)
// com um contexto de vida longa e então ctx nil herda esse.
func Especificacoes(g *Gerente, cfgPai config.Config) []tools.Spec {
	if g == nil {
		return nil
	}
	g.ConfigPai(cfgPai)
	p := g.Prefixo()
	return []tools.Spec{
		especSpawn(g, p),
		especStatus(g, p),
		especResult(g, p),
	}
}

// ── <prefixo>_spawn ────────────────────────────────────────────────────────

func especSpawn(g *Gerente, prefixo string) tools.Spec {
	return tools.Spec{
		Name: prefixo + "_spawn",
		Description: "Inicia uma tarefa-irmã em SEGUNDO PLANO e devolve o id. O sub-agente roda " +
			"autonomamente com todas as ferramentas (terminal, arquivos), mas NÃO pode criar outras tarefas. " +
			"Use para trabalho demorado (build, varredura, teste longo) sem travar a conversa; depois " +
			"consulte com " + prefixo + "_status e " + prefixo + "_result.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"prompt": map[string]any{"type": "string", "description": "instrução completa e autossuficiente para o sub-agente"},
			},
			"required": []string{"prompt"},
		},
		Exec: func(ctx context.Context, _ string, raw json.RawMessage) (string, error) {
			var c struct {
				Prompt string `json:"prompt"`
			}
			if err := json.Unmarshal(raw, &c); err != nil {
				return "", fmt.Errorf("entrada inválida: %w", err)
			}
			if strings.TrimSpace(c.Prompt) == "" {
				return "", fmt.Errorf("prompt obrigatório")
			}
			id, err := g.Spawn(ctx, c.Prompt)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("tarefa %s iniciada; use %s_status para acompanhar", id, prefixo), nil
		},
	}
}

// ── <prefixo>_status ───────────────────────────────────────────────────────

func especStatus(g *Gerente, prefixo string) tools.Spec {
	return tools.Spec{
		Name: prefixo + "_status",
		Description: "Lista as tarefas-irmãs com id, estado (queued/running/completed/failed/cancelled) " +
			"e idade. Não devolve resultado — para o texto use " + prefixo + "_result.",
		Schema: map[string]any{"type": "object", "properties": map[string]any{}},
		Exec: func(context.Context, string, json.RawMessage) (string, error) {
			lista := g.Tarefas()
			if len(lista) == 0 {
				return "nenhuma tarefa ainda", nil
			}
			agora := time.Now()
			var sb strings.Builder
			for i, t := range lista {
				fmt.Fprintf(&sb, "%d. %s %s %s\n", i+1, t.ID, t.Estado, idadeTexto(agora, t))
			}
			return strings.TrimRight(sb.String(), "\n"), nil
		},
	}
}

// idadeTexto resume o tempo relevante: para terminadas, desde a criação até o
// fim; para pendentes, desde a criação até agora.
func idadeTexto(agora time.Time, t *Tarefa) string {
	ref := t.TerminadaEm
	if ref.IsZero() {
		ref = agora
	}
	d := ref.Sub(t.Criada).Round(time.Second)
	if d < 0 {
		d = 0
	}
	return d.String()
}

// ── <prefixo>_result ───────────────────────────────────────────────────────

func especResult(g *Gerente, prefixo string) tools.Spec {
	return tools.Spec{
		Name: prefixo + "_result",
		Description: "Devolve o resultado FINAL de uma tarefa-irmã. completed: o texto do sub-agente " +
			"(cortado em 16 KiB); failed: uma linha com o erro; queued/running/cancelled: uma linha com o estado.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id": map[string]any{"type": "string", "description": "id da tarefa, como vem de " + prefixo + "_status"},
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
				return "", fmt.Errorf("id obrigatório")
			}
			cp, ok := g.snapshot(id)
			if !ok {
				return "", fmt.Errorf("tarefa %s não encontrada", id)
			}
			switch cp.Estado {
			case Completed:
				if strings.TrimSpace(cp.Resultado) == "" {
					return fmt.Sprintf("tarefa %s concluída sem texto final", id), nil
				}
				return cortar(cp.Resultado, TetoResultado, id), nil
			case Failed:
				return fmt.Sprintf("tarefa %s falhou: %s", id, ouVazio(cp.Erro)), nil
			case Cancelled:
				return fmt.Sprintf("tarefa %s cancelada%s", id, sufixo(cp.Erro)), nil
			case Queued:
				return fmt.Sprintf("tarefa %s na fila (aguardando vaga)", id), nil
			case Running:
				return fmt.Sprintf("tarefa %s rodando", id), nil
			default:
				return fmt.Sprintf("tarefa %s em estado %q", id, cp.Estado), nil
			}
		},
	}
}

// cortar aplica o teto com marcador no fim.
func cortar(s string, teto int, id string) string {
	if len(s) <= teto {
		return s
	}
	return s[:teto] + fmt.Sprintf("\n\n[resultado da tarefa %s cortado em %d bytes; peça um recorte mais específico]", id, teto)
}

func ouVazio(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(sem detalhe)"
	}
	return s
}

func sufixo(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	return ": " + s
}
