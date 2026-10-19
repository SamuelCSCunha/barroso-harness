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

// Especificacoes devolve a tool de delegação do modelo: <prefixo>_spawn
// {prompts[]}, que dispara os sub-agentes e volta na hora. O modelo NÃO recebe
// <prefixo>_status nem <prefixo>_result: com elas à mão ele ficava consultando
// o status em loop em vez de seguir ou encerrar o turno. O resultado de cada
// sub-agente chega sozinho (AvisoTerminadas); o usuário acompanha por /tasks.
// especStatus e especResult ficam para os testes do ciclo de vida.
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
	return []tools.Spec{especSpawn(g, p)}
}

// ── <prefixo>_spawn ────────────────────────────────────────────────────────

func especSpawn(g *Gerente, prefixo string) tools.Spec {
	return tools.Spec{
		Name: prefixo + "_spawn",
		Description: "Dispara um ou VÁRIOS sub-agentes de uma vez, em SEGUNDO PLANO, e devolve os ids na hora. " +
			"Cada sub-agente roda autonomamente com as ferramentas built-in (terminal, arquivos), mas NÃO pode " +
			"criar outros sub-agentes. Não há limite de quantidade nem de concorrência: todos começam juntos. " +
			"Não existe tool para consultar ou esperar: quando cada sub-agente terminar ou falhar, o resultado chega " +
			"sozinho como uma mensagem nova, num turno novo. Depois de disparar, siga com outro trabalho ou ENCERRE " +
			"O TURNO dizendo ao usuário o que disparou e que está aguardando os retornos.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"prompts": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "uma instrução completa e autossuficiente por sub-agente; N itens = N sub-agentes em paralelo",
				},
				"prompt": map[string]any{"type": "string", "description": "atalho para um sub-agente só"},
			},
		},
		Exec: func(ctx context.Context, _ string, raw json.RawMessage) (string, error) {
			var c struct {
				Prompt  string   `json:"prompt"`
				Prompts []string `json:"prompts"`
			}
			if err := json.Unmarshal(raw, &c); err != nil {
				return "", fmt.Errorf("entrada inválida: %w", err)
			}
			var pedidos []string
			if strings.TrimSpace(c.Prompt) != "" {
				pedidos = append(pedidos, c.Prompt)
			}
			for _, p := range c.Prompts {
				if strings.TrimSpace(p) != "" {
					pedidos = append(pedidos, p)
				}
			}
			if len(pedidos) == 0 {
				return "", fmt.Errorf("informe prompts (ou prompt)")
			}
			var ids []string
			for _, p := range pedidos {
				// ctx nil: a tarefa vive no contexto longo do gerente, não no
				// do turno — o turno pode acabar e o sub-agente continua.
				id, err := g.Spawn(nil, p)
				if err != nil {
					return "", err
				}
				ids = append(ids, id)
			}
			return fmt.Sprintf("%d sub-agente(s) disparado(s): %s. Eles rodam em segundo plano e cada resultado "+
				"chega sozinho como mensagem nova quando terminar. Não espere: siga com outro trabalho ou encerre o "+
				"turno agora avisando o usuário.", len(ids), strings.Join(ids, ", ")), nil
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
			g.marcarEntregue(id)
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
				return fmt.Sprintf("tarefa %s iniciando", id), nil
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

// AvisoTerminadas monta a mensagem que o pai recebe quando sub-agentes
// terminam: um bloco por tarefa com o resultado (ou o erro) e, no fim,
// quantas ainda estão rodando. O prefixo "[subagents]" deixa a tela e o
// título da sessão reconhecerem a mensagem como aviso, não fala do usuário.
func AvisoTerminadas(ts []Tarefa, ativas int) string {
	var sb strings.Builder
	sb.WriteString("[subagents] Sub-agentes terminaram:\n")
	for i := range ts {
		t := &ts[i]
		dur := t.TerminadaEm.Sub(t.Criada).Round(time.Second)
		fmt.Fprintf(&sb, "\n## %s %s (%s)\n", t.ID, t.Estado, dur)
		switch t.Estado {
		case Completed:
			if strings.TrimSpace(t.Resultado) == "" {
				sb.WriteString("(sem texto final)\n")
			} else {
				sb.WriteString(cortar(t.Resultado, TetoResultado, t.ID) + "\n")
			}
		default:
			sb.WriteString("erro: " + ouVazio(t.Erro) + "\n")
		}
	}
	if ativas > 0 {
		fmt.Fprintf(&sb, "\n%d sub-agente(s) ainda rodando; os resultados chegam do mesmo jeito.\n", ativas)
	} else {
		sb.WriteString("\nNenhum sub-agente rodando agora.\n")
	}
	return sb.String()
}
