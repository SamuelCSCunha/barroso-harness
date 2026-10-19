package skills

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Fonte is a skills root on disk, with the label that identifies it in the
// catalog ("projeto", "global").
type Fonte struct {
	Raiz   string
	Rotulo string
}

// errSourceMissing marks a source that does not exist as a readable directory.
// It counts as empty — the machine simply lacks that root — and never as a
// read failure.
var errSourceMissing = errors.New("source missing")

// Descobrir scans the sources in the given order (index 0 = highest precedence)
// and returns the catalog. From each root it takes the first-level
// subdirectories that contain a SKILL.md, plus a loose SKILL.md in the root
// itself.
//
// Dedupe by name: the first source that defines a name wins. Directories
// starting with "." are ignored (as are hidden files), and skills with an empty
// body are skipped. The final list is sorted by name, case-insensitively.
//
// A missing or empty source is normal and does not change the result. It only
// errors when ALL sources exist and none could be read.
func Descobrir(dirs []Fonte) ([]Skill, error) {
	var (
		out        []Skill
		seen       = map[string]bool{}
		roots      []string
		unreadable []string
	)

	for _, f := range dirs {
		root := strings.TrimSpace(f.Raiz)
		if root == "" {
			continue
		}
		roots = append(roots, root)
		err := scan(root, f.Rotulo, &out, seen)
		if err != nil && !errors.Is(err, errSourceMissing) {
			unreadable = append(unreadable, root)
			continue
		}
	}

	if len(roots) > 0 && len(unreadable) == len(roots) {
		return nil, &allSourcesError{roots: unreadable}
	}

	sort.SliceStable(out, func(i, j int) bool {
		return strings.ToLower(out[i].Nome) < strings.ToLower(out[j].Nome)
	})
	return out, nil
}

// scan reads one root and appends the skills found to `out`, honoring the
// dedupe. It returns errSourceMissing when the root is not a readable
// directory, and a concrete error when it is one but could not be read.
func scan(root, label string, out *[]Skill, seen map[string]bool) error {
	info, err := os.Stat(root)
	if err != nil {
		// does not exist: the source is simply not configured on this machine
		return fmt.Errorf("%w: %v", errSourceMissing, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("skills source is not a directory: %s", root)
	}

	ents, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, e := range ents {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if e.IsDir() {
			md, ok := findSkillMD(filepath.Join(root, name))
			if !ok {
				continue
			}
			s, ok, err := loadFrom(md, label)
			if err != nil || !ok || seen[s.Nome] {
				continue
			}
			seen[s.Nome] = true
			*out = append(*out, s)
			continue
		}
		// Loose SKILL.md in the source root.
		if !strings.EqualFold(name, fileName) {
			continue
		}
		s, ok, err := loadFrom(filepath.Join(root, name), label)
		if err != nil || !ok || seen[s.Nome] {
			continue
		}
		seen[s.Nome] = true
		*out = append(*out, s)
	}
	return nil
}

// allSourcesError is the single error: no source could be read.
type allSourcesError struct{ roots []string }

func (e *allSourcesError) Error() string {
	return "no skills source could be read: " + strings.Join(e.roots, ", ")
}

// FontesPadrao builds the sources in the harness precedence order:
// cwd/.barroso/skills, cwd/skills (both "projeto") and
// home/.barroso-harness/skills ("global"). Empty strings are ignored — an
// unknown home only drops the global source.
func FontesPadrao(home string, cwd string) []Fonte {
	var sources []Fonte
	if c := strings.TrimSpace(cwd); c != "" {
		sources = append(sources,
			Fonte{Raiz: filepath.Join(c, ".barroso", "skills"), Rotulo: "projeto"},
			Fonte{Raiz: filepath.Join(c, "skills"), Rotulo: "projeto"},
		)
	}
	if h := strings.TrimSpace(home); h != "" {
		sources = append(sources,
			Fonte{Raiz: filepath.Join(h, ".barroso-harness", "skills"), Rotulo: "global"},
		)
	}
	return sources
}
