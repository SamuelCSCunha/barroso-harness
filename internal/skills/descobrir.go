package skills

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Fonte é uma raiz de skills no disco, com o rótulo que a identifica no
// catálogo ("projeto", "global").
type Fonte struct {
	Raiz   string
	Rotulo string
}

// errFonteAusente marca uma fonte que não existe como diretório legível. Conta
// como vazia — a máquina simplesmente não tem aquela raiz — e nunca como
// falha de leitura.
var errFonteAusente = errors.New("fonte ausente")

// Descobrir varre as fontes na ordem dada (índice 0 = maior precedência) e
// devolve o catálogo. De cada raiz entram os subdiretórios de um único nível
// que contenham SKILL.md, mais um SKILL.md solto na própria raiz.
//
// Dedupe por nome: a primeira fonte que define um nome ganha. Diretórios
// começados por "." são ignorados (assim como arquivos ocultos), e skills de
// corpo vazio não entram. A lista final é ordenada por nome, ignorando caixa.
//
// Fonte ausente ou vazia é normal e não altera o resultado. Erro só quando
// TODAS as fontes existem e nenhuma pôde ser lida.
func Descobrir(diretorios []Fonte) ([]Skill, error) {
	var (
		fora      []Skill
		vistos    = map[string]bool{}
		raizes    []string
		ilegiveis []string
	)

	for _, f := range diretorios {
		raiz := strings.TrimSpace(f.Raiz)
		if raiz == "" {
			continue
		}
		raizes = append(raizes, raiz)
		err := varrer(raiz, f.Rotulo, &fora, vistos)
		if err != nil && !errors.Is(err, errFonteAusente) {
			ilegiveis = append(ilegiveis, raiz)
			continue
		}
	}

	if len(raizes) > 0 && len(ilegiveis) == len(raizes) {
		return nil, &erroTodasFontes{raizes: ilegiveis}
	}

	sort.SliceStable(fora, func(i, j int) bool {
		return strings.ToLower(fora[i].Nome) < strings.ToLower(fora[j].Nome)
	})
	return fora, nil
}

// varrer lê uma raiz e acumula as skills encontradas em `fora`, respeitando o
// dedupe. Devolve errFonteAusente quando a raiz não é um diretório legível, e
// erro concreto quando é mas não pôde ser lida.
func varrer(raiz, rotulo string, fora *[]Skill, vistos map[string]bool) error {
	info, err := os.Stat(raiz)
	if err != nil {
		// não existe: fonte simplesmente não está configurada nesta máquina
		return fmt.Errorf("%w: %v", errFonteAusente, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("fonte de skills não é um diretório: %s", raiz)
	}

	ents, err := os.ReadDir(raiz)
	if err != nil {
		return err
	}
	for _, e := range ents {
		nome := e.Name()
		if strings.HasPrefix(nome, ".") {
			continue
		}
		if e.IsDir() {
			md, ok := acharSkillMD(filepath.Join(raiz, nome))
			if !ok {
				continue
			}
			s, ok, err := carregarDe(md, rotulo)
			if err != nil || !ok || vistos[s.Nome] {
				continue
			}
			vistos[s.Nome] = true
			*fora = append(*fora, s)
			continue
		}
		// SKILL.md solto na raiz da fonte.
		if !strings.EqualFold(nome, nomeArquivo) {
			continue
		}
		s, ok, err := carregarDe(filepath.Join(raiz, nome), rotulo)
		if err != nil || !ok || vistos[s.Nome] {
			continue
		}
		vistos[s.Nome] = true
		*fora = append(*fora, s)
	}
	return nil
}

// erroTodasFontes é o erro único: nenhuma fonte pôde ser lida.
type erroTodasFontes struct{ raizes []string }

func (e *erroTodasFontes) Error() string {
	return "nenhuma fonte de skills pôde ser lida: " + strings.Join(e.raizes, ", ")
}

// FontesPadrao compõe as fontes na ordem de precedência do harness:
// cwd/.barroso/skills, cwd/skills (ambas "projeto") e
// home/.barroso-harness/skills ("global"). Strings vazias são ignoradas —
// home desconhecido só derruba a fonte global.
func FontesPadrao(home string, cwd string) []Fonte {
	var fontes []Fonte
	if c := strings.TrimSpace(cwd); c != "" {
		fontes = append(fontes,
			Fonte{Raiz: filepath.Join(c, ".barroso", "skills"), Rotulo: "projeto"},
			Fonte{Raiz: filepath.Join(c, "skills"), Rotulo: "projeto"},
		)
	}
	if h := strings.TrimSpace(home); h != "" {
		fontes = append(fontes,
			Fonte{Raiz: filepath.Join(h, ".barroso-harness", "skills"), Rotulo: "global"},
		)
	}
	return fontes
}
