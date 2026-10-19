// Package wiring monta o catálogo estendido da sessão: o que NÃO é built-in e
// precisa de recurso de processo — gerente de sub-agentes e servidores MCP.
//
// Ele existe para que os três pontos de entrada (REPL, modo -p e o agente ACP)
// montem a MESMA lista de ferramentas com UMA receita. Sem o pacote, cada
// chamador repetiria a sequência "conecta MCP, cria gerente, injeta specs,
// lembra de fechar" — e a receita que um esquece é a tool que o modelo não vê.
//
// Por que não no runtime: runtime é importado por internal/subagent (o agente
// filho nasce de uma runtime.Session). Colocar subagent/MCP dentro dele seria
// ciclo de importação. O runtime resolve a metade independente — contexto
// vivo, web e skills, ver internal/runtime/extras.go — e este pacote resolve a
// metade que tem dono externo, entregando o resultado pelo mesmo ponto de
// entrada: Session.Injetar.
package wiring

import (
	"context"
	"fmt"
	"strings"

	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/mcp"
	"github.com/ruanbarroso/barroso-harness/internal/runtime"
	"github.com/ruanbarroso/barroso-harness/internal/subagent"
)

// Conjunto é o que a sessão ganhou: as specs já injetadas e os recursos que
// precisam ser FECHADOS no fim do processo. Um Conjunto sem recurso aberto
// (nenhum servidor MCP; ou o Conjunto{} de sessão nil) é válido — Fechar é
// no-op. O gerente de sub-agentes nasce SEMPRE: toda sessão principal tem
// task_spawn/task_status/task_result; a sessão FILHA não as recebe porque o
// runtime a monta de tools.SpecsBuiltins() (invariante por construção, não
// por configuração).
type Conjunto struct {
	gerente *subagent.Gerente // sempre presente quando Montar passou por sessão real
	gestor  *gestorMCP        // servidores MCP (conectados ou não) e suas tools
}

// Gerente devolve o gerente de tarefas-irmãs (nil só num Conjunto vazio —
// sessão nil). Útil para a UI mostrar contagem e para testes.
func (c *Conjunto) Gerente() *subagent.Gerente {
	if c == nil {
		return nil
	}
	return c.gerente
}

// MCP devolve os clientes MCP conectados, na ordem dos servidores da config.
func (c *Conjunto) MCP() []*mcp.Cliente {
	if c == nil || c.gestor == nil {
		return nil
	}
	return c.gestor.clientes()
}

// Montar conecta os servidores MCP, sobe o gerente de sub-agentes e injeta
// tudo na sessão. Devolve o Conjunto para fechar depois e os AVISOS — uma
// linha por servidor que falhou, para o chamador mostrar sem interromper.
//
// Regra de tolerância: falha de UM servidor MCP não invalida os demais nem o
// turno. Ferramenta externa é conveniência; derrubar a sessão porque um
// subprocesso não subiu seria trocar um problema pequeno por um grande.
//
// ctx deve ser de VIDA LONGA (não o de um turno): ele é herdado pelos
// sub-agentes, que precisam sobreviver ao turno que os criou, e é o prazo do
// handshake MCP. O REPL passa um ctx que só cancela no fim do processo.
func Montar(ctx context.Context, cfg config.Config, sess *runtime.Session, cwd string) (*Conjunto, []string) {
	if sess == nil {
		return &Conjunto{}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var avisos []string

	// ── MCP ────────────────────────────────────────────────────────────────
	// O gestor conecta os servidores da config (falha de um vira aviso) e
	// publica mcp_list/mcp_add/mcp_remove: o agente gerencia os próprios
	// servidores sem editar o config.json à mão.
	gestor := novoGestorMCP(ctx, sess)
	vistos := map[string]bool{}
	for _, s := range cfg.MCPServidores {
		nome := strings.TrimSpace(s.Nome)
		if nome == "" || vistos[nome] {
			continue
		}
		vistos[nome] = true
		e := gestor.conectar(s)
		gestor.registrar(e)
		if e.erro != "" {
			avisos = append(avisos, fmt.Sprintf("servidor MCP %q: %s", nome, e.erro))
		}
	}
	sess.Injetar(gestor.specs()...)

	// ── sub-agentes ────────────────────────────────────────────────────────
	// SEMPRE ligados, sem flag de config: delegar é parte do catálogo base da
	// sessão principal. O filho não herda a delegação por construção — o
	// runtime.Session.Send monta a lista dele de tools.SpecsBuiltins(), que
	// não tem task_* — então não há risco de recursão a administrar.
	gerente := subagent.Novo(subagent.Config{
		Nome:            subagenteNome(),
		MaxConcorrentes: subagent.MaxPadrao,
	})
	gerente.DefinirCwd(cwd)
	// Vincular ao ctx longo ANTES do primeiro Spawn: as tarefas derivam o
	// contexto filho daqui, e o ctx do turno morre no fim da linha.
	gerente.Vincular(ctx)
	sess.Injetar(subagent.Especificacoes(gerente, cfg)...)

	return &Conjunto{gerente: gerente, gestor: gestor}, avisos
}

// Fechar encerra os servidores MCP. Sub-agentes não têm fechamento: o registro
// é em memória e as tarefas morrem com o processo (persistência de tarefa está
// fora deste slice). Devolve nil quando não há cliente aberto.
func (c *Conjunto) Fechar() error {
	if c == nil || c.gestor == nil {
		return nil
	}
	return c.gestor.fechar()
}

// ── conversão config → mcp ────────────────────────────────────────────────

// servidoresDe traduz a config para o tipo do pacote mcp, filtrando o que
// certamente não abriria. A validação fina é do mcp (que conhece o protocolo);
// aqui só se evita tentar abrir servidor sem nome único, sem command/url ou com
// tipo irreconhecível — cada um desses viraria uma linha de aviso à toa.
//
// A tradução é de campo em campo, e não um cast: config não pode importar mcp
// (é a raiz da árvore de dependências), então os dois tipos são independentes
// por desenho e esta função é a fronteira única entre eles.
//
// A regra de tipo é a MESMA de config.servidoresValidos (que o REPL usa para
// listar): "streamable-http" e as variantes de nome entram como "http", que é
// o único transporte HTTP que o cliente implementa.
func servidoresDe(cfgs []config.MCPServidor) []mcp.Servidor {
	var out []mcp.Servidor
	vistos := map[string]bool{}
	for _, s := range cfgs {
		if !aceitavel(s) || vistos[strings.TrimSpace(s.Nome)] {
			continue
		}
		vistos[strings.TrimSpace(s.Nome)] = true
		out = append(out, mcp.Servidor{
			Nome:    strings.TrimSpace(s.Nome),
			Tipo:    tipoMCP(s.Tipo),
			Command: strings.TrimSpace(s.Command),
			Args:    s.Args,
			Env:     s.Env,
			URL:     strings.TrimSpace(s.URL),
			Headers: s.Headers,
			Timeout: s.Timeout,
		})
	}
	return out
}

// aceitavel é o filtro mínimo: nome presente e o campo que o transporte exige.
// Tipo desconhecido já é recusado aqui em vez de virar erro de cada servidor.
func aceitavel(s config.MCPServidor) bool {
	if strings.TrimSpace(s.Nome) == "" {
		return false
	}
	tipo := tipoMCP(s.Tipo)
	switch tipo {
	case "stdio":
		return strings.TrimSpace(s.Command) != ""
	case "http":
		return strings.TrimSpace(s.URL) != ""
	default:
		return false
	}
}

// tipoMCP normaliza o nome do transporte para a forma que o pacote mcp espera
// ("stdio" | "http"); devolve "" quando não é nenhum dos dois.
func tipoMCP(t string) string {
	n := strings.ToLower(strings.TrimSpace(t))
	n = strings.NewReplacer("_", "-", " ", "-").Replace(n)
	switch n {
	case "stdio":
		return "stdio"
	case "http", "streamable-http", "http-streamable", "http-streaming", "streamablehttp":
		return "http"
	default:
		return ""
	}
}

// subagenteNome devolve o prefixo das tools de tarefa (task_spawn/
// task_status/task_result). Fixo, e não derivado do modelo: prefixo
// configurável só interessa a quem roda dois gerentes no MESMO catálogo, e aí
// o risco de ambiguar nomes de tool é maior que o ganho.
func subagenteNome() string { return subagent.PrefixoPadrao }

// umaLinha reduz um erro a UMA linha: aviso de servidor é linha de REPL, não
// bloco.
func umaLinha(err error) string {
	if err == nil {
		return "(sem detalhe)"
	}
	s := strings.TrimSpace(err.Error())
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	const teto = 160
	if len(s) > teto {
		s = s[:teto] + "…"
	}
	return s
}
