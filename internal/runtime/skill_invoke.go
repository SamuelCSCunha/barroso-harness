package runtime

import (
	"strings"

	"github.com/ruanbarroso/barroso-harness/internal/skills"
)

// SkillsDisponiveis returns the skills a turn in this cwd would see — the same
// sources and the same precedence as skills.
func SkillsDisponiveis(cwd string) []skills.Skill {
	found, err := skills.Descobrir(skillSources(cwd))
	if err != nil {
		return nil
	}
	return found
}

// ExpandirSkill turns "/<name> [arguments]" into the prompt of a turn that
// runs the skill. Until now a skill was only READ by the model when it decided
// to call skill; invoking it by name is the user saying "do this now,
// following this skill" — the body goes in the prompt, without depending on
// the model finding it.
//
// ok=false when the line is not /<name> or the name is not a skill; the caller
// follows the normal path (unknown command).
func ExpandirSkill(cwd, line string) (prompt string, name string, ok bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "/") {
		return "", "", false
	}
	head, args, _ := strings.Cut(line[1:], " ")
	head = strings.TrimSpace(head)
	if head == "" {
		return "", "", false
	}
	for _, s := range SkillsDisponiveis(cwd) {
		if !strings.EqualFold(s.Nome, head) {
			continue
		}
		var sb strings.Builder
		sb.WriteString("Run the skill `" + s.Nome + "` now, following the instructions below to the letter.\n\n")
		sb.WriteString("<skill name=\"" + s.Nome + "\" source=\"" + s.Fontes + "\">\n")
		sb.WriteString(strings.TrimSpace(s.Corpo))
		sb.WriteString("\n</skill>\n")
		if a := strings.TrimSpace(args); a != "" {
			sb.WriteString("\nUser arguments: " + a + "\n")
		}
		return sb.String(), s.Nome, true
	}
	return "", "", false
}
