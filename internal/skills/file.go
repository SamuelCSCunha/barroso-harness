package skills

import (
	"fmt"
	"os"
)

// Carregar reads the SKILL.md at `path` and returns the skill. It fails if the
// file does not exist or if the body (after the frontmatter) is empty.
func Carregar(path string) (*Skill, error) {
	s, ok, err := loadFrom(path, "")
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%s: SKILL.md has no body", path)
	}
	return &s, nil
}

// loadFrom reads and parses a SKILL.md; ok=false when the body is empty.
func loadFrom(path, source string) (Skill, bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Skill{}, false, err
	}
	s, ok := parse(string(b), path, source)
	return s, ok, nil
}
