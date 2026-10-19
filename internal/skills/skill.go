// Package skills resolves the agent's skills from disk. A skill is a directory
// containing a SKILL.md (preferred form; the directory name names the skill)
// or a loose SKILL.md in a source root (then the name comes from the
// frontmatter). The frontmatter is the first block delimited by `---` lines;
// only simple `key: value` lines are read — nested YAML, lists and blocks are
// not interpreted.
package skills

import (
	"os"
	"path/filepath"
	"strings"
)

// fileName is the fixed name of the file that materializes a skill.
const fileName = "SKILL.md"

// Skill is a skill already read from disk, ready to enter the catalog.
type Skill struct {
	Nome      string // frontmatter `name`; without it, the directory/file name
	Descricao string // frontmatter `description`
	Caminho   string // absolute path of the SKILL.md
	Corpo     string // the markdown AFTER the frontmatter, verbatim
	Fontes    string // label of the source it came from ("global"|"projeto")
}

// parse splits frontmatter from body and returns the skill. ok=false when the
// body is empty — a skill without content does not enter the catalog.
func parse(content, path, source string) (Skill, bool) {
	name, description, body := splitFrontmatter(content)
	if strings.TrimSpace(name) == "" {
		name = nameFromDisk(path)
	}
	s := Skill{
		Nome:      name,
		Descricao: description,
		Caminho:   path,
		Corpo:     body,
		Fontes:    source,
	}
	if strings.TrimSpace(body) == "" {
		return s, false
	}
	return s, true
}

// splitFrontmatter returns name, description and the body. Without frontmatter
// (or with an unclosed block) the whole file is the body and the name comes
// from disk. The body starts on the first line after the closing `---`: the
// newline ending the block is consumed, the rest is preserved verbatim.
func splitFrontmatter(content string) (name, description, body string) {
	lines := strings.Split(content, "\n")

	open := -1
	for i, l := range lines {
		t := trimCR(l)
		if t == "" {
			continue
		}
		if t == "---" {
			open = i
		}
		break
	}
	if open < 0 {
		return "", "", content
	}

	closing := -1
	for i := open + 1; i < len(lines); i++ {
		if trimCR(lines[i]) == "---" {
			closing = i
			break
		}
	}
	if closing < 0 {
		return "", "", content
	}

	for i := open + 1; i < closing; i++ {
		l := strings.TrimSpace(trimCR(lines[i]))
		key, value, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "name":
			name = unquote(strings.TrimSpace(value))
		case "description":
			description = unquote(strings.TrimSpace(value))
		}
	}

	if closing+1 < len(lines) {
		body = strings.Join(lines[closing+1:], "\n")
	}
	return name, description, body
}

// trimCR strips the \r left over from line breaks in CRLF files.
func trimCR(l string) string { return strings.TrimSuffix(l, "\r") }

// unquote removes quotes (double or single) wrapping the whole value.
func unquote(v string) string {
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			return v[1 : len(v)-1]
		}
	}
	return v
}

// nameFromDisk derives the name from the path: the parent directory when the
// file is the canonical SKILL.md (`<root>/<skill>/SKILL.md` → `<skill>`);
// otherwise, the file name without extension.
func nameFromDisk(path string) string {
	clean := filepath.Clean(path)
	base := filepath.Base(clean)
	if strings.EqualFold(base, fileName) {
		parent := filepath.Base(filepath.Dir(clean))
		if parent == "" || parent == "." || parent == string(filepath.Separator) {
			return ""
		}
		return parent
	}
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// findSkillMD looks for the SKILL.md inside dir (without descending further).
// The comparison ignores case, to cope with case-insensitive filesystems.
func findSkillMD(dir string) (string, bool) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	for _, e := range ents {
		if e.IsDir() || !strings.EqualFold(e.Name(), fileName) {
			continue
		}
		return filepath.Join(dir, e.Name()), true
	}
	return "", false
}
