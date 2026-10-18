// Package mcp é o cliente MCP (Model Context Protocol) da harness. Fala com
// servidores MCP pelos dois transportes previstos no protocolo — stdio
// (subprocesso, JSON-RPC delimitado por newline em stdin/stdout) e HTTP
// streamable (POST do JSON-RPC, resposta em JSON puro ou SSE) — e converte as
// ferramentas publicadas pelo servidor em tools.Spec da harness, para que o
// provider as enxergue como qualquer outra ferramenta built-in.
//
// Sem dependências externas: só a biblioteca padrão.
package mcp

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Servidor é um servidor MCP configurado. Tipo decide o transporte:
//   - "stdio": Command (+Args/Env) é o subprocesso a abrir;
//   - "http": URL é o endpoint streamable HTTP (Headers carrega auth etc.).
//
// Timeout é em SEGUNDOS e vale para cada chamada JSON-RPC; 0 = padrão.
type Servidor struct {
	Nome    string            `json:"name"`
	Tipo    string            `json:"type"` // "stdio" | "http"
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Timeout int               `json:"timeout_segundos,omitempty"` // default 120
}

// Tipos de transporte aceitos em Servidor.Tipo (case-insensitive; Traço e
// espaço são tolerados por fora do protocolo, para caber em config digitar).
const (
	tipoStdio = "stdio"
	tipoHTTP  = "http"
)

// timeoutDuracao devolve o teto por chamada. Nunca retorna 0.
func (s Servidor) timeoutDuracao() time.Duration {
	if s.Timeout > 0 {
		return time.Duration(s.Timeout) * time.Second
	}
	return timeoutPorChamada
}

// normalizarTipo traduz o Tipo para a forma canônica ("stdio" | "http").
func normalizarTipo(t string) (string, error) {
	n := strings.ToLower(strings.TrimSpace(t))
	n = strings.NewReplacer("_", "-", " ", "-").Replace(n)
	switch n {
	case tipoStdio:
		return tipoStdio, nil
	case tipoHTTP, "streamable-http", "http-streamable", "http-streaming", "streamablehttp":
		return tipoHTTP, nil
	default:
		return "", fmt.Errorf("tipo de transporte MCP desconhecido: %q (use %q ou %q)", t, tipoStdio, tipoHTTP)
	}
}

// validar checa o mínimo para abrir o transporte escolhido. Devolve erro de
// UMA linha, sem ecoar segredos (Env e Headers são citados por chave apenas).
func (s Servidor) validar() error {
	if strings.TrimSpace(s.Nome) == "" {
		return fmt.Errorf("servidor MCP sem nome")
	}
	tipo, err := normalizarTipo(s.Tipo)
	if err != nil {
		return err
	}
	switch tipo {
	case tipoStdio:
		if strings.TrimSpace(s.Command) == "" {
			return fmt.Errorf("servidor %q: tipo %q exige command", s.Nome, tipoStdio)
		}
	case tipoHTTP:
		if strings.TrimSpace(s.URL) == "" {
			return fmt.Errorf("servidor %q: tipo %q exige url", s.Nome, tipoHTTP)
		}
	}
	return nil
}

// envSlice converte o mapa de ambiente em pares K=V, classificados por nome
// para a ordem do mapa não vazar para o subprocesso.
func envSlice(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	chaves := make([]string, 0, len(env))
	for k := range env {
		chaves = append(chaves, k)
	}
	sort.Strings(chaves)
	out := make([]string, 0, len(chaves))
	for _, k := range chaves {
		out = append(out, k+"="+env[k])
	}
	return out
}
