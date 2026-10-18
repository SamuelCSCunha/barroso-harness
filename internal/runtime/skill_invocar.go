package runtime

import (
	"strings"

	"github.com/ruanbarroso/barroso-harness/internal/skills"
)

// SkillsDisponiveis devolve as skills que um turno neste cwd enxergaria — as
// mesmas fontes e a mesma precedência de skills_list.
func SkillsDisponiveis(cwd string) []skills.Skill {
	hab, err := skills.Descobrir(fontesDeSkills(cwd))
	if err != nil {
		return nil
	}
	return hab
}

// ExpandirSkill transforma "/<nome> [argumentos]" no pedido de um turno que
// executa a skill. Até aqui skill era só LIDA pelo modelo quando ele resolvia
// chamar skill_read; invocar pelo nome é o usuário dizendo "faça isto agora,
// seguindo esta skill" — o corpo vai no pedido, sem depender do modelo achar.
//
// ok=false quando a linha não é /<nome> ou o nome não é de skill; o chamador
// segue o caminho normal (comando desconhecido).
func ExpandirSkill(cwd, linha string) (pedido string, nome string, ok bool) {
	linha = strings.TrimSpace(linha)
	if !strings.HasPrefix(linha, "/") {
		return "", "", false
	}
	cab, args, _ := strings.Cut(linha[1:], " ")
	cab = strings.TrimSpace(cab)
	if cab == "" {
		return "", "", false
	}
	for _, s := range SkillsDisponiveis(cwd) {
		if !strings.EqualFold(s.Nome, cab) {
			continue
		}
		var sb strings.Builder
		sb.WriteString("Execute agora a skill `" + s.Nome + "`, seguindo as instruções abaixo à risca.\n\n")
		sb.WriteString("<skill nome=\"" + s.Nome + "\" origem=\"" + s.Fontes + "\">\n")
		sb.WriteString(strings.TrimSpace(s.Corpo))
		sb.WriteString("\n</skill>\n")
		if a := strings.TrimSpace(args); a != "" {
			sb.WriteString("\nArgumentos do usuário: " + a + "\n")
		}
		return sb.String(), s.Nome, true
	}
	return "", "", false
}
