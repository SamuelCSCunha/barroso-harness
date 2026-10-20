// MCP server management by the agent itself: mcp_list, mcp_add, mcp_edit and
// mcp_remove. Before this the model could only USE servers hand-written in
// config.json — it had no way to add one, see their state or remove one that
// broke. The plan asked for "skills and MCP management"; this is the MCP half.
//
// Adding connects RIGHT AWAY and injects the tools into the session (they are
// available from the next cycle of the turn); removing closes the client and
// withdraws the tools. Both persist to config.json, so the change survives the
// process.
package wiring

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/mcp"
	"github.com/ruanbarroso/barroso-harness/internal/runtime"
	"github.com/ruanbarroso/barroso-harness/internal/tools"
)

// mcpState is what the manager knows about each configured server.
type mcpState struct {
	cfg    config.MCPServidor
	client *mcp.Cliente // nil when it did not connect
	tools  []string     // names injected into the session
	err    string       // why it did not connect / list
}

// mcpManager lives inside the Conjunto; the mutex protects the map against
// concurrent turns and shutdown.
type mcpManager struct {
	mu     sync.Mutex
	ctx    context.Context
	sess   *runtime.Session
	states map[string]*mcpState
	order  []string
	save   func([]config.MCPServidor) error // persists the list; injectable in tests
	// seen is config.json's mtime at the last reconcile (zero = not yet).
	seen time.Time
}

func newMCPManager(ctx context.Context, sess *runtime.Session) *mcpManager {
	return &mcpManager{
		ctx:    ctx,
		sess:   sess,
		states: map[string]*mcpState{},
		save:   saveMCPServers,
	}
}

// saveMCPServers re-reads config.json from disk and replaces only the server
// list: the REPL may have changed provider/model since the session was born,
// and rewriting the whole in-memory config would erase that change.
func saveMCPServers(list []config.MCPServidor) error {
	c, err := config.Load()
	if err != nil {
		return err
	}
	c.MCPServidores = list
	return c.Save()
}

// connect opens a server and injects its tools; it never fails "outward" —
// the error stays in the state for mcp_list to show.
func (m *mcpManager) connect(s config.MCPServidor) *mcpState {
	e := &mcpState{cfg: s}
	servers := serversFrom([]config.MCPServidor{s})
	if len(servers) == 0 {
		e.err = "incomplete configuration: stdio requires command; http requires url"
		return e
	}
	clients, failures := mcp.ConectarTodos(m.ctx, servers)
	if len(failures) > 0 {
		e.err = oneLine(failures[0].Err)
		return e
	}
	c := clients[0]
	specs, err := c.Ferramentas(m.ctx)
	if err != nil {
		_ = c.Fecha()
		e.err = "tools/list: " + oneLine(err)
		return e
	}
	e.client = c
	for _, sp := range specs {
		e.tools = append(e.tools, sp.Name)
	}
	m.sess.Injetar(specs...)
	return e
}

func (m *mcpManager) register(e *mcpState) {
	name := strings.TrimSpace(e.cfg.Nome)
	if _, exists := m.states[name]; !exists {
		m.order = append(m.order, name)
	}
	m.states[name] = e
}

func (m *mcpManager) configs() []config.MCPServidor {
	out := make([]config.MCPServidor, 0, len(m.order))
	for _, n := range m.order {
		out = append(out, m.states[n].cfg)
	}
	return out
}

func (m *mcpManager) clients() []*mcp.Cliente {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*mcp.Cliente
	for _, n := range m.order {
		if c := m.states[n].client; c != nil {
			out = append(out, c)
		}
	}
	return out
}

func (m *mcpManager) close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var cs []*mcp.Cliente
	for _, n := range m.order {
		if c := m.states[n].client; c != nil {
			cs = append(cs, c)
			m.states[n].client = nil
		}
	}
	if len(cs) == 0 {
		return nil
	}
	return mcp.FechaTodos(cs)
}

// ── the three tools ───────────────────────────────────────────────────────

func (m *mcpManager) specs() []tools.Spec {
	return []tools.Spec{m.specList(), m.specAdd(), m.specEdit(), m.specRemove()}
}

// describe is the reply after connecting a server: its tools or its error.
func describe(e *mcpState, verb string) string {
	if e.client == nil {
		return fmt.Sprintf("server %q %s and saved to config.json, but did NOT connect: %s", e.cfg.Nome, verb, e.err)
	}
	t := append([]string(nil), e.tools...)
	sort.Strings(t)
	return fmt.Sprintf("server %q %s, connected and saved; %d tools available: %s", e.cfg.Nome, verb, len(t), strings.Join(t, ", "))
}

func (m *mcpManager) specEdit() tools.Spec {
	return tools.Spec{
		Name: "mcp_edit",
		Description: "Edits an existing MCP server: give only the fields that change (type, command, args, env, url, headers, new_name). " +
			"env and headers given here replace the whole map. Saves config.json and reconnects only this server. " +
			"With reconnect true and nothing else, just reconnects it (e.g. after the server process died).",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name":      map[string]any{"type": "string", "description": "current server name"},
				"new_name":  map[string]any{"type": "string", "description": "rename the server"},
				"type":      map[string]any{"type": "string", "enum": []string{"stdio", "http"}},
				"command":   map[string]any{"type": "string", "description": "executable (stdio)"},
				"args":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"env":       map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}},
				"url":       map[string]any{"type": "string", "description": "endpoint (http)"},
				"headers":   map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}},
				"reconnect": map[string]any{"type": "boolean", "description": "reconnect even if nothing else changes"},
			},
			"required": []string{"name"},
		},
		Exec: func(_ context.Context, _ string, raw json.RawMessage) (string, error) {
			var c struct {
				Name      string             `json:"name"`
				NewName   string             `json:"new_name"`
				Type      *string            `json:"type"`
				Command   *string            `json:"command"`
				Args      *[]string          `json:"args"`
				Env       *map[string]string `json:"env"`
				URL       *string            `json:"url"`
				Headers   *map[string]string `json:"headers"`
				Reconnect bool               `json:"reconnect"`
			}
			if err := json.Unmarshal(raw, &c); err != nil {
				return "", fmt.Errorf("invalid input: %w", err)
			}
			name := strings.TrimSpace(c.Name)
			m.mu.Lock()
			defer m.mu.Unlock()
			old := m.states[name]
			if old == nil {
				return "", fmt.Errorf("server %q does not exist (see mcp_list; add it with mcp_add)", name)
			}
			s := old.cfg
			changed := c.Reconnect
			if c.Type != nil {
				s.Tipo, changed = *c.Type, true
			}
			if c.Command != nil {
				s.Command, changed = *c.Command, true
			}
			if c.Args != nil {
				s.Args, changed = *c.Args, true
			}
			if c.Env != nil {
				s.Env, changed = *c.Env, true
			}
			if c.URL != nil {
				s.URL, changed = *c.URL, true
			}
			if c.Headers != nil {
				s.Headers, changed = *c.Headers, true
			}
			newName := strings.TrimSpace(c.NewName)
			if newName != "" && newName != name {
				if m.states[newName] != nil {
					return "", fmt.Errorf("server %q already exists", newName)
				}
				s.Nome, changed = newName, true
			}
			if !changed {
				return "", fmt.Errorf("nothing to change: give the fields to edit or reconnect true")
			}
			if mcpType(s.Tipo) == "" {
				return "", fmt.Errorf("type must be stdio or http")
			}
			if !acceptable(s) {
				return "", fmt.Errorf("incomplete configuration: stdio requires command; http requires url")
			}
			m.withdraw(old)
			e := m.connect(s)
			if s.Nome != name {
				delete(m.states, name)
				for i, n := range m.order {
					if n == name {
						m.order[i] = s.Nome
						break
					}
				}
			}
			m.states[s.Nome] = e
			if err := m.save(m.configs()); err != nil {
				return "", fmt.Errorf("reconnected, but did not save to config.json: %w", err)
			}
			return describe(e, "edited"), nil
		},
	}
}

func (m *mcpManager) specList() tools.Spec {
	return tools.Spec{
		Name:        "mcp_list",
		Description: "Lists the configured MCP servers (name, transport, whether it connected, how many tools it published or the error).",
		Schema:      map[string]any{"type": "object", "properties": map[string]any{}},
		Exec: func(context.Context, string, json.RawMessage) (string, error) {
			m.mu.Lock()
			defer m.mu.Unlock()
			if len(m.order) == 0 {
				return "no MCP server configured. Use mcp_add to add one.", nil
			}
			var sb strings.Builder
			for _, n := range m.order {
				e := m.states[n]
				target := e.cfg.URL
				if mcpType(e.cfg.Tipo) == "stdio" {
					target = strings.TrimSpace(e.cfg.Command + " " + strings.Join(e.cfg.Args, " "))
				}
				if e.client != nil {
					t := append([]string(nil), e.tools...)
					sort.Strings(t)
					fmt.Fprintf(&sb, "%s [%s] %s — connected, %d tools: %s\n", n, mcpType(e.cfg.Tipo), target, len(t), strings.Join(t, ", "))
				} else {
					fmt.Fprintf(&sb, "%s [%s] %s — NOT connected: %s\n", n, mcpType(e.cfg.Tipo), target, e.err)
				}
			}
			return strings.TrimRight(sb.String(), "\n"), nil
		},
	}
}

func (m *mcpManager) specAdd() tools.Spec {
	return tools.Spec{
		Name: "mcp_add",
		Description: "Adds a NEW MCP server, connects right away and saves it to config.json. " +
			"stdio: provide command (and args/env). http (streamable HTTP): provide url (and headers). " +
			"The server's tools become available from the next call on. Fails if the name exists: change it with mcp_edit.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name":    map[string]any{"type": "string", "description": "unique server name"},
				"type":    map[string]any{"type": "string", "enum": []string{"stdio", "http"}},
				"command": map[string]any{"type": "string", "description": "executable (stdio)"},
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
				return "", fmt.Errorf("invalid input: %w", err)
			}
			s.Nome = strings.TrimSpace(s.Nome)
			if s.Nome == "" {
				return "", fmt.Errorf("name is required")
			}
			if mcpType(s.Tipo) == "" {
				return "", fmt.Errorf("type must be stdio or http")
			}
			if !acceptable(s) {
				return "", fmt.Errorf("incomplete configuration: stdio requires command; http requires url")
			}
			m.mu.Lock()
			defer m.mu.Unlock()
			if m.states[s.Nome] != nil {
				return "", fmt.Errorf("server %q already exists: use mcp_edit to change it", s.Nome)
			}
			e := m.connect(s)
			m.register(e)
			if err := m.save(m.configs()); err != nil {
				return "", fmt.Errorf("connected, but did not save to config.json: %w", err)
			}
			return describe(e, "added"), nil
		},
	}
}

func (m *mcpManager) specRemove() tools.Spec {
	return tools.Spec{
		Name:        "mcp_remove",
		Description: "Removes an MCP server: disconnects it, takes its tools out of the catalog and deletes it from config.json.",
		Schema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"name": map[string]any{"type": "string"}},
			"required":   []string{"name"},
		},
		Exec: func(_ context.Context, _ string, raw json.RawMessage) (string, error) {
			var c struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(raw, &c); err != nil {
				return "", fmt.Errorf("invalid input: %w", err)
			}
			name := strings.TrimSpace(c.Name)
			m.mu.Lock()
			defer m.mu.Unlock()
			e := m.states[name]
			if e == nil {
				return "", fmt.Errorf("server %q does not exist (see mcp_list)", name)
			}
			m.withdraw(e)
			delete(m.states, name)
			for i, n := range m.order {
				if n == name {
					m.order = append(m.order[:i], m.order[i+1:]...)
					break
				}
			}
			if err := m.save(m.configs()); err != nil {
				return "", fmt.Errorf("disconnected, but did not save to config.json: %w", err)
			}
			return fmt.Sprintf("server %q removed (%d tools left the catalog)", name, len(e.tools)), nil
		},
	}
}

// withdraw disconnects and removes the tools (called under m.mu).
func (m *mcpManager) withdraw(e *mcpState) {
	m.sess.Retirar(e.tools...)
	if e.client != nil {
		_ = e.client.Fecha()
		e.client = nil
	}
}

// reconcile brings this process's MCP clients in line with config.json, which
// another bh process (or the user) may have changed: new servers connect,
// edited ones reconnect, removed ones close. It is the session's Watcher, run
// at every request boundary; the stat keeps the common case (nothing changed)
// to one syscall. Each returned line becomes a [system update].
func (m *mcpManager) reconcile() []string {
	lines := m.refreshChanged()
	info, err := os.Stat(filepath.Join(config.Dir(), "config.json"))
	if err != nil {
		return lines
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if info.ModTime().Equal(m.seen) {
		return lines
	}
	first := m.seen.IsZero()
	m.seen = info.ModTime()
	if first {
		return lines
	}
	c, err := config.Load()
	if err != nil {
		return lines
	}
	want := map[string]config.MCPServidor{}
	for _, s := range c.MCPServidores {
		name := strings.TrimSpace(s.Nome)
		if name == "" {
			continue
		}
		want[name] = s
		old := m.states[name]
		if old != nil && sameServer(old.cfg, s) {
			continue
		}
		verb := "added"
		if old != nil {
			m.withdraw(old)
			verb = "edited"
		}
		e := m.connect(s)
		m.register(e)
		lines = append(lines, "MCP "+describe(e, verb))
	}
	for _, n := range append([]string(nil), m.order...) {
		if _, ok := want[n]; ok {
			continue
		}
		m.withdraw(m.states[n])
		delete(m.states, n)
		for i, o := range m.order {
			if o == n {
				m.order = append(m.order[:i], m.order[i+1:]...)
				break
			}
		}
		lines = append(lines, fmt.Sprintf("MCP server %q was removed; its tools are gone", n))
	}
	return lines
}

// sameServer compares two server configs by their serialized form.
func sameServer(a, b config.MCPServidor) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}

// refreshChanged refetches the tools of servers that sent tools/list_changed.
// A failed refresh keeps the previous tools (as Claude Code does).
func (m *mcpManager) refreshChanged() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var lines []string
	for _, n := range m.order {
		e := m.states[n]
		if e.client == nil || !e.client.ToolsChanged() {
			continue
		}
		specs, err := e.client.Ferramentas(m.ctx)
		if err != nil {
			continue
		}
		m.sess.Retirar(e.tools...)
		e.tools = e.tools[:0]
		for _, sp := range specs {
			e.tools = append(e.tools, sp.Name)
		}
		m.sess.Injetar(specs...)
		lines = append(lines, "MCP "+describe(e, "refreshed its tools"))
	}
	return lines
}
