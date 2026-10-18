// Specs das tools built-in do slice 0. Modo YOLO: sem gate de permissão;
// limites técnicos (timeout, teto de saída) são operacionais, não autorização.
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
	tetoSaida     = 64 << 10                               // teto de saída por tool call
	timeoutPadrao = 120 * time.Second                      // terminal_run
	caudaPodada   = "(conteúdo compactado pelo guardrail)" // reservado p/ runtime
)

// SpecsBuiltins devolve o catálogo completo. Ordem estável entre placas.
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

func texto(m map[string]any, k string) string { s, _ := m[k].(string); return s }

func inteiro(m map[string]any, k string, fb int) int {
	if f, ok := m[k].(float64); ok {
		return int(f)
	}
	return fb
}

// caminho resolve relativo ao cwd; em YOLO não há sandbox — privilégios do
// processo local valem.
func caminho(cwd, p string) string {
	if p == "" {
		return cwd
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(cwd, filepath.Clean(p))
}

// cortar aplica o teto com marcador no fim.
func cortar(s string, teto int) string {
	if len(s) <= teto {
		return s
	}
	return s[:teto] + fmt.Sprintf("\n\n[saída cortada em %d bytes; leia com start/limit menores para o resto]", teto)
}

// ── terminal_run ─────────────────────────────────────────────────────────

func specTerminalRun() Spec {
	return Spec{
		Name: "terminal_run",
		Description: "Executa um comando no terminal da máquina local (modo YOLO, sem pedido de aprovação). " +
			"Devolve stdout+stderr mesclados com exit_code. Timeout padrão 120 s (timeout_secs sobrescreve).",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command":      map[string]any{"type": "string", "description": "comando a executar"},
				"workdir":      map[string]any{"type": "string", "description": "diretório de execução (padrão: cwd do agente)"},
				"timeout_secs": map[string]any{"type": "integer", "description": "timeout em segundos; padrão 120"},
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
				return "", fmt.Errorf("entrada inválida: %w", err)
			}
			c.Command = strings.TrimSpace(c.Command)
			if c.Command == "" {
				return "", fmt.Errorf("command vazio")
			}
			dir := cwd
			if c.Workdir != "" {
				dir = caminho(cwd, c.Workdir)
			}
			timeout := timeoutPadrao
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
				res = fmt.Sprintf("exit_code: %d\ninício falhou: %v", code, exeErr)
			}
			return cortar(res, tetoSaida), nil
		},
	}
}

// ── file_read ────────────────────────────────────────────────────────────

func specFileRead() Spec {
	return Spec{
		Name:        "file_read",
		Description: "Lê um arquivo com números de linha. Em arquivos grandes, leia janelas: start (linha inicial, 1-based) + limit; o retorno diz o intervalo entregue e o total.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":  map[string]any{"type": "string"},
				"start": map[string]any{"type": "integer", "description": "linha inicial (1-based)"},
				"limit": map[string]any{"type": "integer", "description": "máximo de linhas nessa leitura"},
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
				return "", fmt.Errorf("entrada inválida: %w", err)
			}
			if c.Path == "" {
				return "", fmt.Errorf("path obrigatório")
			}
			b, err := os.ReadFile(caminho(cwd, c.Path))
			if err != nil {
				return "", err
			}
			linhas := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
			total := len(linhas)
			ini := 0
			if c.Start > 1 {
				ini = c.Start - 1
			}
			fim := total
			if c.Limit > 0 && ini+c.Limit < fim {
				fim = ini + c.Limit
			}
			if ini >= total {
				return fmt.Sprintf("arquivo tem %d linhas (start=%d)", total, c.Start), nil
			}
			var sb strings.Builder
			fmt.Fprintf(&sb, "linhas %d–%d de %d\n", ini+1, fim, total)
			for i := ini; i < fim; i++ {
				linha := linhas[i]
				if len(linha) > 512 {
					linha = linha[:512] + "…"
				}
				fmt.Fprintf(&sb, "%d: %s\n", i+1, linha)
			}
			return cortar(sb.String(), tetoSaida), nil
		},
	}
}

// ── file_write / file_append ─────────────────────────────────────────────

func specFileWrite() Spec {
	return Spec{
		Name:        "file_write",
		Description: "Escreve (sobrescreve) um arquivo. Cria os diretórios pais que faltarem.",
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
				return "", fmt.Errorf("entrada inválida: %w", err)
			}
			if c.Path == "" {
				return "", fmt.Errorf("path obrigatório")
			}
			p := caminho(cwd, c.Path)
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
		Description: "Anexa conteúdo ao fim de um arquivo (cria se não existir).",
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
				return "", fmt.Errorf("entrada inválida: %w", err)
			}
			if c.Path == "" {
				return "", fmt.Errorf("path obrigatório")
			}
			p := caminho(cwd, c.Path)
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
			return fmt.Sprintf("ok: anexado em %s", c.Path), nil
		},
	}
}

// ── file_copy ────────────────────────────────────────────────────────────

func specFileCopy() Spec {
	return Spec{
		Name:        "file_copy",
		Description: "Copia um arquivo para outro caminho (sobrescreve o destino). Usa os privilégios do sistema (cp/copy) — em YOLO não há sandbox.",
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
				return "", fmt.Errorf("entrada inválida: %w", err)
			}
			if c.From == "" || c.To == "" {
				return "", fmt.Errorf("from e to obrigatórios")
			}
			var cmd *exec.Cmd
			if runtime.GOOS == "windows" {
				cmd = exec.Command("cmd", "/C", "copy", "/Y", caminho(cwd, c.From), caminho(cwd, c.To))
			} else {
				cmd = exec.Command("cp", caminho(cwd, c.From), caminho(cwd, c.To))
			}
			cmd.Dir = cwd
			out, err := cmd.CombinedOutput()
			if err != nil {
				return fmt.Sprintf("falhou: %v\n%s", err, string(out)), nil
			}
			return fmt.Sprintf("ok: %s → %s", c.From, c.To), nil
		},
	}
}

// ── list_dir ─────────────────────────────────────────────────────────────

func specListDir() Spec {
	return Spec{
		Name:        "list_dir",
		Description: "Lista um diretório (padrão: cwd), recursivo até depth (padrão 1, max 3). Diretório começa com '/'; arquivo mostra o tamanho.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":  map[string]any{"type": "string"},
				"depth": map[string]any{"type": "integer", "description": "profundidade; padrão 1, máximo 3"},
			},
		},
		Exec: func(_ context.Context, cwd string, raw json.RawMessage) (string, error) {
			var c struct {
				Path  string `json:"path"`
				Depth int    `json:"depth"`
			}
			_ = json.Unmarshal(raw, &c)
			raiz := cwd
			if c.Path != "" {
				raiz = caminho(cwd, c.Path)
			}
			if c.Depth <= 0 {
				c.Depth = 1
			}
			if c.Depth > 3 {
				c.Depth = 3
			}
			var sb strings.Builder
			var andante func(p string, lvl int)
			andante = func(p string, lvl int) {
				ents, err := os.ReadDir(p)
				if err != nil {
					fmt.Fprintf(&sb, "(erro lendo %s: %v)\n", p, err)
					return
				}
				for _, e := range ents {
					nome := e.Name()
					if strings.HasPrefix(nome, ".") {
						continue
					}
					tudo := filepath.Join(p, nome)
					if e.IsDir() {
						fmt.Fprintf(&sb, "%s/\n", tudo)
						if lvl < c.Depth {
							andante(tudo, lvl+1)
						}
						continue
					}
					info, e2 := e.Info()
					if e2 != nil {
						fmt.Fprintf(&sb, "%s (erro: %v)\n", tudo, e2)
						continue
					}
					fmt.Fprintf(&sb, "%s (%d bytes)\n", tudo, info.Size())
				}
			}
			andante(raiz, 1)
			return cortar(sb.String(), tetoSaida), nil
		},
	}
}

// ── ctx_read (arquivo vivo de contexto do próprio agente) ────────────────

func specCtxRead() Spec {
	return Spec{
		Name:        "ctx_read",
		Description: "Lê o arquivo vivo de contexto da sessão (o que o agente está fazendo, decisões e pendências). No slice 0, o contexto é o BARROSO.md + o próprio transcript; esta devolve um contador do estado.",
		Schema:      map[string]any{"type": "object", "properties": map[string]any{}},
		Exec: func(_ context.Context, cwd string, _ json.RawMessage) (string, error) {
			baroo, _ := contextoVivo(cwd)
			return cortar(strings.TrimSpace("contexto vivo atual:\n"+baroo), tetoSaida), nil
		},
	}
}

// contextoVivo devolve um template informativo; versão editável (ctx_write/
// ctx_append) entra com sessão persistente no slice 1.
func contextoVivo(cwd string) (string, error) {
	b, err := os.ReadFile(filepath.Join(cwd, "BARROSO.md"))
	if err != nil {
		return "(sem BARROSO.md no projeto)", nil
	}
	return string(b), nil
}
