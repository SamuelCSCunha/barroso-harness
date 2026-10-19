// Package runtime implementa uma sessão de conversa reutilizável. O transcript
// só confirma uma rodada depois de uma resposta válida e, para tool_calls,
// depois de todos os resultados — erros/cancelamentos não deixam chamadas órfãs.
package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ruanbarroso/barroso-harness/internal/budget"
	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/dealer"
	"github.com/ruanbarroso/barroso-harness/internal/instructions"
	"github.com/ruanbarroso/barroso-harness/internal/store"
	"github.com/ruanbarroso/barroso-harness/internal/tools"
)

// Session guarda o transcript e estado reconfigurável de um REPL.
type Session struct {
	config  config.Config
	dealer  *dealer.Cliente
	history []dealer.Mensagem
	est     budget.Estimador
	key     string // calibração por (URL normalizada, model)

	// Persistência é OPCIONAL. store nil = sessão só em memória (o caminho de
	// NewSession, usado pelo modo -p e pelos testes). Quando há store, cada
	// evento confirmado do turno entra no log — e a sessão pode ser retomada
	// por Resume num processo novo.
	store     *store.Store
	sessionID string

	// injetadas são as specs de subsistemas EXTERNOS (sub-agentes, MCP),
	// acrescentadas pelo wiring com Injetar. Não vivem no catálogo estático
	// porque essas ferramentas têm dono de processo: ver extras.go.
	//
	// muExtras só protege estes dois campos. O mutex é fino de propósito — o
	// caminho quente (Send, montando a lista do turno) o segura só para
	// copiar o slice, e nunca enquanto fala com o provider.
	muExtras  sync.Mutex
	injetadas []tools.Spec
}

// NewSession cria uma sessão vazia sobre a configuração fornecida.
func NewSession(cfg config.Config) *Session {
	s := &Session{config: cfg}
	s.rebuild()
	return s
}

// NewSessionWithStore cria uma sessão JÁ persistente; o id só entra no log
// com o primeiro evento. É o caminho do REPL, que sabe de antemão onde o
// banco está.
func NewSessionWithStore(cfg config.Config, st *store.Store, sessionID string) *Session {
	s := &Session{config: cfg}
	s.rebuild()
	s.Attach(st, sessionID)
	return s
}

// Attach liga a sessão a um store existente. Pode ser chamado depois de
// NewSession (quando o banco só abre depois, ex.: falha best-effort no REPL);
// os eventos do próximo Send passam a ser gravados.
func (s *Session) Attach(st *store.Store, sessionID string) {
	if st == nil || strings.TrimSpace(sessionID) == "" {
		return
	}
	s.store = st
	s.sessionID = sessionID
	// Não indexa aqui: a linha da sessão nasce com o primeiro evento
	// (StoreEvent) ou com /rename. Abrir e fechar o bh sem conversar não
	// deixa sessão vazia no banco nem na lista do /resume.
}

// ID devolve o id da sessão (vazio quando não há persistência).
func (s *Session) ID() string { return s.sessionID }

// Persistente diz se há log gravando (usado pela CLI para mostrar `sess`).
func (s *Session) Persistente() bool { return s.store != nil && s.sessionID != "" }

func (s *Session) Config() config.Config { return s.config }

// Configure atualiza provider/model/effort, limpando tool state pendente (o
// transcript completo mantém somente turns confirmados; texto segue compatível).
func (s *Session) Configure(cfg config.Config) {
	if s.config.Model != cfg.Model || s.config.CurrentProvider != cfg.CurrentProvider {
		// Não há tool calls pendentes por causa da transação de Send; histórico
		// textual confirmado é válido entre modelos e providers.
	}
	s.config = cfg
	s.rebuild()
}

func (s *Session) rebuild() {
	p := s.config.Ativo()
	base, key := "", ""
	if p != nil {
		base, key = p.BaseURL, p.APIKey
	}
	id := dealer.NormalizarURL(base) + "\x00" + s.config.Model
	if id != s.key {
		s.est = budget.Estimador{}
		if s.config.BytesPorToken > 0 {
			s.est.CharsPorToken = float64(s.config.BytesPorToken)
		}
		s.key = id
	}
	s.dealer = dealer.Novo(dealer.Config{
		BaseURL: base, APIKey: key, Model: s.config.Model,
		MaxTokens: s.config.MaxTokens, JanelaTokens: s.config.Janela,
		Effort: s.config.Effort,
	})
}

// History devolve uma cópia para testes/observabilidade segura; não expõe o
// slice mutável interno.
func (s *Session) History() []dealer.Mensagem { return append([]dealer.Mensagem(nil), s.history...) }

// UltimoContexto devolve os prompt tokens do último request registrado no
// transcript (0 se não houver): o tamanho do contexto de uma sessão retomada,
// antes de qualquer turno novo.
func (s *Session) UltimoContexto() int {
	for i := len(s.history) - 1; i >= 0; i-- {
		if u := s.history[i].Usage; u != nil && u.PromptTokens > 0 {
			return u.PromptTokens
		}
	}
	return 0
}

// ClearHistory limpa o transcript. Usado só quando faz sentido iniciar uma
// nova conversa explicitamente; não é chamado ao trocar modelo.
//
// Com persistência, a quebra também entra no log (session_reset): o log é
// append-only e não pode "desfazer" os eventos anteriores, mas precisa
// registrar que a conversa foi cortada — senão um Resume montaria um
// transcript que o usuário acha que descartou.
func (s *Session) ClearHistory() {
	s.history = nil
	s.registrar("session_reset", map[string]any{"role": "system", "content": "história limpa; conversa nova"})
}

// Send executa um turno e mantém transcript entre chamadas. O append da
// mensagem do usuário e das rodadas fica num buffer privado e só é confirmado
// em sucesso completo; tool execution de YOLO já realizada não pode ser
// revertida, mas o histórico do provider nunca fica em estado malformado.
func (s *Session) Send(ctx context.Context, r Renderer, cwd, pedido string) error {
	if r == nil {
		r = RendererDe(&strings.Builder{})
	}
	if strings.TrimSpace(pedido) == "" {
		return fmt.Errorf("empty prompt")
	}
	if !s.config.Pronta() {
		return fmt.Errorf("incomplete configuration (%s); run `bh` with no arguments to set it up", strings.Join(s.config.Faltas(), ", "))
	}
	if ctx == nil {
		var cancelado context.CancelFunc
		ctx, cancelado = context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancelado()
	}
	inicio := time.Now()
	baroMD, err := instructions.Resolve(cwd)
	if err != nil {
		return err
	}
	working := append([]dealer.Mensagem(nil), s.history...)
	working = append(working, dealer.Mensagem{Role: "user", Conteudo: pedido})
	piso := 0
	reativaFeita := false
	compactou := false
	// Soma do turno inteiro: cada ciclo de tool gasta request e resposta, e o
	// rodapé da UI mostra o total, não o do último ciclo.
	usoPrompt, usoSaida := 0, 0

	// O pedido do usuário entra no log ANTES do loop: se o turno falhar no
	// meio, o log ainda registra o que foi pedido.
	s.registrar("user_message", map[string]any{"role": "user", "content": pedido})

	for ciclo := 0; ciclo < 64; ciclo++ {
		// Catálogo remontado a CADA ciclo: mcp_add, mcp_remove e skill_create
		// mudam as tools no meio do turno, e a próxima rodada já precisa ver.
		// Montado uma vez só, a tool recém-conectada ficava invisível até o
		// pedido seguinte — o modelo via o servidor em mcp_list e não podia
		// chamar a tool dele.
		lista := s.catalogoTools(cwd)
		schemas := toolSchemas(lista)
		system := "Você é o agente Barroso Harness, rodando localmente na máquina do usuário em modo YOLO: as ferramentas executam sem pedir aprovação.\n" +
			"Use as ferramentas para agir (terminal, arquivos) e TEXTO para falar com o usuário. Quando a tarefa terminar, responda com texto puro.\n"
		if baroMD != "" {
			system += "\n## BARROSO.md (instrução de sistema)\n" + baroMD
		}
		system += "\n## Ambiente\n- cwd: " + cwd + "\n- plataforma: use tools para descobrir; não presuma shell.\n"

		corpo, err := s.dealer.Corpo(system, working, schemas)
		if err != nil {
			return err
		}
		av := budget.Avaliar(string(corpo), s.config.Janela, s.est)
		if !av.Cabe {
			podado := podar(working, &piso, av.TokensEntrada+av.TokensSaida-s.config.Janela)
			if podado == nil {
				return fmt.Errorf("guardrail: request does not fit (%s) and pruning did not free enough", av.Veredito())
			}
			r.Guardrail(true)
			s.registrar("guardrail", map[string]any{
				"tipo":       "proativa",
				"politica":   politica(s.config.PoliticaContexto),
				"veredito":   av.Veredito(),
				"janela":     s.config.Janela,
				"estimativa": av.TokensEntrada + av.TokensSaida,
			})
			compactou = true
			working = podado
			continue
		}

		if rd, ok := r.(RendererDetalhado); ok {
			rd.Pensando()
		}
		rodada, err := s.dealer.RodarContext(ctx, system, working, schemas)
		if err != nil {
			if estouraContexto(err) && s.config.Janela > 0 && !reativaFeita {
				reativaFeita = true
				r.Guardrail(false)
				s.registrar("guardrail", map[string]any{
					"tipo":     "reativa",
					"politica": politica(s.config.PoliticaContexto),
					"motivo":   umaLinha(err.Error()),
				})
				podado := podar(working, &piso, budget.ReservaOperacional)
				if podado != nil {
					compactou = true
					working = podado
					continue
				}
			}
			return err
		}
		if rodada.Usage != nil {
			if rd, ok := r.(RendererDetalhado); ok {
				rd.Contexto(rodada.Usage.PromptTokens)
			}
			s.est.Calibrar(len(corpo), rodada.Usage.PromptTokens)
			if rodada.Usage.PromptTokens > 0 {
				usoPrompt += rodada.Usage.PromptTokens
			}
			usoSaida += rodada.Usage.OutputTokens
		}
		if len(rodada.Tools) == 0 {
			r.Resposta(rodada.Texto)
			r.Tokens(usoPrompt, usoSaida, time.Since(inicio), compactou)
			working = append(working, dealer.Mensagem{Role: "assistant", Conteudo: rodada.Texto, Usage: rodada.Usage})
			s.history = working
			// Texto confirmado entra no log DEPOIS da confirmação do transcript:
			// uma rodada que falhou não deixa rastro de resposta que não existiu.
			assistente := map[string]any{"role": "assistant", "content": rodada.Texto}
			if rodada.Usage != nil {
				assistente["usage"] = map[string]any{
					"prompt_tokens": rodada.Usage.PromptTokens,
					"output_tokens": rodada.Usage.OutputTokens,
				}
			}
			s.registrar("assistant", assistente)
			return nil
		}

		// Monta assistant tool-call + resultados numa transação separada; só
		// adiciona à working quando todas as execuções terminaram.
		turn := make([]dealer.Mensagem, 0, len(rodada.Tools)+1)
		if rodada.Texto != "" {
			turn = append(turn, dealer.Mensagem{Role: "assistant", Conteudo: rodada.Texto, Usage: rodada.Usage})
		}
		results := make([]dealer.Mensagem, 0, len(rodada.Tools))
		for _, call := range rodada.Tools {
			turn = append(turn, dealer.Mensagem{Role: "tool_call", ToolID: call.ID, ToolName: call.Nome, ToolArgs: call.Args})
			// A chamada entra no log aqui, antes de executar: se a execução
			// travar ou estourar, o log ainda mostra o que foi pedido e com
			// quais argumentos.
			s.registrar("tool_call", map[string]any{
				"role":      "tool_call",
				"tool_id":   call.ID,
				"tool_name": call.Nome,
				"tool_args": call.Args,
			})
		}
		for _, call := range rodada.Tools {
			if rd, ok := r.(RendererDetalhado); ok {
				rd.ToolInicio(call.Nome, call.Args)
			}
			inicio := time.Now()
			saida, exeErr := ferramenta(ctx, lista, cwd, call)
			dur := time.Since(inicio)
			isErr := exeErr != nil
			if isErr {
				saida = "erro: " + exeErr.Error()
			}
			results = append(results, dealer.Mensagem{Role: "tool_result", Conteudo: saida, ToolID: call.ID, ToolName: call.Nome, IsErro: isErr})
			// Só grava o resultado quando o turno foi confirmado (a transação
			// fecha logo abaixo, com working recebendo turn+results). Saídas
			// acima de limiteBlob viram blob + preview.
			s.registrarResultado(call.ID, call.Nome, saida, isErr)
			r.Tool(call.Nome, dur, isErr)
			r.Eco(resultadoSaida(saida))
		}
		working = append(working, turn...)
		working = append(working, results...)
		if estado := estadoCompactado(rodada.Tools, results); estado != "" {
			antes := bytesHistorico(working)
			working = compactarHistorico(working, estado)
			piso = 0 // o histórico mudou de forma; a poda recomeça do início
			compactou = true
			r.Guardrail(true)
			s.registrar("compactacao", map[string]any{
				"politica":     PoliticaAgente,
				"ciclo":        ciclo,
				"bytes_antes":  antes,
				"bytes_depois": bytesHistorico(working),
				"estado_bytes": len(estado),
				"estado":       estado,
			})
		}
	}
	return fmt.Errorf("turn exceeded 64 cycles; configurable technical limit (not a permission check)")
}

func toolSchemas(lista []tools.Spec) []map[string]any {
	schemas := make([]map[string]any, 0, len(lista))
	for _, s := range lista {
		schemas = append(schemas, map[string]any{
			"type":     "function",
			"function": map[string]any{"name": s.Name, "description": s.Description, "parameters": s.Schema},
		})
	}
	return schemas
}

func ferramenta(ctx context.Context, lista []tools.Spec, cwd string, call dealer.ToolChamada) (string, error) {
	for _, s := range lista {
		if s.Name == call.Nome {
			return s.Exec(ctx, cwd, json.RawMessage(call.Args))
		}
	}
	return "", fmt.Errorf("tool desconhecida: %q (checar o nome exato)", call.Nome)
}
