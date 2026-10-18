package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ruanbarroso/barroso-harness/internal/dealer"
	"github.com/ruanbarroso/barroso-harness/internal/tools"
)

// Políticas de contexto (config.json, campo context_policy).
//
//   - PoliticaAgente (padrão): o modelo tem ctx_compact e decide QUANDO
//     compactar, num marco da tarefa, escrevendo o estado de trabalho. O
//     guardrail por orçamento continua ligado como rede de segurança.
//   - PoliticaLimiar: sem ctx_compact; só o guardrail compacta, ao estourar.
//
// As duas gravam cada compactação no log (evento "compactacao" ou
// "guardrail", ambos com o campo politica), para poderem ser comparadas sob o
// mesmo harness com as sessões reais em sessions.db.
const (
	PoliticaAgente = "agente"
	PoliticaLimiar = "limiar"
)

// politica normaliza o valor da config; desconhecido cai no padrão.
func politica(v string) string {
	if strings.EqualFold(strings.TrimSpace(v), PoliticaLimiar) {
		return PoliticaLimiar
	}
	return PoliticaAgente
}

const nomeCompactar = "ctx_compact"

// tetoResumo limita o estado escrito pelo modelo: um resumo maior que isso
// deixa de ser compactação.
const tetoResumo = 16 << 10

// specCompactar é a ação de compactação do agente. O Exec só valida; quem
// troca o histórico é o laço do turno, depois que os resultados da rodada
// entram (o histórico não pode mudar no meio de uma rodada de tools).
func specCompactar() tools.Spec {
	return tools.Spec{
		Name: nomeCompactar,
		Description: "Compacta o histórico desta conversa: tudo ANTES do pedido atual é substituído pelo " +
			"estado de trabalho que você escrever. Use num marco natural da tarefa (ex.: achou a causa e " +
			"vai editar; terminou a exploração) — não espere o contexto encher. O estado precisa bastar " +
			"para continuar: objetivo, decisões, fatos descobertos (caminhos, assinaturas, valores), o que " +
			"já foi alterado e o próximo passo. O log completo continua recuperável por session_search.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"estado": map[string]any{"type": "string", "description": "estado de trabalho estruturado que substitui o histórico anterior"},
			},
			"required": []string{"estado"},
		},
		Exec: func(_ context.Context, _ string, raw json.RawMessage) (string, error) {
			estado, err := lerEstadoCompactar(raw)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("compactação agendada: o histórico anterior a este pedido será trocado pelo estado (%d caracteres) ao fim desta rodada", len(estado)), nil
		},
	}
}

func lerEstadoCompactar(raw json.RawMessage) (string, error) {
	var c struct {
		Estado string `json:"estado"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return "", fmt.Errorf("entrada inválida: %w", err)
	}
	e := strings.TrimSpace(c.Estado)
	if e == "" {
		return "", fmt.Errorf("estado vazio: escreva o estado de trabalho que substitui o histórico")
	}
	if len(e) > tetoResumo {
		return "", fmt.Errorf("estado com %d bytes passa do teto de %d: resuma mais", len(e), tetoResumo)
	}
	return e, nil
}

// prefixoResumo marca a mensagem sintética; Resume e as compactações seguintes
// a reconhecem por ele.
const prefixoResumo = "[estado de trabalho — escrito pelo próprio agente ao compactar o histórico anterior]\n"

// compactarHistorico troca tudo antes do pedido ATUAL (a última mensagem de
// usuário real) por uma mensagem com o estado. O pedido atual e a rodada em
// curso — com os pares tool_call/tool_result — ficam intactos, então o
// request continua válido para o provider.
//
// Sem nada antes do pedido atual, não há o que compactar e h volta igual.
func compactarHistorico(h []dealer.Mensagem, estado string) []dealer.Mensagem {
	corte := -1
	for i := len(h) - 1; i >= 0; i-- {
		if h[i].Role == "user" && !strings.HasPrefix(h[i].Conteudo, prefixoResumo) {
			corte = i
			break
		}
	}
	if corte <= 0 {
		return h
	}
	out := make([]dealer.Mensagem, 0, len(h)-corte+1)
	out = append(out, dealer.Mensagem{Role: "user", Conteudo: prefixoResumo + estado})
	return append(out, h[corte:]...)
}

func bytesHistorico(h []dealer.Mensagem) int {
	n := 0
	for _, m := range h {
		n += len(m.Conteudo) + len(m.ToolArgs)
	}
	return n
}

// estadoCompactado devolve o estado da última ctx_compact BEM-SUCEDIDA da
// rodada, ou "" quando não houve. Chamada que falhou (estado vazio, teto) não
// compacta nada: o modelo recebeu o erro e pode tentar de novo.
func estadoCompactado(calls []dealer.ToolChamada, results []dealer.Mensagem) string {
	estado := ""
	for i, c := range calls {
		if c.Nome != nomeCompactar || i >= len(results) || results[i].IsErro {
			continue
		}
		if e, err := lerEstadoCompactar(json.RawMessage(c.Args)); err == nil {
			estado = e
		}
	}
	return estado
}
