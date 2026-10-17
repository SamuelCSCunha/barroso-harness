// Package tools — contrato das ferramentas built-in. Este arquivo guarda os
// tipos e as helpers; as specs vivem em specs.go.
package tools

import (
	"context"
	"encoding/json"
)

// Call é uma chamada recebida (não usado no slice 0; entra com sessão).
type Call struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Raw  json.RawMessage `json:"arguments"`
}

// Result de uma execução.
type Result struct {
	CallID  string `json:"call_id"`
	Name    string `json:"name"`
	Output  string `json:"output"`
	IsError bool   `json:"is_error"`
}

// Spec descreve uma ferramenta: contrato exposto ao provider + executor.
type Spec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Schema      map[string]any `json:"parameters"`
	Exec        func(ctx context.Context, cwd string, raw json.RawMessage) (string, error)
}
