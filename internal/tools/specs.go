// Specs of the slice 0 built-in tools. YOLO mode: no permission gate;
// technical limits (timeout, output cap) are operational, not authorization.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	outputCap      = 64 << 10                               // output cap per tool call
	defaultTimeout = 120 * time.Second                      // terminal_run
	prunedTail     = "(content compacted by the guardrail)" // reserved for runtime
)

// SpecsBuiltins returns the full catalog. Order is stable across platforms.
func SpecsBuiltins() []Spec {
	return []Spec{
		specTerminalRun(),
		specFileRead(),
		specFileWrite(),
		specFileAppend(),
		specFileCopy(),
		specListDir(),
		specCtxRead(),
	}
}

// ── helpers ──────────────────────────────────────────────────────────────

func str(m map[string]any, k string) string { s, _ := m[k].(string); return s }

func intOr(m map[string]any, k string, fb int) int {
	if f, ok := m[k].(float64); ok {
		return int(f)
	}
	return fb
}

// resolvePath resolves relative to cwd; in YOLO there is no sandbox — the
// local process privileges apply.
func resolvePath(cwd, p string) string {
	if p == "" {
		return cwd
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(cwd, filepath.Clean(p))
}

// truncate applies the cap with a marker at the end.
func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + fmt.Sprintf("\n\n[output truncated at %d bytes; read with smaller start/limit for the rest]", limit)
}

// ── terminal_run ─────────────────────────────────────────────────────────

func specTerminalRun() Spec {
	return Spec{
		Name: "terminal_run",
		Description: "Runs a command in the local machine's terminal (YOLO mode, no approval prompt). " +
			"Returns merged stdout+stderr with exit_code. Default timeout 120 s (timeout_secs overrides).",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command":      map[string]any{"type": "string", "description": "command to run"},
				"workdir":      map[string]any{"type": "string", "description": "working directory (default: the agent's cwd)"},
				"timeout_secs": map[string]any{"type": "integer", "description": "timeout in seconds; default 120"},
			},
			"required": []string{"command"},
		},
		Exec: func(ctx context.Context, cwd string, raw json.RawMessage) (string, error) {
			var c struct {
				Command string `json:"command"`
				Workdir string `json:"workdir"`
				Timeout int    `json:"timeout_secs"`
			}
			if err := json.Unmarshal(raw, &c); err != nil {
				return "", fmt.Errorf("invalid input: %w", err)
			}
			c.Command = strings.TrimSpace(c.Command)
			if c.Command == "" {
				return "", fmt.Errorf("empty command")
			}
			dir := cwd
			if c.Workdir != "" {
				dir = resolvePath(cwd, c.Workdir)
			}
			timeout := defaultTimeout
			if c.Timeout > 0 {
				timeout = time.Duration(c.Timeout) * time.Second
			}
			cctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()

			var cmd *exec.Cmd
			if runtime.GOOS == "windows" {
				cmd = exec.CommandContext(cctx, "cmd", "/C", c.Command)
			} else {
				cmd = exec.CommandContext(cctx, "bash", "-c", c.Command)
			}
			cmd.Dir = dir
			out, exeErr := cmd.CombinedOutput()
			code := -1
			if cmd.ProcessState != nil {
				code = cmd.ProcessState.ExitCode()
			}
			res := fmt.Sprintf("exit_code: %d\nstdout+stderr:\n%s", code, string(out))
			if exeErr != nil && len(out) == 0 {
				res = fmt.Sprintf("exit_code: %d\nstart failed: %v", code, exeErr)
			}
			return truncate(res, outputCap), nil
		},
	}
}

// ── file_read ────────────────────────────────────────────────────────────

func specFileRead() Spec {
	return Spec{
		Name:        "file_read",
		Description: "Reads a file with line numbers. For large files, read windows: start (first line, 1-based) + limit; the result states the delivered range and the total.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":  map[string]any{"type": "string"},
				"start": map[string]any{"type": "integer", "description": "first line (1-based)"},
				"limit": map[string]any{"type": "integer", "description": "maximum number of lines in this read"},
			},
			"required": []string{"path"},
		},
		Exec: func(_ context.Context, cwd string, raw json.RawMessage) (string, error) {
			var c struct {
				Path  string `json:"path"`
				Start int    `json:"start"`
				Limit int    `json:"limit"`
			}
			if err := json.Unmarshal(raw, &c); err != nil {
				return "", fmt.Errorf("invalid input: %w", err)
			}
			if c.Path == "" {
				return "", fmt.Errorf("path is required")
			}
			b, err := os.ReadFile(resolvePath(cwd, c.Path))
			if err != nil {
				return "", err
			}
			lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
			total := len(lines)
			first := 0
			if c.Start > 1 {
				first = c.Start - 1
			}
			end := total
			if c.Limit > 0 && first+c.Limit < end {
				end = first + c.Limit
			}
			if first >= total {
				return fmt.Sprintf("file has %d lines (start=%d)", total, c.Start), nil
			}
			var sb strings.Builder
			fmt.Fprintf(&sb, "lines %d–%d of %d\n", first+1, end, total)
			for i := first; i < end; i++ {
				line := lines[i]
				if len(line) > 512 {
					line = line[:512] + "…"
				}
				fmt.Fprintf(&sb, "%d: %s\n", i+1, line)
			}
			return truncate(sb.String(), outputCap), nil
		},
	}
}

// ── file_write / file_append ─────────────────────────────────────────────

func specFileWrite() Spec {
	return Spec{
		Name:        "file_write",
		Description: "Writes (overwrites) a file. Creates any missing parent directories.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":    map[string]any{"type": "string"},
				"content": map[string]any{"type": "string"},
			},
			"required": []string{"path", "content"},
		},
		Exec: func(_ context.Context, cwd string, raw json.RawMessage) (string, error) {
			var c struct {
				Path    string `json:"path"`
				Content string `json:"content"`
			}
			if err := json.Unmarshal(raw, &c); err != nil {
				return "", fmt.Errorf("invalid input: %w", err)
			}
			if c.Path == "" {
				return "", fmt.Errorf("path is required")
			}
			p := resolvePath(cwd, c.Path)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return "", err
			}
			if err := os.WriteFile(p, []byte(c.Content), 0o644); err != nil {
				return "", err
			}
			return fmt.Sprintf("ok: %s (%d bytes)", c.Path, len(c.Content)), nil
		},
	}
}

func specFileAppend() Spec {
	return Spec{
		Name:        "file_append",
		Description: "Appends content to the end of a file (creates it if missing).",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":    map[string]any{"type": "string"},
				"content": map[string]any{"type": "string"},
			},
			"required": []string{"path", "content"},
		},
		Exec: func(_ context.Context, cwd string, raw json.RawMessage) (string, error) {
			var c struct {
				Path    string `json:"path"`
				Content string `json:"content"`
			}
			if err := json.Unmarshal(raw, &c); err != nil {
				return "", fmt.Errorf("invalid input: %w", err)
			}
			if c.Path == "" {
				return "", fmt.Errorf("path is required")
			}
			p := resolvePath(cwd, c.Path)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return "", err
			}
			f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
			if err != nil {
				return "", err
			}
			defer f.Close()
			if _, err := f.WriteString(c.Content); err != nil {
				return "", err
			}
			return fmt.Sprintf("ok: appended to %s", c.Path), nil
		},
	}
}

// ── file_copy ────────────────────────────────────────────────────────────

func specFileCopy() Spec {
	return Spec{
		Name:        "file_copy",
		Description: "Copies a file to another path (overwrites the destination). Uses the system's privileges (cp/copy) — in YOLO there is no sandbox.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"from": map[string]any{"type": "string"},
				"to":   map[string]any{"type": "string"},
			},
			"required": []string{"from", "to"},
		},
		Exec: func(_ context.Context, cwd string, raw json.RawMessage) (string, error) {
			var c struct {
				From string `json:"from"`
				To   string `json:"to"`
			}
			if err := json.Unmarshal(raw, &c); err != nil {
				return "", fmt.Errorf("invalid input: %w", err)
			}
			if c.From == "" || c.To == "" {
				return "", fmt.Errorf("from and to are required")
			}
			var cmd *exec.Cmd
			if runtime.GOOS == "windows" {
				cmd = exec.Command("cmd", "/C", "copy", "/Y", resolvePath(cwd, c.From), resolvePath(cwd, c.To))
			} else {
				cmd = exec.Command("cp", resolvePath(cwd, c.From), resolvePath(cwd, c.To))
			}
			cmd.Dir = cwd
			out, err := cmd.CombinedOutput()
			if err != nil {
				return fmt.Sprintf("failed: %v\n%s", err, string(out)), nil
			}
			return fmt.Sprintf("ok: %s → %s", c.From, c.To), nil
		},
	}
}

// ── list_dir ─────────────────────────────────────────────────────────────

func specListDir() Spec {
	return Spec{
		Name:        "list_dir",
		Description: "Lists a directory (default: cwd), recursively down to depth (default 1, max 3). Directories end with '/'; files show their size.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":  map[string]any{"type": "string"},
				"depth": map[string]any{"type": "integer", "description": "depth; default 1, maximum 3"},
			},
		},
		Exec: func(_ context.Context, cwd string, raw json.RawMessage) (string, error) {
			var c struct {
				Path  string `json:"path"`
				Depth int    `json:"depth"`
			}
			_ = json.Unmarshal(raw, &c)
			root := cwd
			if c.Path != "" {
				root = resolvePath(cwd, c.Path)
			}
			if c.Depth <= 0 {
				c.Depth = 1
			}
			if c.Depth > 3 {
				c.Depth = 3
			}
			var sb strings.Builder
			var walk func(p string, lvl int)
			walk = func(p string, lvl int) {
				ents, err := os.ReadDir(p)
				if err != nil {
					fmt.Fprintf(&sb, "(error reading %s: %v)\n", p, err)
					return
				}
				for _, e := range ents {
					name := e.Name()
					if strings.HasPrefix(name, ".") {
						continue
					}
					full := filepath.Join(p, name)
					if e.IsDir() {
						fmt.Fprintf(&sb, "%s/\n", full)
						if lvl < c.Depth {
							walk(full, lvl+1)
						}
						continue
					}
					info, e2 := e.Info()
					if e2 != nil {
						fmt.Fprintf(&sb, "%s (error: %v)\n", full, e2)
						continue
					}
					fmt.Fprintf(&sb, "%s (%d bytes)\n", full, info.Size())
				}
			}
			walk(root, 1)
			return truncate(sb.String(), outputCap), nil
		},
	}
}

// ── ctx_read (the agent's own live context file) ─────────────────────────

func specCtxRead() Spec {
	return Spec{
		Name:        "ctx_read",
		Description: "Reads the session's live context file (what the agent is doing, decisions and pending items). In slice 0, the context is BARROSO.md + the transcript itself; this returns a snapshot of the state.",
		Schema:      map[string]any{"type": "object", "properties": map[string]any{}},
		Exec: func(_ context.Context, cwd string, _ json.RawMessage) (string, error) {
			content, _ := liveContext(cwd)
			return truncate(strings.TrimSpace("current live context:\n"+content), outputCap), nil
		},
	}
}

// liveContext returns an informative template; the editable version
// (ctx_write/ctx_append) arrives with persistent sessions in slice 1.
func liveContext(cwd string) (string, error) {
	b, err := os.ReadFile(filepath.Join(cwd, "BARROSO.md"))
	if err != nil {
		return "(no BARROSO.md in the project)", nil
	}
	return string(b), nil
}
