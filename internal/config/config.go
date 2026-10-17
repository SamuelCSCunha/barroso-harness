// Package config guarda a configuração mínima da harness: endpoint no padrão
// Chat Completions da OpenAI (base_url + api_key + model). A chave NUNCA vive
// no arquivo — entra por BARROSO_API_KEY no ambiente.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type Config struct {
	BaseURL       string `json:"base_url"`
	Model         string `json:"model"`
	Janela        int    `json:"context_window"`   // janela manual em tokens; 0 = desconhecida
	MaxTokens     int    `json:"max_tokens"`       // reservado ao provider; 0 = padrão do dealer
	BytesPorToken int    `json:"bytes_per_token"`  // calibragem manual; 0 = estimador padrão (4)
	APIKey        string `json:"-"`
	Home          string `json:"-"`
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

// Load lê config.json e resolve overrides de ambiente. base_url e model são
// sempre requeridos; BARROSO_API_KEY é obrigatória no ambiente.
func Load() (Config, error) {
	c := Config{Home: Dir()}
	path := filepath.Join(c.Home, "config.json")
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(b, &c); err != nil {
			return c, fmt.Errorf("config %s: %w", path, err)
		}
	case errors.Is(err, os.ErrNotExist):
		return c, fmt.Errorf("sem config: crie %s com {\"base_url\": \"https://...\", \"model\": \"...\"} e exporte BARROSO_API_KEY (ou rode barroso-harness -init)", path)
	default:
		return c, err
	}
	if v := os.Getenv("BARROSO_BASE_URL"); v != "" {
		c.BaseURL = v
	}
	if v := os.Getenv("BARROSO_MODEL"); v != "" {
		c.Model = v
	}
	c.APIKey = os.Getenv("BARROSO_API_KEY")
	c.Home = Dir()
	if err := c.validar(); err != nil {
		return c, err
	}
	return c, nil
}

func (c Config) validar() error {
	if c.BaseURL == "" || c.Model == "" {
		return fmt.Errorf("config incompleta: base_url e model são obrigatórios (arquivo %s ou env BARROSO_BASE_URL/BARROSO_MODEL)", filepath.Join(Dir(), "config.json"))
	}
	if c.APIKey == "" {
		return errors.New("BARROSO_API_KEY não definida no ambiente; a chave nunca vive no config.json")
	}
	return nil
}

// Init cria os arquivos de exemplo (config.json sem segredo e BARROSO.md
// global vazio), sem sobrescrever nada que já exista.
func Init() error {
	dir := Dir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	cfgPath := filepath.Join(dir, "config.json")
	if _, err := os.Stat(cfgPath); errors.Is(err, os.ErrNotExist) {
		exemplo := struct {
			BaseURL string `json:"base_url"`
			Model   string `json:"model"`
		}{BaseURL: "https://seu-endpoint.example/v1", Model: "seu-modelo"}
		b, _ := json.MarshalIndent(exemplo, "", "  ")
		if err := os.WriteFile(cfgPath, append(b, '\n'), 0o644); err != nil {
			return err
		}
	}
	barPath := filepath.Join(dir, "BARROSO.md")
	if _, err := os.Stat(barPath); errors.Is(err, os.ErrNotExist) {
		exemplo := "# BARROSO.md global\n\nInstruções que valem para TODAS as sessões da harness, em qualquer diretório.\n" +
			"BARROSO.md por projeto (na pasta do projeto ou nos pais) entra antes deste.\n"
		return os.WriteFile(barPath, []byte(exemplo), 0o644)
	}
	return nil
}
