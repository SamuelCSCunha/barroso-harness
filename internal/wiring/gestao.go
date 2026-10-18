// Gestão de servidores MCP pelo próprio agente: mcp_list, mcp_add e
// mcp_remove. Antes disto o modelo só USAVA servidores escritos à mão no
// config.json — não tinha como adicionar um, ver o estado deles ou tirar um
// que quebrou. O plano pedia "gestão de skills e MCP"; esta é a metade MCP.
//
// Adicionar conecta NA HORA e injeta as tools na sessão (valem já no próximo
// ciclo do turno); remover fecha o cliente e retira as tools. As duas
// persistem no config.json, então a mudança sobrevive ao processo.
package wiring

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/mcp"
	"github.com/ruanbarroso/barroso-harness/internal/runtime"
	"github.com/ruanbarroso/barroso-harness/internal/tools"
)

// estadoMCP é o que o gestor sabe de cada servidor configurado.
type estadoMCP struct {
	cfg     config.MCPServidor
	cliente *mcp.Cliente // nil quando não conectou
	tools   []string     // nomes injetados na sessão
	erro    string       // por que não conectou / não listou
}

// gestorMCP vive dentro do Conjunto; o mutex protege o mapa contra turnos e
// fechamento concorrentes.
type gestorMCP struct {
	mu      sync.Mutex
	ctx     context.Context
	sess    *runtime.Session
	estados map[string]*estadoMCP
	ordem   []string
	salvar  func([]config.MCPServidor) error // persiste a lista; injetável em teste
}

func novoGestorMCP(ctx context.Context, sess *runtime.Session) *gestorMCP {
	return &gestorMCP{
		ctx:     ctx,
		sess:    sess,
		estados: map[string]*estadoMCP{},
		salvar:  salvarServidoresMCP,
	}
}

// salvarServidoresMCP relê o config.json do disco e troca só a lista de
// servidores: o REPL pode ter mudado provider/modelo desde que a sessão nasceu,
// e regravar a config inteira da memória apagaria essa mudança.
func salvarServidoresMCP(lista []config.MCPServidor) error {
	c, err := config.Load()
	if err != nil {
		return err
	}
	c.MCPServidores = lista
	return c.Save()
}

// conectar abre um servidor e injeta as tools; nunca falha "para fora" — o
// erro fica no estado para mcp_list mostrar.
func (g *gestorMCP) conectar(s config.MCPServidor) *estadoMCP {
	e := &estadoMCP{cfg: s}
	servs := servidoresDe([]config.MCPServidor{s})
	if len(servs) == 0 {
		e.erro = "configuração incompleta: stdio exige command; http exige url"
		return e
	}
	clientes, falhas := mcp.ConectarTodos(g.ctx, servs)
	if len(falhas) > 0 {
		e.erro = umaLinha(falhas[0].Err)
		return e
	}
	c := clientes[0]
	specs, err := c.Ferramentas(g.ctx)
	if err != nil {
		_ = c.Fecha()
		e.erro = "tools/list: " + umaLinha(err)
		return e
	}
	e.cliente = c
	for _, sp := range specs {
		e.tools = append(e.tools, sp.Name)
	}
	g.sess.Injetar(specs...)
	return e
}

func (g *gestorMCP) registrar(e *estadoMCP) {
	nome := strings.TrimSpace(e.cfg.Nome)
	if _, ja := g.estados[nome]; !ja {
		g.ordem = append(g.ordem, nome)
	}
	g.estados[nome] = e
}

func (g *gestorMCP) configs() []config.MCPServidor {
	out := make([]config.MCPServidor, 0, len(g.ordem))
	for _, n := range g.ordem {
		out = append(out, g.estados[n].cfg)
	}
	return out
}

func (g *gestorMCP) clientes() []*mcp.Cliente {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []*mcp.Cliente
	for _, n := range g.ordem {
		if c := g.estados[n].cliente; c != nil {
			out = append(out, c)
		}
	}
	return out
}

func (g *gestorMCP) fechar() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	var cs []*mcp.Cliente
	for _, n := range g.ordem {
		if c := g.estados[n].cliente; c != nil {
			cs = append(cs, c)
			g.estados[n].cliente = nil
		}
	}
	if len(cs) == 0 {
		return nil
	}
	return mcp.FechaTodos(cs)
}

// ── as três tools ─────────────────────────────────────────────────────────

func (g *gestorMCP) specs() []tools.Spec {
	return []tools.Spec{g.specList(), g.specAdd(), g.specRemove()}
}

func (g *gestorMCP) specList() tools.Spec {
	return tools.Spec{
		Name:        "mcp_list",
		Description: "Lista os servidores MCP configurados (nome, transporte, se conectou, quantas tools publicou ou o erro).",
		Schema:      map[string]any{"type": "object", "properties": map[string]any{}},
		Exec: func(context.Context, string, json.RawMessage) (string, error) {
			g.mu.Lock()
			defer g.mu.Unlock()
			if len(g.ordem) == 0 {
				return "nenhum servidor MCP configurado. Use mcp_add para adicionar.", nil
			}
			var sb strings.Builder
			for _, n := range g.ordem {
				e := g.estados[n]
				alvo := e.cfg.URL
				if tipoMCP(e.cfg.Tipo) == "stdio" {
					alvo = strings.TrimSpace(e.cfg.Command + " " + strings.Join(e.cfg.Args, " "))
				}
				if e.cliente != nil {
					t := append([]string(nil), e.tools...)
					sort.Strings(t)
					fmt.Fprintf(&sb, "%s [%s] %s — conectado, %d tools: %s\n", n, tipoMCP(e.cfg.Tipo), alvo, len(t), strings.Join(t, ", "))
				} else {
					fmt.Fprintf(&sb, "%s [%s] %s — NÃO conectado: %s\n", n, tipoMCP(e.cfg.Tipo), alvo, e.erro)
				}
			}
			return strings.TrimRight(sb.String(), "\n"), nil
		},
	}
}

func (g *gestorMCP) specAdd() tools.Spec {
	return tools.Spec{
		Name: "mcp_add",
		Description: "Adiciona (ou substitui) um servidor MCP, conecta na hora e grava no config.json. " +
			"stdio: informe command (e args/env). http (streamable HTTP): informe url (e headers). " +
			"As tools do servidor ficam disponíveis a partir da próxima chamada.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name":    map[string]any{"type": "string", "description": "nome único do servidor"},
				"type":    map[string]any{"type": "string", "enum": []string{"stdio", "http"}},
				"command": map[string]any{"type": "string", "description": "executável (stdio)"},
				"args":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"env":     map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}},
				"url":     map[string]any{"type": "string", "description": "endpoint (http)"},
				"headers": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}},
			},
			"required": []string{"name", "type"},
		},
		Exec: func(_ context.Context, _ string, raw json.RawMessage) (string, error) {
			var s config.MCPServidor
			if err := json.Unmarshal(raw, &s); err != nil {
				return "", fmt.Errorf("entrada inválida: %w", err)
			}
			s.Nome = strings.TrimSpace(s.Nome)
			if s.Nome == "" {
				return "", fmt.Errorf("name obrigatório")
			}
			if tipoMCP(s.Tipo) == "" {
				return "", fmt.Errorf("type deve ser stdio ou http")
			}
			if !aceitavel(s) {
				return "", fmt.Errorf("configuração incompleta: stdio exige command; http exige url")
			}
			g.mu.Lock()
			defer g.mu.Unlock()
			// Substituir: o antigo sai antes, para as tools não duplicarem.
			if velho := g.estados[s.Nome]; velho != nil {
				g.retirar(velho)
			}
			e := g.conectar(s)
			g.registrar(e)
			if err := g.salvar(g.configs()); err != nil {
				return "", fmt.Errorf("conectou, mas não gravou no config.json: %w", err)
			}
			if e.cliente == nil {
				return fmt.Sprintf("servidor %q gravado no config.json, mas NÃO conectou: %s", s.Nome, e.erro), nil
			}
			t := append([]string(nil), e.tools...)
			sort.Strings(t)
			return fmt.Sprintf("servidor %q conectado e gravado; %d tools disponíveis: %s", s.Nome, len(t), strings.Join(t, ", ")), nil
		},
	}
}

func (g *gestorMCP) specRemove() tools.Spec {
	return tools.Spec{
		Name:        "mcp_remove",
		Description: "Remove um servidor MCP: desconecta, tira as tools dele do catálogo e apaga do config.json.",
		Schema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"name": map[string]any{"type": "string"}},
			"required":   []string{"name"},
		},
		Exec: func(_ context.Context, _ string, raw json.RawMessage) (string, error) {
			var c struct {
				Nome string `json:"name"`
			}
			if err := json.Unmarshal(raw, &c); err != nil {
				return "", fmt.Errorf("entrada inválida: %w", err)
			}
			nome := strings.TrimSpace(c.Nome)
			g.mu.Lock()
			defer g.mu.Unlock()
			e := g.estados[nome]
			if e == nil {
				return "", fmt.Errorf("servidor %q não existe (mcp_list lista)", nome)
			}
			g.retirar(e)
			delete(g.estados, nome)
			for i, n := range g.ordem {
				if n == nome {
					g.ordem = append(g.ordem[:i], g.ordem[i+1:]...)
					break
				}
			}
			if err := g.salvar(g.configs()); err != nil {
				return "", fmt.Errorf("desconectou, mas não gravou no config.json: %w", err)
			}
			return fmt.Sprintf("servidor %q removido (%d tools saíram do catálogo)", nome, len(e.tools)), nil
		},
	}
}

// retirar desconecta e tira as tools (chamado sob g.mu).
func (g *gestorMCP) retirar(e *estadoMCP) {
	g.sess.Retirar(e.tools...)
	if e.cliente != nil {
		_ = e.cliente.Fecha()
		e.cliente = nil
	}
}
