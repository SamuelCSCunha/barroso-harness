// Package runtime compõe o turno: instruções BARROSO.md → histórico →
// guardrail → loop do modelo → tools YOLO → compactação proativa/reativa.
// O transcript é append-only dentro do processo; persistência em disco
// (sessões) entra no slice 1.
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

// Turno executa um pedido: streaming de texto na saída, tools em modo YOLO,
// guardrail de janela antes de cada chamada e recuperação reativa limitada.
func Turno(out io.Writer, cfg config.Config, cwd, pedido string) error {
	if strings.TrimSpace(pedido) == "" {
		return fmt.Errorf("pedido vazio: use -p \"...\"")
	}
	baroMD, err := instructions.Resolve(cwd)
	if err != nil {
		return err
	}

	dc := dealer.Novo(dealer.Config{
		BaseURL:      cfg.BaseURL,
		Model:        cfg.Model,
		APIKey:       cfg.APIKey,
		MaxTokens:    cfg.MaxTokens,
		JanelaTokens: cfg.Janela,
	})
	lista := tools.SpecsBuiltins()
	schemas := make([]map[string]any, 0, len(lista))
	for _, s := range lista {
		schemas = append(schemas, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        s.Name,
				"description": s.Description,
				"parameters":  s.Schema,
			},
		})
	}

	historico := []dealer.Mensagem{{Role: "user", Conteudo: pedido}}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	est := budget.Estimador{}
	if cfg.BytesPorToken > 0 {
		est.CharsPorToken = float64(cfg.BytesPorToken)
	}

	piso := 0               // poda não desce abaixo deste índice (monotônica no turno)
	reativaFeita := false   // compactação reativa: uma tentativa por turno

	for ciclo := 0; ciclo < 64; ciclo++ {
		system := "Você é o agente Barroso Harness, rodando localmente na máquina do usuário em modo YOLO: as ferramentas executam sem pedir aprovação.\n" +
			"Use as ferramentas para agir (terminal, arquivos) e TEXTO para falar com o usuário. Quando a tarefa terminar, responda com texto puro.\n"
		if baroMD != "" {
			system += "\n## BARROSO.md (instrução de sistema)\n" + baroMD
		}
		system += "\n## Ambiente\n- cwd: " + cwd + "\n- plataforma: use tools para descobrir; não presuma shell.\n"

		// ── guardrail de capacidade ──────────────────────────────
		corpo, err := dc.Corpo(system, historico, schemas)
		if err != nil {
			return err
		}
		av := budget.Avaliar(string(corpo), cfg.Janela, est)
		if !av.Cabe {
			podado := podar(historico, &piso, av.TokensEntrada+av.TokensSaida-cfg.Janela)
			if podado == nil {
				return fmt.Errorf("guardrail: request não cabe (%s) e a poda não aliviou o suficiente", av.Veredito())
			}
			fmt.Fprintln(out, "[guardrail] compactação proativa por previsão de estouro")
			historico = podado
			continue // remonta e mede de novo
		}

		// ── rodada do modelo ─────────────────────────────────────
		rodada, err := dc.Rodar(system, historico, schemas)
		if err != nil {
			if estouraContexto(err) && cfg.Janela > 0 && !reativaFeita {
				reativaFeita = true
				fmt.Fprintln(out, "[guardrail] provider recusou por contexto; compactação reativa")
				podado := podar(historico, &piso, budget.ReservaOperacional)
				if podado != nil {
					historico = podado
					continue
				}
			}
			return err
		}
		if rodada.Usage != nil {
			est.Calibrar(len(corpo), rodada.Usage.PromptTokens)
		}

		if rodada.Texto != "" && len(rodada.Tools) > 0 {
			// texto+preamble antes das tools vai como assistant content
			historico = append(historico, dealer.Mensagem{Role: "assistant", Conteudo: rodada.Texto})
		}
		if len(rodada.Tools) == 0 {
			if rodada.Texto != "" {
				fmt.Fprint(out, rodada.Texto)
			}
			fmt.Fprintln(out)
			return nil
		}

		for _, call := range rodada.Tools {
			historico = append(historico, dealer.Mensagem{
				Role: "tool_call", ToolID: call.ID, ToolName: call.Nome, ToolArgs: call.Args,
			})
		}

		// ── execução YOLO das tools ──────────────────────────────
		for _, call := range rodada.Tools {
			inicio := time.Now()
			saida, exeErr := ferramenta(ctx, lista, cwd, call)
			dur := time.Since(inicio)
			isErr := exeErr != nil
			if isErr {
				saida = "erro: " + exeErr.Error()
			}
			historico = append(historico, dealer.Mensagem{
				Role: "tool_result", Conteudo: saida,
				ToolID: call.ID, ToolName: call.Nome, IsErro: isErr,
			})
			estado := "ok"
			if isErr {
				estado = "erro"
			}
			fmt.Fprintf(out, "[tool %s %s em %s]\n", call.Nome, estado, dur.Round(time.Millisecond))
			// trecho visível do resultado para o humano acompanhar
			// (o histórico do modelo já recebeu tudo).
			eco := resultadoSaida(saida)
			if eco != "" {
				fmt.Fprintln(out, eco)
			}
		}
	}
	return fmt.Errorf("turno excedeu 64 ciclos; teto técnico configurável (não é permissionamento)")
}

func ferramenta(ctx context.Context, lista []tools.Spec, cwd string, call dealer.ToolChamada) (string, error) {
	for _, s := range lista {
		if s.Name == call.Nome {
			return s.Exec(ctx, cwd, json.RawMessage(call.Args))
		}
	}
	return "", fmt.Errorf("tool desconhecida: %q (checar o nome exato)", call.Nome)
}

// resultadoSaida devolve o trecho a ecoar no stream para o humano: até
// 2 KiB de uma saída de tool (janela legível; o modelo recebeu tudo).
func resultadoSaida(saida string) string {
	s := strings.TrimRight(saida, "\n")
	if s == "" {
		return ""
	}
	linhas := strings.Split(s, "\n")
	if len(linhas) > 12 {
		linhas = append(linhas[:12], "…")
	}
	eco := strings.Join(linhas, "\n")
	if len(eco) > 2<<10 {
		eco = eco[:2<<10] + "…"
	}
	return eco
}

// podar troca resultados antigos e grandes de tool por bilhete curto. A poda
// é monotônica dentro do turno (o piso não desce) — o provider de prefixo,
// quendo houver, agradecer. Retorna nil se não aliviar o suficiente.
func podar(h []dealer.Mensagem, piso *int, deficit int) []dealer.Mensagem {
	if deficit <= 0 {
		return nil
	}
	cauda := 24
	if len(h) <= cauda {
		return nil
	}
	nova := make([]dealer.Mensagem, len(h))
	copy(nova, h)
	limite := len(nova) - cauda // índice da primeira fora da cauda intocada
	if limite <= *piso {
		return nil
	}
	recuperado := 0
	// varre do mais antigo para o mais novo dentro da zona podável: mantém o
	// prefixo do request estável o máximo possível (provider com cache
	// de prefixo agradece).
	for i := *piso; i < limite; i++ {
		if nova[i].Role != "tool_result" {
			continue
		}
		t := len(nova[i].Conteudo)
		if t < 2048 {
			continue
		}
		nova[i].Conteudo = fmt.Sprintf(
			"(resultado anterior de %s compactado pelo guardrail; re-peça com range menor se precisar do conteúdo)",
			nova[i].ToolName)
		recuperado += t - len(nova[i].Conteudo)
		if recuperado >= deficit {
			break
		}
	}
	if recuperado < deficit {
		return nil
	}
	*piso = limite
	return nova
}

func estouraContexto(err error) bool {
	s := strings.ToLower(err.Error())
	return (strings.Contains(s, "context") || strings.Contains(s, "maximum")) &&
		(strings.Contains(s, "length") || strings.Contains(s, "tokens"))
}
