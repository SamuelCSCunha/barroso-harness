// Package config holds the harness's persistent configuration. Source of
// truth on disk: ~/.barroso-harness/config.json, with the active provider in
// current_provider + providers (a map, so switching never erases credentials).
// Credentials live in THIS file, with restricted permissions — never in
// environment variables, code, README or logs. BARROSO_HARNESS_HOME remains
// valid only to relocate the data directory.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DefaultBaseURL is the in-house default gateway (OpenAI Chat Completions
// style, with /v1 at the end).
const DefaultBaseURL = "https://keys.barroso.tec.br/v1"

// Possible sources of the context window:
//   - FonteCatalogo: value derived from the provider's /models (switching
//     models may update or clear it).
//   - FonteManual: value written by hand in config.json (never cleared
//     automatically).
const (
	FonteCatalogo = "catalog"
	FonteManual   = "manual"
)

// Provider is a Chat Completions endpoint: base URL + key.
type Provider struct {
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key,omitempty"`
}

// Config is the persisted harness configuration.
type Config struct {
	CurrentProvider string               `json:"current_provider"`
	Providers       map[string]*Provider `json:"providers,omitempty"`
	Model           string               `json:"model"`
	Effort          string               `json:"effort,omitempty"`                // empty = disabled (the dealer omits the field)
	Janela          int                  `json:"context_window"`                  // window in tokens; 0 = unknown (guardrail off)
	JanelaSource    string               `json:"context_window_source,omitempty"` // FonteCatalogo | FonteManual | ""
	MaxTokens       int                  `json:"max_tokens"`                      // reserved for the provider; 0 = dealer default
	BytesPorToken   int                  `json:"bytes_per_token"`                 // manual calibration; 0 = default estimator (4)
	BaseURL         string               `json:"-"`                               // resolved from the active provider
	APIKey          string               `json:"-"`                               // resolved from the active provider; never log
	Home            string               `json:"-"`

	// SearchURL is the SearXNG endpoint with the JSON API enabled. Empty
	// DISABLES the tool: web_search still enters the catalog (the tool
	// explains what is missing), so web_fetch remains useful on its own.
	SearchURL string `json:"search_url,omitempty"`

	// MCPServidores lists MCP servers (stdio or streamable HTTP). The tools
	// they publish enter the turn's catalog.
	MCPServidores []MCPServidor `json:"mcp_servers,omitempty"`

	// PoliticaContexto chooses who decides to compact: "agente" (default; the
	// model calls ctx_compact at a task milestone, with the guardrail as a
	// safety net) or "limiar" (threshold: only the guardrail, when the window
	// overflows). These values are part of the on-disk format.
	PoliticaContexto string `json:"context_policy,omitempty"`
}

// MCPServidor mirrors mcp.Servidor without importing the package — config is
// the root of the dependency tree and must not point to a package that speaks
// a protocol. Converting to mcp.Servidor is one line in the wiring.
type MCPServidor struct {
	Nome    string            `json:"name"`
	Tipo    string            `json:"type"` // "stdio" | "http"
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Timeout int               `json:"timeout_segundos,omitempty"` // seconds; 0 = default
}

// BuscaAtiva reports whether a search endpoint is configured. The web_search
// tool enters the catalog either way (with the actionable error inside), but
// whoever needs to KNOW — wiring, UI, tests — asks here instead of comparing
// strings.
func (c Config) BuscaAtiva() bool { return strings.TrimSpace(c.SearchURL) != "" }

// validServers returns only the MCP servers with a recognizable name and
// type. FINE-grained validation belongs to the mcp package (it knows the
// protocol); here we only drop what would never open, so the wiring does not
// try.
func (c Config) validServers() []MCPServidor {
	var out []MCPServidor
	seen := map[string]bool{}
	for _, s := range c.MCPServidores {
		name := strings.TrimSpace(s.Nome)
		if name == "" || seen[name] {
			continue
		}
		kind := strings.ToLower(strings.TrimSpace(s.Tipo))
		kind = strings.NewReplacer("_", "-", " ", "-").Replace(kind)
		switch {
		case kind == "stdio":
			if strings.TrimSpace(s.Command) == "" {
				continue
			}
		case kind == "http", kind == "streamable-http", kind == "http-streamable",
			kind == "http-streaming", kind == "streamablehttp":
			if strings.TrimSpace(s.URL) == "" {
				continue
			}
		default:
			continue
		}
		seen[name] = true
		out = append(out, s)
	}
	return out
}

// Dir is the harness data root (~/.barroso-harness by default).
func Dir() string {
	if v := os.Getenv("BARROSO_HARNESS_HOME"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home, _ = os.Getwd()
	}
	return filepath.Join(home, ".barroso-harness")
}

// Provider returns the provider with the given name (nil if it does not exist).
func (c *Config) Provider(name string) *Provider {
	if c.Providers == nil || name == "" {
		return nil
	}
	return c.Providers[name]
}

// Ativo returns the current provider (nil if none is selected). The in-memory
// BaseURL/APIKey fields keep compatibility with older callers; they are never
// serialized and the disk keeps using providers/current_provider.
func (c *Config) Ativo() *Provider {
	if p := c.Provider(c.CurrentProvider); p != nil {
		return p
	}
	if c.BaseURL != "" || c.APIKey != "" {
		return &Provider{BaseURL: c.BaseURL, APIKey: c.APIKey}
	}
	return nil
}

// SelecionarProvider switches the active provider, recomputing the window
// when it came from the catalog (a manual window is never overwritten).
func (c *Config) SelecionarProvider(name string, window int) {
	c.CurrentProvider = name
	if c.JanelaSource == FonteCatalogo {
		c.Janela = window
		if c.Janela <= 0 {
			c.JanelaSource = ""
		}
	}
}

// Faltas lists what is missing for the config to be ready (used in
// actionable messages; never contains the key).
func (c Config) Faltas() []string {
	var missing []string
	p := c.Ativo()
	if p == nil {
		missing = append(missing, "provider (base_url + api_key)")
	} else {
		if p.BaseURL == "" {
			missing = append(missing, "base_url")
		}
		if p.APIKey == "" {
			missing = append(missing, "api_key")
		}
	}
	if c.Model == "" {
		missing = append(missing, "model")
	}
	return missing
}

// Pronta reports whether the harness has a provider (URL + key) and a model,
// enough to run a turn without interaction.
func (c Config) Pronta() bool { return len(c.Faltas()) == 0 }

// Resumo is the status line shown in the REPL — WITHOUT the key.
func (c Config) Resumo() string {
	base := ""
	if p := c.Ativo(); p != nil {
		base = p.BaseURL
	}
	line := fmt.Sprintf("provider %q @ %s", c.CurrentProvider, base)
	if c.Effort != "" {
		line += " · effort " + c.Effort
	}
	line += fmt.Sprintf(" · model %s", c.Model)
	if c.Janela > 0 {
		line += fmt.Sprintf(" · window %d (%s)", c.Janela, windowSource(c.JanelaSource))
	}
	return line
}

func windowSource(s string) string {
	switch s {
	case FonteCatalogo:
		return FonteCatalogo
	case FonteManual:
		return FonteManual
	default:
		return "unknown"
	}
}

// Load reads config.json. A missing file is NOT an error: it returns an empty
// config (the interactive path starts onboarding; a one-shot turn reports what
// is missing).
func Load() (Config, error) {
	c := Config{Home: Dir()}
	path := filepath.Join(c.Home, "config.json")
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return c, nil
	case err != nil:
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("invalid config %s: %w", path, err)
	}
	migrateLegacyShape(b, &c)
	c.Home = Dir()
	if p := c.Provider(c.CurrentProvider); p != nil {
		c.BaseURL, c.APIKey = p.BaseURL, p.APIKey
	}
	return c, nil
}

// migrateLegacyShape converts the slice-0 config ({base_url, model?} at the
// root, key from env) to the canonical shape with a named provider.
func migrateLegacyShape(b []byte, c *Config) {
	if c.CurrentProvider != "" || len(c.Providers) != 0 {
		return // already in the new shape
	}
	var legacy struct {
		BaseURL string `json:"base_url"`
		Model   string `json:"model"`
	}
	if err := json.Unmarshal(b, &legacy); err != nil || legacy.BaseURL == "" {
		return
	}
	name := "keys.barroso"
	if strings.TrimSuffix(legacy.BaseURL, "/") != DefaultBaseURL {
		name = "default"
	}
	c.Providers = map[string]*Provider{
		name: {BaseURL: strings.TrimSuffix(legacy.BaseURL, "/"), APIKey: ""},
	}
	c.CurrentProvider = name
	if c.Model == "" {
		c.Model = legacy.Model
	}
	if c.JanelaSource == "" && c.Janela > 0 {
		c.JanelaSource = FonteManual
	}
}

// Save persists the config atomically: a temp file in the same directory
// (0600 permissions), renamed over the target. The directory is created 0700.
func (c Config) Save() error {
	dir := Dir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	c.Home = dir
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	final := filepath.Join(dir, "config.json")
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	_ = os.Chmod(tmp, 0o600) // best-effort; Windows ignores POSIX bits
	if err := atomicReplace(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// Init creates the data layout NON-destructively: a 0700 directory, a sample
// global BARROSO.md and a config.json skeleton (completed by `bh` onboarding).
// Nothing that already exists is overwritten.
func Init() error {
	dir := Dir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	barPath := filepath.Join(dir, "BARROSO.md")
	if _, err := os.Stat(barPath); errors.Is(err, os.ErrNotExist) {
		sample := "# Global BARROSO.md\n\nInstructions that apply to ALL harness sessions, in any directory.\n" +
			"A per-project BARROSO.md (in the project folder or its parents) comes before this one.\n"
		if err := os.WriteFile(barPath, []byte(sample), 0o644); err != nil {
			return err
		}
	}
	cfgPath := filepath.Join(dir, "config.json")
	if _, err := os.Stat(cfgPath); errors.Is(err, os.ErrNotExist) {
		return Config{Home: dir}.Save()
	}
	return nil
}
