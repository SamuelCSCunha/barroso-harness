// Package instructions resolve o BARROSO.md que entra como instrução de
// sistema. Precedência explícita e documentada: BARROSO.md do diretório de
// trabalho, depois dos diretórios pais até a raiz, depois o global em
// ~/.barroso-harness/BARROSO.md. O mesmo texto nunca entra duas vezes.
package instructions

import (
	"os"
	"path/filepath"
	"strings"
)

// Resolve devolve o(s) BARROSO.md aplicáveis concatenados em ordem de
// precedência (mais específico primeiro). Vazio é válido: sessão sem
// instrução extra funciona.
func Resolve(cwd string) (string, error) {
	var partes []string
	ja := map[string]bool{}

	dir := cwd
	for {
		p := filepath.Join(dir, "BARROSO.md")
		if b, err := os.ReadFile(p); err == nil {
			if t := strings.TrimSpace(string(b)); t != "" && !ja[t] {
				ja[t] = true
				partes = append(partes, "# BARROSO.md — "+dir+"\n\n"+t)
			}
		}
		pai := filepath.Dir(dir)
		if pai == dir {
			break
		}
		dir = pai
	}

	if home, err := os.UserHomeDir(); err == nil {
		g := filepath.Join(home, ".barroso-harness", "BARROSO.md")
		if b, err := os.ReadFile(g); err == nil {
			if t := strings.TrimSpace(string(b)); t != "" && !ja[t] {
				ja[t] = true
				partes = append(partes, "# BARROSO.md — global\n\n"+t)
			}
		}
	}

	if len(partes) == 0 {
		return "", nil
	}
	return strings.Join(partes, "\n\n---\n\n") + "\n", nil
}
