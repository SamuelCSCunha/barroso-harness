package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ── fake stdio MCP subprocess ────────────────────────────────────────────

// envHelper is the variable that enables the subprocess's "fake MCP server"
// mode. The test sets it on the child; without it the test binary runs
// normally.
const envHelper = "BARROSO_MCP_TEST_HELPER"

// envSilent enables the subprocess's "server that never replies" mode (used
// by the handshake test that times out).
const envSilent = "BARROSO_MCP_TEST_SILENT"

// TestHelperProcess is NOT a real test: it is the server process the stdio
// tests launch. It runs outside the normal `go test` flow (the parent binary
// only invokes it with -test.run=TestHelperProcess), so the env guard keeps
// it from running when the package is tested normally.
//
// It speaks MCP as newline-delimited JSON on stdin/stdout — the same format a
// real MCP server uses. No Content-Length: that belongs to LSP, and MCP
// stdio deliberately adopted newlines.
func TestHelperProcess(t *testing.T) {
	if os.Getenv(envHelper) == "" {
		t.Skip("stdio test helper; only runs as a subprocess")
	}

	// "Silent" mode: the process reads its input and NEVER replies. It
	// exercises the handshake path that times out — the client has to give up
	// on its own and still clean up the subprocess.
	if os.Getenv(envSilent) != "" {
		drain := bufio.NewScanner(os.Stdin)
		for drain.Scan() {
		}
		return
	}

	// Notifications emitted BEFORE the first response: the client has to
	// correlate by id and simply ignore them.
	emitNotification("notifications/progress", map[string]any{
		"progressToken": "fake",
		"message":       "waking up",
	})

	in := bufio.NewScanner(os.Stdin)
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()

	for in.Scan() {
		line := strings.TrimSpace(in.Text())
		if line == "" {
			continue
		}
		var m message
		if json.Unmarshal([]byte(line), &m) != nil {
			continue
		}

		switch m.Method {
		case "initialize":
			reply(out, idOf(m.ID), map[string]any{
				"protocolVersion": "2025-06-18",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "fake-stdio", "version": "9"},
			})

		case "notifications/initialized":
			// Notification: nothing to reply.

		case "tools/list":
			// Noise before the response: a non-JSON line and a notification.
			fmt.Fprintln(out, "non-json noise from the server")
			emitNotification("notifications/message", map[string]any{"level": "info"})
			reply(out, idOf(m.ID), map[string]any{
				"tools": []any{
					map[string]any{
						"name":        "echo",
						"description": "Returns the received text",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"msg": map[string]any{"type": "string"},
							},
							"required": []string{"msg"},
						},
					},
					map[string]any{
						"name":        "fail",
						"description": "Always reports an error",
						"inputSchema": map[string]any{"type": "object"},
					},
					map[string]any{
						"name":        "no-schema",
						"description": "No inputSchema at all",
					},
				},
			})

		case "tools/call":
			var p paramsToolsCall
			_ = json.Unmarshal(m.Params, &p)
			switch p.Name {
			case "echo":
				msg := ""
				if v, ok := p.Arguments["msg"].(string); ok {
					msg = v
				}
				reply(out, idOf(m.ID), map[string]any{
					"content": []any{
						map[string]any{"type": "text", "text": "echo: " + msg},
					},
				})
			case "fail":
				reply(out, idOf(m.ID), map[string]any{
					"isError": true,
					"content": []any{
						map[string]any{"type": "text", "text": "failed on purpose"},
					},
				})
			default:
				replyError(out, idOf(m.ID), -32602, "unknown tool: "+p.Name)
			}

		default:
			replyError(out, idOf(m.ID), -32601, "unknown method: "+m.Method)
		}
	}
}

// idOf echoes the request's raw id (number or string) back, as JSON-RPC
// requires: the response carries the request id, not an invented one.
func idOf(raw json.RawMessage) any {
	return json.RawMessage(raw)
}

func reply(w *bufio.Writer, id any, result any) {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	// newline is the MCP stdio message delimiter.
	fmt.Fprintf(w, "%s\n", b)
	_ = w.Flush()
}

func replyError(w *bufio.Writer, id any, code int, msg string) {
	b, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": msg},
	})
	fmt.Fprintf(w, "%s\n", b)
	_ = w.Flush()
}

// emitNotification writes a notification (no id) — the client must ignore it.
func emitNotification(method string, params any) {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
	fmt.Printf("%s\n", b)
}

// fakeStdioServer builds the Servidor that points at this same test binary,
// in helper mode. The child env carries envHelper — plus a marker the helper
// uses to know it is running under test.
func fakeStdioServer(name string) Servidor {
	return Servidor{
		Nome:    name,
		Tipo:    "stdio",
		Command: os.Args[0],
		Args:    []string{"-test.run=TestHelperProcess"},
		Env: map[string]string{
			envHelper:                "1",
			"GO_WANT_HELPER_PROCESS": "1",
		},
		Timeout: 10,
	}
}

// ── stdio transport tests ────────────────────────────────────────────────

// Full handshake: initialize (with notifications before it), tools/list.
func TestStdioHandshakeAndList(t *testing.T) {
	c, err := Novo(fakeStdioServer("fake-stdio"))
	if err != nil {
		t.Fatalf("Novo: %v", err)
	}
	ctx := context.Background()
	if err := c.Abre(ctx); err != nil {
		t.Fatalf("Abre (stdio): %v", err)
	}
	defer c.Fecha()

	if c.Transporto() != "stdio" {
		t.Errorf("Transporto() = %q, want stdio", c.Transporto())
	}
	info := c.InfoServidor()
	if info.ServerInfo.Name != "fake-stdio" {
		t.Errorf("ServerInfo.Name = %q; want fake-stdio", info.ServerInfo.Name)
	}

	specs, err := c.Ferramentas(ctx)
	if err != nil {
		t.Fatalf("Ferramentas (stdio): %v", err)
	}
	if len(specs) != 3 {
		t.Fatalf("Ferramentas returned %d; want 3", len(specs))
	}
	names := map[string]bool{}
	for _, s := range specs {
		names[s.Name] = true
	}
	for _, w := range []string{"echo", "fail", "no-schema"} {
		if !names[w] {
			t.Errorf("tool %q missing from the catalog", w)
		}
	}
	// The one without a schema got the free object.
	for _, s := range specs {
		if s.Name == "no-schema" && s.Schema["type"] != "object" {
			t.Errorf("no-schema.Schema = %#v", s.Schema)
		}
	}
}

// tools/call through the Spec path (what the runtime actually executes).
func TestStdioCallEchoesArguments(t *testing.T) {
	c, err := Novo(fakeStdioServer("fake-stdio"))
	if err != nil {
		t.Fatalf("Novo: %v", err)
	}
	ctx := context.Background()
	if err := c.Abre(ctx); err != nil {
		t.Fatalf("Abre: %v", err)
	}
	defer c.Fecha()

	out, err := c.Chamar(ctx, "echo", `{"msg":"héllo stdio"}`)
	if err != nil {
		t.Fatalf("Chamar: %v", err)
	}
	if out != "echo: héllo stdio" {
		t.Errorf("Chamar = %q; want %q", out, "echo: héllo stdio")
	}

	// Empty arguments become {} and the server replies "echo: ".
	out, err = c.Chamar(ctx, "echo", "")
	if err != nil {
		t.Fatalf("Chamar with empty args: %v", err)
	}
	if out != "echo: " {
		t.Errorf("empty Chamar = %q", out)
	}
}

// A server isError becomes a Go error carrying its message.
func TestStdioIsError(t *testing.T) {
	c, err := Novo(fakeStdioServer("fake-stdio"))
	if err != nil {
		t.Fatalf("Novo: %v", err)
	}
	ctx := context.Background()
	if err := c.Abre(ctx); err != nil {
		t.Fatalf("Abre: %v", err)
	}
	defer c.Fecha()

	out, err := c.Chamar(ctx, "fail", "{}")
	if err == nil {
		t.Fatalf("Chamar should return an error")
	}
	if !strings.Contains(err.Error(), "failed on purpose") {
		t.Errorf("error = %q; want the server's message", err.Error())
	}
	if !strings.Contains(out, "failed on purpose") {
		t.Errorf("output = %q", out)
	}
}

// A JSON-RPC error (different from isError) also becomes a Go error.
func TestStdioJSONRPCError(t *testing.T) {
	c, err := Novo(fakeStdioServer("fake-stdio"))
	if err != nil {
		t.Fatalf("Novo: %v", err)
	}
	ctx := context.Background()
	if err := c.Abre(ctx); err != nil {
		t.Fatalf("Abre: %v", err)
	}
	defer c.Fecha()

	_, err = c.Chamar(ctx, "does-not-exist", "{}")
	if err == nil {
		t.Fatalf("Chamar on a nonexistent tool should fail")
	}
	if !strings.Contains(err.Error(), "unknown tool") {
		t.Errorf("error = %q", err.Error())
	}
}

// Several calls in sequence: each with its own id, response matched.
func TestStdioSequentialCalls(t *testing.T) {
	c, err := Novo(fakeStdioServer("fake-stdio"))
	if err != nil {
		t.Fatalf("Novo: %v", err)
	}
	ctx := context.Background()
	if err := c.Abre(ctx); err != nil {
		t.Fatalf("Abre: %v", err)
	}
	defer c.Fecha()

	for i := 0; i < 6; i++ {
		want := fmt.Sprintf("echo: msg-%d", i)
		out, err := c.Chamar(ctx, "echo", fmt.Sprintf(`{"msg":"msg-%d"}`, i))
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if out != want {
			t.Errorf("call %d = %q; want %q", i, out, want)
		}
	}
}

// Parallel calls: distinct ids must correlate without getting swapped.
func TestStdioConcurrentCalls(t *testing.T) {
	c, err := Novo(fakeStdioServer("fake-stdio"))
	if err != nil {
		t.Fatalf("Novo: %v", err)
	}
	ctx := context.Background()
	if err := c.Abre(ctx); err != nil {
		t.Fatalf("Abre: %v", err)
	}
	defer c.Fecha()

	const n = 12
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			want := fmt.Sprintf("echo: par-%d", i)
			out, err := c.Chamar(ctx, "echo", fmt.Sprintf(`{"msg":"par-%d"}`, i))
			if err != nil {
				errs <- fmt.Errorf("par-%d: %w", i, err)
				return
			}
			if out != want {
				errs <- fmt.Errorf("par-%d: swapped response: %q", i, out)
				return
			}
			errs <- nil
		}(i)
	}
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Errorf("%v", err)
		}
	}
}

// Fecha ends the subprocess: EOF on stdin makes the helper exit; Wait
// happens within the grace period (5 s), without relying on Kill.
func TestStdioFechaEndsSubprocess(t *testing.T) {
	c, err := Novo(fakeStdioServer("fake-stdio"))
	if err != nil {
		t.Fatalf("Novo: %v", err)
	}
	if err := c.Abre(context.Background()); err != nil {
		t.Fatalf("Abre: %v", err)
	}

	pid := c.cmd.Process.Pid
	start := time.Now()
	if err := c.Fecha(); err != nil {
		t.Errorf("Fecha: %v", err)
	}
	if elapsed := time.Since(start); elapsed > killGracePeriod+3*time.Second {
		t.Errorf("Fecha took %v; want it to exit via EOF, not Kill", elapsed)
	}

	// Closing twice is a no-op.
	if err := c.Fecha(); err != nil {
		t.Errorf("repeated Fecha: %v", err)
	}

	// The process no longer exists.
	if err := processAlive(pid); err == nil {
		t.Errorf("subprocess %d still alive after Fecha", pid)
	}

	// Chamar after closing returns an error; it neither hangs nor panics.
	_, err = c.Chamar(context.Background(), "echo", "{}")
	if err == nil {
		t.Errorf("Chamar after Fecha should fail")
	}
}

// Nonexistent command: ONE-line error, no subprocess to clean up.
func TestStdioNonexistentCommand(t *testing.T) {
	c, err := Novo(Servidor{
		Nome: "ghost",
		Tipo: "stdio",
		// Absolute path that does not exist: fails at Start, not in PATH lookup.
		Command: filepath.Join(t.TempDir(), "does-not-exist-abc123"),
		Timeout: 5,
	})
	if err != nil {
		t.Fatalf("Novo: %v", err)
	}
	if err := c.Abre(context.Background()); err == nil {
		t.Fatalf("Abre should fail with a nonexistent command")
	} else if strings.Contains(err.Error(), "\n") {
		t.Errorf("multi-line error: %q", err.Error())
	}
}

// Context already cancelled: Abre returns the ctx error before starting a process.
func TestStdioCancelledContext(t *testing.T) {
	c, err := Novo(fakeStdioServer("fake-stdio"))
	if err != nil {
		t.Fatalf("Novo: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Abre(ctx); err == nil {
		t.Errorf("Abre with a cancelled ctx should fail")
	}
}

// Failed handshake: the subprocess must be cleaned up (no zombie).
func TestStdioFailedHandshakeCleansUpSubprocess(t *testing.T) {
	// Subprocess that does not speak MCP: reads its input and never replies.
	c, err := Novo(Servidor{
		Nome:    "silent",
		Tipo:    "stdio",
		Command: os.Args[0],
		Args:    []string{"-test.run=TestHelperProcess"},
		Env: map[string]string{
			envHelper:                "1",
			"GO_WANT_HELPER_PROCESS": "1",
			envSilent:                "1",
		},
		Timeout: 3,
	})
	if err != nil {
		t.Fatalf("Novo: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := c.Abre(ctx); err == nil {
		t.Fatalf("Abre should fail with a silent server")
	}
	// After Abre fails no process may remain: Fecha on the handshake error
	// path guarantees that.
	if err := processAlive(c.cmd.Process.Pid); err == nil {
		t.Errorf("zombie subprocess after failed handshake")
	}
}

// processAlive checks whether the pid exists. Windows has no signal 0, so it
// uses tasklist, which is portable enough for the test.
func processAlive(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("invalid pid")
	}
	cmd := exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/NH")
	out, err := cmd.Output()
	if err != nil {
		return err
	}
	if strings.Contains(string(out), fmt.Sprintf("%d", pid)) {
		return nil
	}
	return fmt.Errorf("no process %d", pid)
}
