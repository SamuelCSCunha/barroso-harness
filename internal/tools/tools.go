// Package tools — contract of the built-in tools. This file holds the types
// and helpers; the specs live in specs.go.
package tools

import (
	"context"
	"encoding/json"
)

// Call is a received call (unused in slice 0; arrives with sessions).
type Call struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Raw  json.RawMessage `json:"arguments"`
}

// Result of an execution.
type Result struct {
	CallID  string `json:"call_id"`
	Name    string `json:"name"`
	Output  string `json:"output"`
	IsError bool   `json:"is_error"`
}

// Spec describes a tool: the contract exposed to the provider + executor.
type Spec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Schema      map[string]any `json:"parameters"`
	Exec        func(ctx context.Context, cwd string, raw json.RawMessage) (string, error)
}
