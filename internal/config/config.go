// Package config guarda a configuração persistente da harness. Fonte de
// verdade no disco: ~/.barroso-harness/config.json, com o provider ativo em
// current_provider + providers (mapa, para troca sem apagar credenciais).
// As credenciais vivem NESTE arquivo, com permissão restrita — nunca em
// variáveis de ambiente, código, README ou log. BARROSO_HARNESS_HOME segue
// válido apenas para mover o diretório de dados.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DefaultBaseURL é o gateway padrão da casa (padrão Chat Completions da
// OpenAI, com /v1 no fim).
const DefaultBaseURL = "https://keys.barroso.tec.br/v1"

// Fontes possíveis da janela de contexto:
//   - FonteCatalogo: valor derivado do /models do provider (a troca de
//     modelo pode atualizar ou limpar este valor).
//   - FonteManual: valor escrito à mão no config.json (nunca é apagado
//     automaticamente).
const (
	FonteCatalogo = "catalog"
	FonteManual   = "manual"
)

// Provider é um endpoint Chat Completions: base URL + chave.
type Provider struct {
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key,omitempty"`
}

type Config struct {
	CurrentProvider string               `json:"current_provider"`
	Providers       map[string]*Provider `json:"providers,omitempty"`
	Model           string               `json:"model"`
	Effort          string               `json:"effort,omitempty"`                // vazio = desativado (o dealer omite o campo)
	Janela          int                  `json:"context_window"`                  // janela em tokens; 0 = desconhecida (guardrail inativo)
	JanelaSource    string               `json:"context_window_source,omitempty"` // FonteCatalogo | FonteManual | ""
	MaxTokens       int                  `json:"max_tokens"`                      // reservado ao provider; 0 = padrão do dealer
	BytesPorToken   int                  `json:"bytes_per_token"`                 // calibragem manual; 0 = estimador padrão (4)
	BaseURL         string               `json:"-"`                               // resolvido do provider ativo
	APIKey          string               `json:"-"`                               // resolvido do provider ativo; nunca logar
	Home            string               `json:"-"`
}

// Dir é a raiz dos dados da harness (~/.barroso-harness por padrão).
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

// Provider devolve o provider de nome dado (nil se não existe).
func (c *Config) Provider(nome string) *Provider {
	if c.Providers == nil || nome == "" {
		return nil
	}
	return c.Providers[nome]
}

// Ativo devolve o provider corrente (nil se nenhum está selecionado). Os
// campos BaseURL/APIKey em memória mantêm compatibilidade com callers antigos;
// nunca são serializados e o disco continua com providers/current_provider.
func (c *Config) Ativo() *Provider {
	if p := c.Provider(c.CurrentProvider); p != nil {
		return p
	}
	if c.BaseURL != "" || c.APIKey != "" {
		return &Provider{BaseURL: c.BaseURL, APIKey: c.APIKey}
	}
	return nil
}

// SelecionarProvider troca o provider ativo, recalculando a janela quando ela
// vinha do catálogo (a da fonte manual nunca é sobrescrita).
func (c *Config) SelecionarProvider(nome string, janela int) {
	c.CurrentProvider = nome
	if c.JanelaSource == FonteCatalogo {
		c.Janela = janela
		if c.Janela <= 0 {
			c.JanelaSource = ""
		}
	}
}

// Faltas lista o que falta para a config estar pronta (usado em mensagens
// acionáveis; nunca contém a chave).
func (c Config) Faltas() []string {
	var faltas []string
	p := c.Ativo()
	if p == nil {
		faltas = append(faltas, "provider (base_url + api_key)")
	} else {
		if p.BaseURL == "" {
			faltas = append(faltas, "base_url")
		}
		if p.APIKey == "" {
			faltas = append(faltas, "api_key")
		}
	}
	if c.Model == "" {
		faltas = append(faltas, "model")
	}
	return faltas
}

// Pronta diz se a harness tem provider (URL + chave) e model suficientes para
// rodar um turno sem interação.
func (c Config) Pronta() bool { return len(c.Faltas()) == 0 }

// Resumo é a linha de estado exibida no REPL — SEM a chave.
func (c Config) Resumo() string {
	base := ""
	if p := c.Ativo(); p != nil {
		base = p.BaseURL
	}
	parte := fmt.Sprintf("provider %q @ %s", c.CurrentProvider, base)
	if c.Effort != "" {
		parte += " · effort " + c.Effort
	}
	parte += fmt.Sprintf(" · model %s", c.Model)
	if c.Janela > 0 {
		parte += fmt.Sprintf(" · janela %d (%s)", c.Janela, fonteJanela(c.JanelaSource))
	}
	return parte
}

func fonteJanela(s string) string {
	switch s {
	case FonteCatalogo:
		return FonteCatalogo
	case FonteManual:
		return FonteManual
	default:
		return "desconhecida"
	}
}

// Load lê config.json. Arquivo ausente NÃO é erro: devolve config vazia
// (o caminho interativo inicia onboarding; o turno único avisa o que falta).
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
		return c, fmt.Errorf("config %s inválida: %w", path, err)
	}
	migrarShapeAntigo(b, &c)
	c.Home = Dir()
	if p := c.Provider(c.CurrentProvider); p != nil {
		c.BaseURL, c.APIKey = p.BaseURL, p.APIKey
	}
	return c, nil
}

// migrarShapeAntigo converte a config do slice 0 ({base_url, model?} na raiz,
// chave por env) para a forma canônica com provider nomeado.
func migrarShapeAntigo(b []byte, c *Config) {
	if c.CurrentProvider != "" || len(c.Providers) != 0 {
		return // já está na forma nova
	}
	var legacy struct {
		BaseURL string `json:"base_url"`
		Model   string `json:"model"`
	}
	if err := json.Unmarshal(b, &legacy); err != nil || legacy.BaseURL == "" {
		return
	}
	nome := "keys.barroso"
	if strings.TrimSuffix(legacy.BaseURL, "/") != DefaultBaseURL {
		nome = "default"
	}
	c.Providers = map[string]*Provider{
		nome: {BaseURL: strings.TrimSuffix(legacy.BaseURL, "/"), APIKey: ""},
	}
	c.CurrentProvider = nome
	if c.Model == "" {
		c.Model = legacy.Model
	}
	if c.JanelaSource == "" && c.Janela > 0 {
		c.JanelaSource = FonteManual
	}
}

// Save persiste a config com atomicidade: arquivo temporário no mesmo
// diretório (permissão 0600), rename por cima. Diretório criado com 0700.
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
	_ = os.Chmod(tmp, 0o600) // best-effort; Windows ignora bits POSIX
	if err := atomicReplace(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// Init cria a estrutura de dados de forma NÃO destrutiva: diretório 0700,
// BARROSO.md global de exemplo e um esqueleto de config.json (que o
// onboarding de `bh` completa). Nada existente é sobrescrito.
func Init() error {
	dir := Dir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	barPath := filepath.Join(dir, "BARROSO.md")
	if _, err := os.Stat(barPath); errors.Is(err, os.ErrNotExist) {
		exemplo := "# BARROSO.md global\n\nInstruções que valem para TODAS as sessões da harness, em qualquer diretório.\n" +
			"BARROSO.md por projeto (na pasta do projeto ou nos pais) entra antes deste.\n"
		if err := os.WriteFile(barPath, []byte(exemplo), 0o644); err != nil {
			return err
		}
	}
	cfgPath := filepath.Join(dir, "config.json")
	if _, err := os.Stat(cfgPath); errors.Is(err, os.ErrNotExist) {
		return Config{Home: dir}.Save()
	}
	return nil
}
