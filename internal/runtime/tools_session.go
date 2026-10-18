// Tools de recuperação do log da sessão. Vivem no runtime, e não no pacote
// tools, porque fecham sobre a Sessão: precisam do store para ler o log, e o
// pacote tools não pode importar o runtime (seria ciclo). Send anexa estas
// specs às built-in antes de montar os schemas, então o modelo as vê como
// qualquer outra ferramenta.
//
// O problema que resolvem: o guardrail compacta resultados grandes de tool
// para o turno caber na janela, e o transcript manda o resto embora. Sem
// recuperação, o conteúdo some — o agente fica sabendo que existiu algo ali
// sem poder ler de novo. Estas duas tools dão o caminho de volta.
package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ruanbarroso/barroso-harness/internal/tools"
)

const limitePadraoBusca = 10
const tetoPadraoRetrieve = 8 << 10 // 8 KiB: retrieve não pode estourar a janela

// SessaoSpecs devolve as tools de sessão para a Sessão dada. Sem persistência
// elas continuam na lista (o schema é o mesmo), mas devolvem uma mensagem
// explicando que o log não está ativo — melhor que a tool não existir e o
// modelo tentar de novo.
func SessaoSpecs(s *Session) []tools.Spec {
	return []tools.Spec{
		specSessaoBusca(s),
		specSessaoRetrieve(s),
	}
}

func specSessaoBusca(s *Session) tools.Spec {
	return tools.Spec{
		Name: "session_search",
		Description: "Procura no histórico da sessão atual (substring, ignora maiúsculas) e devolve " +
			"os trechos que casam, com o número do evento (seq). Use para reencontrar conteúdo " +
			"que a compactação do guardrail tirou da janela ou que saiu do transcript: saídas " +
			"grandes de tool, decisões antigas, caminhos já lidos. Combinado com session_retrieve " +
			"recupera o texto inteiro. Só enxerga esta sessão.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string", "description": "texto a procurar (substring, sem distinguir maiúsculas)"},
				"limit": map[string]any{"type": "integer", "description": "quantos eventos devolver; padrão 10"},
			},
			"required": []string{"query"},
		},
		Exec: func(_ context.Context, _ string, raw json.RawMessage) (string, error) {
			var c struct {
				Query string `json:"query"`
				Limit int    `json:"limit"`
			}
			if err := json.Unmarshal(raw, &c); err != nil {
				return "", fmt.Errorf("entrada inválida: %w", err)
			}
			if strings.TrimSpace(c.Query) == "" {
				return "", fmt.Errorf("query vazio")
			}
			if !s.Persistente() {
				return "(sessão sem persistência: o log não está gravando, não há o que procurar)", nil
			}
			limite := c.Limit
			if limite <= 0 {
				limite = limitePadraoBusca
			}
			hits := s.Search(c.Query, limite)
			if len(hits) == 0 {
				return fmt.Sprintf("nenhum trecho com %q no histórico desta sessão", c.Query), nil
			}
			var sb strings.Builder
			fmt.Fprintf(&sb, "%d trecho(s) com %q — use session_retrieve com o seq para o texto inteiro:\n", len(hits), c.Query)
			for _, h := range hits {
				fmt.Fprintf(&sb, "seq %d [%s] %s\n", h.Seq, h.Kind, h.Snippet)
			}
			return sb.String(), nil
		},
	}
}

func specSessaoRetrieve(s *Session) tools.Spec {
	return tools.Spec{
		Name: "session_retrieve",
		Description: "Devolve UM evento do histórico da sessão atual pelo número (seq), com o " +
			"conteúdo completo — inclusive saídas de tool grandes que foram arquivadas em blob " +
			"durante a compactação. O seq vem de session_search. Use quando um trecho não bastar " +
			"e você precisar do texto inteiro de volta.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"seq":   map[string]any{"type": "integer", "description": "número do evento (seq), como devolvido por session_search"},
				"limit": map[string]any{"type": "integer", "description": "teto de bytes do conteúdo devolvido; padrão 8192"},
			},
			"required": []string{"seq"},
		},
		Exec: func(_ context.Context, _ string, raw json.RawMessage) (string, error) {
			var c struct {
				Seq   int `json:"seq"`
				Limit int `json:"limit"`
			}
			if err := json.Unmarshal(raw, &c); err != nil {
				return "", fmt.Errorf("entrada inválida: %w", err)
			}
			if !s.Persistente() {
				return "(sessão sem persistência: o log não está gravando, não há evento para recuperar)", nil
			}
			if c.Seq <= 0 {
				return "", fmt.Errorf("seq inválido: %d", c.Seq)
			}
			limite := c.Limit
			if limite <= 0 {
				limite = tetoPadraoRetrieve
			}
			e, err := s.Retrieve(c.Seq)
			if err != nil {
				return "", fmt.Errorf("evento %d não recuperado: %w", c.Seq, err)
			}
			var sb strings.Builder
			fmt.Fprintf(&sb, "seq %d [%s] %s\n", e.Seq, e.Kind, e.Ts)
			corpo := e.Payload
			if len(corpo) > limite {
				corpo = corpo[:limite] + fmt.Sprintf("\n\n[cortado em %d de %d bytes; peça um limit maior]", limite, len(e.Payload))
			}
			sb.WriteString(corpo)
			return sb.String(), nil
		},
	}
}
