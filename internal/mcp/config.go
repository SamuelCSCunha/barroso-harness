// Package mcp is the harness's MCP (Model Context Protocol) client. It talks
// to MCP servers over the two transports defined by the protocol — stdio
// (subprocess, newline-delimited JSON-RPC on stdin/stdout) and streamable
// HTTP (JSON-RPC POST, response as plain JSON or SSE) — and converts the
// tools published by the server into harness tools.Spec values, so the
// provider sees them like any other built-in tool.
//
// No external dependencies: standard library only.
package mcp

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Servidor is a configured MCP server. Tipo selects the transport:
//   - "stdio": Command (+Args/Env) is the subprocess to launch;
//   - "http": URL is the streamable HTTP endpoint (Headers carries auth etc.).
//
// Timeout is in SECONDS and applies to each JSON-RPC call; 0 = default.
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

// Transport types accepted in Servidor.Tipo (case-insensitive; dashes and
// spaces are tolerated outside the protocol, to be forgiving in hand-typed
// config).
const (
	transportStdio = "stdio"
	transportHTTP  = "http"
)

// timeoutDuration returns the per-call cap. Never returns 0.
func (s Servidor) timeoutDuration() time.Duration {
	if s.Timeout > 0 {
		return time.Duration(s.Timeout) * time.Second
	}
	return defaultCallTimeout
}

// normalizeTransport maps Tipo to its canonical form ("stdio" | "http").
func normalizeTransport(t string) (string, error) {
	n := strings.ToLower(strings.TrimSpace(t))
	n = strings.NewReplacer("_", "-", " ", "-").Replace(n)
	switch n {
	case transportStdio:
		return transportStdio, nil
	case transportHTTP, "streamable-http", "http-streamable", "http-streaming", "streamablehttp":
		return transportHTTP, nil
	default:
		return "", fmt.Errorf("unknown MCP transport type: %q (use %q or %q)", t, transportStdio, transportHTTP)
	}
}

// validate checks the minimum needed to open the chosen transport. Returns a
// ONE-line error without echoing secrets (Env and Headers are referenced by
// key only).
func (s Servidor) validate() error {
	if strings.TrimSpace(s.Nome) == "" {
		return fmt.Errorf("MCP server without a name")
	}
	transport, err := normalizeTransport(s.Tipo)
	if err != nil {
		return err
	}
	switch transport {
	case transportStdio:
		if strings.TrimSpace(s.Command) == "" {
			return fmt.Errorf("server %q: type %q requires command", s.Nome, transportStdio)
		}
	case transportHTTP:
		if strings.TrimSpace(s.URL) == "" {
			return fmt.Errorf("server %q: type %q requires url", s.Nome, transportHTTP)
		}
	}
	return nil
}

// envSlice converts the environment map into K=V pairs, sorted by name so
// map ordering does not leak into the subprocess.
func envSlice(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out
}
