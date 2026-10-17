// Package runtime implementa uma sessão de conversa reutilizável. O transcript
// só confirma uma rodada depois de uma resposta válida e, para tool_calls,
// depois de todos os resultados — erros/cancelamentos não deixam chamadas órfãs.
package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ruanbarroso/barroso-harness/internal/budget"
	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/dealer"
	"github.com/ruanbarroso/barroso-harness/internal/instructions"
	"github.com/ruanbarroso/barroso-harness/internal/tools"
)

// Session guarda o transcript e estado reconfigurável de um REPL.
type Session struct {
	config  config.Config
	dealer  *dealer.Cliente
	history []dealer.Mensagem
	est     budget.Estimador
	key     string // calibração por (URL normalizada, model)
}

// NewSession cria uma sessão vazia sobre a configuração fornecida.
func NewSession(cfg config.Config) *Session {
	s := &Session{config: cfg}
	s.rebuild()
	return s
}

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

// ClearHistory limpa o transcript. Usado só quando faz sentido iniciar uma
// nova conversa explicitamente no futuro; não é chamado ao trocar modelo.
func (s *Session) ClearHistory() { s.history = nil }

// Send executa um turno e mantém transcript entre chamadas. O append da
// mensagem do usuário e das rodadas fica num buffer privado e só é confirmado
// em sucesso completo; tool execution de YOLO já realizada não pode ser
// revertida, mas o histórico do provider nunca fica em estado malformado.
func (s *Session) Send(ctx context.Context, out io.Writer, cwd, pedido string) error {
	if strings.TrimSpace(pedido) == "" {
		return fmt.Errorf("pedido vazio")
	}
	if !s.config.Pronta() {
		return fmt.Errorf("configuração incompleta (%s); execute `bh` sem argumentos para configurar", strings.Join(s.config.Faltas(), ", "))
	}
	if ctx == nil {
		var cancelado context.CancelFunc
		ctx, cancelado = context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancelado()
	}
	baroMD, err := instructions.Resolve(cwd)
	if err != nil {
		return err
	}
	lista := tools.SpecsBuiltins()
	schemas := toolSchemas(lista)
	working := append([]dealer.Mensagem(nil), s.history...)
	working = append(working, dealer.Mensagem{Role: "user", Conteudo: pedido})
	piso := 0
	reativaFeita := false

	for ciclo := 0; ciclo < 64; ciclo++ {
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
				return fmt.Errorf("guardrail: request não cabe (%s) e a poda não aliviou o suficiente", av.Veredito())
			}
			fmt.Fprintln(out, "[guardrail] compactação proativa por previsão de estouro")
			working = podado
			continue
		}

		rodada, err := s.dealer.RodarContext(ctx, system, working, schemas)
		if err != nil {
			if estouraContexto(err) && s.config.Janela > 0 && !reativaFeita {
				reativaFeita = true
				fmt.Fprintln(out, "[guardrail] provider recusou por contexto; compactação reativa")
				podado := podar(working, &piso, budget.ReservaOperacional)
				if podado != nil {
					working = podado
					continue
				}
			}
			return err
		}
		if rodada.Usage != nil {
			s.est.Calibrar(len(corpo), rodada.Usage.PromptTokens)
		}
		if len(rodada.Tools) == 0 {
			if rodada.Texto != "" {
				fmt.Fprint(out, rodada.Texto)
			}
			fmt.Fprintln(out)
			working = append(working, dealer.Mensagem{Role: "assistant", Conteudo: rodada.Texto, Usage: rodada.Usage})
			s.history = working
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
		}
		for _, call := range rodada.Tools {
			inicio := time.Now()
			saida, exeErr := ferramenta(ctx, lista, cwd, call)
			dur := time.Since(inicio)
			isErr := exeErr != nil
			if isErr {
				saida = "erro: " + exeErr.Error()
			}
			results = append(results, dealer.Mensagem{Role: "tool_result", Conteudo: saida, ToolID: call.ID, ToolName: call.Nome, IsErro: isErr})
			estado := "ok"
			if isErr {
				estado = "erro"
			}
			fmt.Fprintf(out, "[tool %s %s em %s]\n", call.Nome, estado, dur.Round(time.Millisecond))
			if eco := resultadoSaida(saida); eco != "" {
				fmt.Fprintln(out, eco)
			}
		}
		working = append(working, turn...)
		working = append(working, results...)
	}
	return fmt.Errorf("turno excedeu 64 ciclos; teto técnico configurável (não é permissionamento)")
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
