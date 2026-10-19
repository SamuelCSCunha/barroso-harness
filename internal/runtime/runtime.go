// Package runtime compõe o turno: instruções BARROSO.md → histórico →
// guardrail → loop do modelo → tools YOLO → compactação proativa/reativa.
// O transcript é append-only dentro do processo. A Sessão (session.go)
// executa o loop; Turno é o wrapper de um único pedido usado por -p.
package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/dealer"
	"github.com/ruanbarroso/barroso-harness/internal/store"
)

// Turno executa um pedido único (modo -p): streaming de texto na saída, tools
// em modo YOLO, guardrail de janela antes de cada chamada e recuperação
// reativa limitada. Reusa a Sessão sem histórico prévio: stateless por vez.
func Turno(out io.Writer, cfg config.Config, cwd, pedido string) error {
	if strings.TrimSpace(pedido) == "" {
		return fmt.Errorf("empty prompt: use -p \"...\"")
	}
	if !cfg.Pronta() {
		return fmt.Errorf(
			"incomplete configuration (%s) — run `bh` with no arguments to set up provider/model interactively, or edit %s",
			strings.Join(cfg.Faltas(), ", "), filePath(cfg))
	}
	if err := NewSession(cfg).Send(context.Background(), RendererDe(out), cwd, pedido); err != nil {
		return err
	}
	return nil
}

// TurnoPersistente é o Turno do modo -p COM log. O store chega pronto de quem
// chama (main.go abre em best-effort); sessionID é o id já gerado, para que o
// caller possa comunicar. Sem store, degrada para Turno: o turno roda igual.
func TurnoPersistente(out io.Writer, cfg config.Config, cwd, pedido string, st *store.Store, sessionID string) error {
	if st == nil || strings.TrimSpace(sessionID) == "" {
		return Turno(out, cfg, cwd, pedido)
	}
	if strings.TrimSpace(pedido) == "" {
		return fmt.Errorf("empty prompt: use -p \"...\"")
	}
	if !cfg.Pronta() {
		return fmt.Errorf(
			"incomplete configuration (%s) — run `bh` with no arguments to set up provider/model interactively, or edit %s",
			strings.Join(cfg.Faltas(), ", "), filePath(cfg))
	}
	return NewSessionWithStore(cfg, st, sessionID).Send(context.Background(), RendererDe(out), cwd, pedido)
}

// NovoSessaoID gera um id de sessão no mesmo formato do REPL
// (bh-<YYYYMMDDHHMMSS>-<4hex>), para o modo -p gravar no MESMO log.
func NovoSessaoID() string {
	return fmt.Sprintf("bh-%s-%s", time.Now().Format("20060102150405"), hex4Rand())
}

func hex4Rand() string {
	var b [2]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Sem entropia, o relógio vira a segunda fonte do aleatório.
		return fmt.Sprintf("%04x", time.Now().UnixNano()&0xffff)
	}
	return hex.EncodeToString(b[:])
}

// filePath devolve o caminho do config.json (não impresso em testes).
func filePath(cfg config.Config) string {
	if cfg.Home != "" {
		return cfg.Home + "\\config.json"
	}
	return "~/.barroso-harness/config.json"
}

// resultadoSaida devolve trecho a ecoar no stream: até 2 KiB de uma saída de
// tool (o histórico do modelo já recebeu tudo).
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
// é monotônica dentro do turno — mantém o prefixo do request estável.
func podar(h []dealer.Mensagem, piso *int, deficit int) []dealer.Mensagem {
	if deficit <= 0 {
		return nil
	}
	const cauda = 24
	if len(h) <= cauda {
		return nil
	}
	nova := append([]dealer.Mensagem(nil), h...)
	limite := len(nova) - cauda
	if limite <= *piso {
		return nil
	}
	recuperado := 0
	for i := *piso; i < limite; i++ {
		if nova[i].Role != "tool_result" || len(nova[i].Conteudo) < 2048 {
			continue
		}
		t := len(nova[i].Conteudo)
		nova[i].Conteudo = fmt.Sprintf("(resultado anterior de %s compactado pelo guardrail; re-peça com range menor se precisar do conteúdo)", nova[i].ToolName)
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
