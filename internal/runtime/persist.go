// Persistência da sessão: a ponte entre o log append-only (internal/store) e o
// transcript em memória ([ ]dealer.Mensagem). O log guarda o que aconteceu; a
// memória é derivada dele — nunca o contrário. Send grava; Resume reconstrói.
package runtime

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/ruanbarroso/barroso-harness/internal/dealer"
	"github.com/ruanbarroso/barroso-harness/internal/store"
)

// Tetos do log. O limite de 16 KiB no resultado de tool é o que mantém o
// arquivo curto sem perder conteúdo: acima dele o resultado vai para `blobs`
// e o evento carrega só o preview + a referência.
const (
	limiteBlob   = 16 << 10 // 16 KiB: acima disso, tool_result vira blob
	previewBytes = 512      // quanto do resultado fica no preview
)

// registrar grava um evento no log. Silencioso quando não há persistência: a
// sessão em memória continua funcionando sem banco (é o modo -p e os testes).
//
// Falha de gravação NÃO aborta o turno. Razão: o log é observabilidade, não
// autorização — perder uma linha porque o disco encheu não pode derrubar uma
// conversa que está rodando. O custo é um histórico incompleto, reversível.
func (s *Session) registrar(kind string, payload map[string]any) {
	if !s.Persistente() || payload == nil {
		return
	}
	_, _ = s.store.StoreEvent(s.sessionID, kind, payload)
}

// registrarResultado grava um tool_result, deslocando para blob o que passar
// de limiteBlob. Devolve o payload gravado (útil para teste e inspeção).
func (s *Session) registrarResultado(callID, nome, saida string, isErr bool) map[string]any {
	payload := map[string]any{
		"role":      "tool_result",
		"tool_id":   callID,
		"tool_name": nome,
		"is_erro":   isErr,
	}
	if len(saida) > limiteBlob {
		if ref, err := s.store.PutBlob([]byte(saida)); err == nil {
			for k, v := range ref {
				payload[k] = v
			}
			payload["preview"] = cortarPrefixo(saida, previewBytes)
			payload["truncated"] = true
		} else {
			// Sem blob (falha de gravação), o preview é o que fica registrado.
			payload["content"] = cortarPrefixo(saida, previewBytes)
			payload["truncated"] = true
		}
	} else {
		payload["content"] = saida
	}
	s.registrar("tool_result", payload)
	return payload
}

// cortarPrefixo devolve os primeiros n bytes sem quebrar uma runa UTF-8 no
// meio: preview com bytes pendurados viraria "?" na leitura e no log.
func cortarPrefixo(s string, n int) string {
	if len(s) <= n {
		return s
	}
	corte := s[:n]
	// Recua enquanto o byte anterior for de continuação (10xxxxxx): significa
	// que cortamos dentro de uma runa multibyte.
	for len(corte) > 0 && !utf8.RuneStart(corte[len(corte)-1]) {
		corte = corte[:len(corte)-1]
	}
	return corte
}

// ── reconstrução ──────────────────────────────────────────────────────────

// Resume carrega os eventos da sessão e remonta s.history no mesmo formato
// []dealer.Mensagem que o loop de Send já usa. Resultados de tool que foram
// para blob são reidratados aqui — o transcript volta completo.
//
// Sem eventos, history fica vazia (sessão nova ou id errado): não é erro,
// é apenas uma conversa começando.
func (s *Session) Resume() error {
	if !s.Persistente() {
		return nil
	}
	eventos, err := s.store.ListEvents(s.sessionID, 0, 0)
	if err != nil {
		return err
	}
	s.history = eventosParaMensagens(s.store, eventos)
	return nil
}

// eventosParaMensagens traduz o log para o transcript. Só eventos de conteúdo
// entram: user_message, assistant, tool_call, tool_result. Os demais
// (guardrail, session_reset, metadado) são observabilidade, não transcript.
func eventosParaMensagens(st *store.Store, eventos []store.Event) []dealer.Mensagem {
	var out []dealer.Mensagem
	for _, e := range eventos {
		switch e.Kind {
		case "user_message", "assistant", "tool_call", "tool_result":
		default:
			continue
		}
		var p struct {
			Role     string `json:"role"`
			Content  string `json:"content"`
			ToolID   string `json:"tool_id"`
			ToolName string `json:"tool_name"`
			ToolArgs string `json:"tool_args"`
			IsErro   bool   `json:"is_erro"`
			Blob     string `json:"blob"`
			Preview  string `json:"preview"`
			Usage    *struct {
				PromptTokens int `json:"prompt_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(e.Payload), &p); err != nil {
			continue // evento ilegível não pode corromper o transcript
		}
		m := dealer.Mensagem{
			Role:     p.Role,
			Conteudo: p.Content,
			ToolID:   p.ToolID,
			ToolName: p.ToolName,
			ToolArgs: p.ToolArgs,
			IsErro:   p.IsErro,
		}
		if p.Usage != nil {
			m.Usage = &dealer.Usage{
				PromptTokens: p.Usage.PromptTokens,
				OutputTokens: p.Usage.OutputTokens,
			}
		}
		// Reidratação: conteúdo grande ficou no blob, o evento tem a referência.
		if p.Blob != "" {
			m.Conteudo = reidratar(st, p.Blob, p.Preview)
		}
		out = append(out, m)
	}
	return out
}

// reidratar devolve o conteúdo completo de um tool_result arquivado. O
// preview NÃO é somado: ele é o começo do próprio blob, e concatená-los
// repetiria os primeiros bytes. Sem blob (banco sem os dados), o preview é o
// que sobrou — e é melhor que nada.
func reidratar(st *store.Store, hash, preview string) string {
	if st == nil || hash == "" {
		return preview
	}
	dados, err := st.GetBlob(hash)
	if err != nil {
		return preview
	}
	return string(dados)
}

// umaLinha colapsa um texto em uma única linha: o log é uma linha por evento,
// e mensagem de provider costuma ter newlines no meio.
func umaLinha(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	const teto = 240
	if len(s) > teto {
		s = s[:teto] + "…"
	}
	return s
}

// ── busca e recuperação ───────────────────────────────────────────────────

// Buscar procura nos eventos da sessão (substring, sem distinguir maiúscula)
// e devolve trechos com contexto — é o que recupera conteúdo que o guardrail
// compactou ou que saiu da janela do modelo.
func (s *Session) Search(query string, limit int) []store.Hit {
	if !s.Persistente() || strings.TrimSpace(query) == "" {
		return nil
	}
	hits, err := s.store.Buscar(s.sessionID, query, limit)
	if err != nil {
		return nil
	}
	return hits
}

// Retrieve devolve UM evento com o payload completo (blob reidratado), para o
// modelo recuperar a saída inteira de uma tool arquivada.
func (s *Session) Retrieve(seq int) (store.Event, error) {
	if !s.Persistente() {
		return store.Event{}, store.ErrSessaoInexistente
	}
	e, err := s.store.GetEvent(s.sessionID, seq)
	if err != nil {
		return store.Event{}, err
	}
	cheio, err := s.store.Rehydrate(e.Payload)
	if err != nil {
		return *e, nil
	}
	e.Payload = cheio
	return *e, nil
}
