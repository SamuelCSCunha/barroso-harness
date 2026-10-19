// Package instructions resolves the BARROSO.md that goes in as the system
// instruction. Explicit, documented precedence: the working directory's
// BARROSO.md, then the parent directories up to the root, then the global one
// in ~/.barroso-harness/BARROSO.md. The same text never goes in twice.
package instructions

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/ruanbarroso/barroso-harness/internal/config"
)

// Resolve returns the applicable BARROSO.md file(s) concatenated in
// precedence order (most specific first). Empty is valid: a session with no
// extra instruction works.
func Resolve(cwd string) (string, error) {
	var parts []string
	seen := map[string]bool{}

	dir := cwd
	for {
		p := filepath.Join(dir, "BARROSO.md")
		if b, err := os.ReadFile(p); err == nil {
			if t := strings.TrimSpace(string(b)); t != "" && !seen[t] {
				seen[t] = true
				parts = append(parts, "# BARROSO.md — "+dir+"\n\n"+t)
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	// Same directory as config.json (config.Dir honors BARROSO_HARNESS_HOME):
	// otherwise the global BARROSO.md and the config would come from
	// different places.
	g := filepath.Join(config.Dir(), "BARROSO.md")
	if b, err := os.ReadFile(g); err == nil {
		if t := strings.TrimSpace(string(b)); t != "" && !seen[t] {
			seen[t] = true
			parts = append(parts, "# BARROSO.md — global\n\n"+t)
		}
	}

	if len(parts) == 0 {
		return "", nil
	}
	return strings.Join(parts, "\n\n---\n\n") + "\n", nil
}
