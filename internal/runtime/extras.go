// Catálogo estendido do turno: tudo que NÃO é built-in e NÃO é do subsistema
// de sessão (session_search/session_retrieve, que vivem em tools_session.go
// porque fecham sobre o store).
//
// A divisão de responsabilidade é esta:
//
//   - runtime monta SOZINHO o que é função pura da configuração e do cwd —
//     contexto vivo (ctx_vivo.go), web (internal/web) e skills
//     (internal/skills). Nenhum deles depende do ciclo de vida do processo,
//     então podem ser reconstruídos a cada turno sem custo de verdade.
//   - o CHAMADOR injeta o que tem dono externo — sub-agentes
//     (internal/subagent) e servidores MCP (internal/mcp). Os dois precisam
//     de recursos de processo (goroutines de worker, subprocesso stdio,
//     conexão HTTP) que não podem nascer e morrer dentro de um turno. Por
//     isso eles não são importados aqui: subagent importa este pacote, e
//     importar de volta seria ciclo. Ver Injetar.
//
// O nome de tool é único no catálogo. Dois ctx_read num schema é chamada
// ambígua para o provider, e uma ferramenta externa chamada terminal_run
// seria um sequestro de built-in. semRepetir e semSombra resolvem os dois
// casos com políticas diferentes, de propósito.
package runtime

import (
	"path/filepath"
	"strings"

	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/skills"
	"github.com/ruanbarroso/barroso-harness/internal/tools"
	"github.com/ruanbarroso/barroso-harness/internal/web"
)

// Injetar acrescenta specs de subsistemas externos ao catálogo de todos os
// turnos desta sessão. Chamado pelo wiring (CLI, ACP, main) depois de criar a
// sessão, com o que o pacote dono devolve:
//
//	sess := runtime.NewSession(cfg)
//	sess.Injetar(subagent.Especificacoes(gerente, cfg)...)
//	sess.Injetar(mcp.Especificacoes...)
//
// Specs injetadas NUNCA sombreiam built-in, contexto vivo, web ou skills: ver
// semSombra. Injetar depois de um turno já ter rodado vale para os próximos —
// a lista é monta a cada Send, não cacheada na sessão.
//
// Seguro para chamar concorrentemente; não é o caminho quente (o wiring chama
// uma vez), e o lock cobre o caso de um /model trocar a config no meio.
func (s *Session) Injetar(specs ...tools.Spec) {
	if len(specs) == 0 {
		return
	}
	s.muExtras.Lock()
	defer s.muExtras.Unlock()
	for _, sp := range specs {
		if strings.TrimSpace(sp.Name) == "" {
			continue
		}
		s.injetadas = append(s.injetadas, sp)
	}
}

// extrasDoTurno devolve o catálogo estendido: os do runtime primeiro, os
// injetados depois, concatenados sem colisão de nome.
func (s *Session) extrasDoTurno(cwd string) []tools.Spec {
	s.muExtras.Lock()
	injetadas := append([]tools.Spec(nil), s.injetadas...)
	s.muExtras.Unlock()

	proprios := semRepetir(append(
		EspecificacoesContextoVivoSessao(s),
		append(web.Especificacoes(s.busca()), specsDeSkills(cwd)...)...,
	))
	return semSombra(proprios, injetadas)
}

// busca traduz a configuração no handle do pacote web. nil quando não há
// search_url: a tool web_search entra no catálogo MESMO assim, porque o erro
// de configuração precisa aparecer na saída da tool — o modelo lê e sabe o
// que pedir — em vez de a tool silenciosamente não existir.
func (s *Session) busca() *web.Busca {
	url := strings.TrimSpace(s.config.SearchURL)
	if url == "" {
		return nil
	}
	return &web.Busca{Endpoint: url}
}

// specsDeSkills descobre as skills do disco e devolve as duas tools. Descobrir
// devolve erro só quando TODAS as fontes existem e nenhuma pôde ser lida; a
// resposta certa aí é catálogo vazio (skills_list explica que não há skills),
// não um turno abortado — skill é conveniência, não pré-requisito.
//
// A descoberta acontece a cada turno, de propósito: ler três diretórios e os
// SKILL.md custa microssegundos, e cachear significaria que uma skill criada
// agora só apareceria na próxima sessão — o agente sendo avisado de uma
// habilidade que ainda não enxerga é pior que o custo.
func specsDeSkills(cwd string) []tools.Spec {
	hab, err := skills.Descobrir(fontesDeSkills(cwd))
	if err != nil {
		return skills.Especificacoes(nil)
	}
	return skills.Especificacoes(hab)
}

// fontesDeSkills compõe as raízes de skills na ordem de precedência:
// cwd/.barroso/skills, cwd/skills (projeto) e <dados da harness>/skills
// (global).
//
// A raiz global vem de config.Dir(), e não do home do usuário: quando
// BARROSO_HARNESS_HOME aponta para outro lugar, os dados da harness — inclusive
// as skills globais — foram com ele. skills.FontesPadrao assume o home padrão
// e erraria justamente no caso que a variável de ambiente existe para servir.
func fontesDeSkills(cwd string) []skills.Fonte {
	var fontes []skills.Fonte
	if c := strings.TrimSpace(cwd); c != "" {
		fontes = append(fontes,
			skills.Fonte{Raiz: filepath.Join(c, ".barroso", "skills"), Rotulo: "projeto"},
			skills.Fonte{Raiz: filepath.Join(c, "skills"), Rotulo: "projeto"},
		)
	}
	fontes = append(fontes, skills.Fonte{
		Raiz:   filepath.Join(config.Dir(), "skills"),
		Rotulo: "global",
	})
	return fontes
}

// ── deduplicação ──────────────────────────────────────────────────────────

// semRepetir colapsa nomes repetidos ficando com a ÚLTIMA definição, na
// POSIÇÃO da primeira. É a política dos catálogos que o runtime controla: o
// ctx_read placeholder de tools/specs.go é substituído pelo de ctx_vivo.go
// (que tem corpo de verdade), e a posição estável mantém a ordem do schema
// previsível entre turnos.
//
// Spec sem nome é descartada: tool anônima não pode ser chamada pelo provider
// de jeito nenhum, e deixá-la na lista só inflaria o request.
func semRepetir(specs []tools.Spec) []tools.Spec {
	indice := make(map[string]int, len(specs))
	out := make([]tools.Spec, 0, len(specs))
	for _, sp := range specs {
		nome := strings.TrimSpace(sp.Name)
		if nome == "" {
			continue
		}
		if i, ok := indice[nome]; ok {
			out[i] = sp
			continue
		}
		indice[nome] = len(out)
		out = append(out, sp)
	}
	return out
}

// semSombra acrescenta as specs externas SÓ com nomes que a base ainda não
// tem. Política oposta à de semRepetir, e deliberadamente:
//
// Um servidor MCP que publique uma tool chamada terminal_run não pode tomar o
// lugar da built-in — o nome viraria um cavalo de Troia com a aparência de
// privilégio local. O mesmo vale para um subagente. Originado de fora, o nome
// perde para o de dentro; o dono legítimo fica no catálogo e a ferramenta
// externa simplesmente não é exposta (o aviso de conexão do MCP e o catálogo
// de skills continuam sendo o lugar de ver o que foi publicado).
func semSombra(base, externas []tools.Spec) []tools.Spec {
	if len(externas) == 0 {
		return base
	}
	tomados := make(map[string]bool, len(base))
	for _, sp := range base {
		tomados[strings.TrimSpace(sp.Name)] = true
	}
	out := append([]tools.Spec(nil), base...)
	for _, sp := range externas {
		nome := strings.TrimSpace(sp.Name)
		if nome == "" || tomados[nome] {
			continue
		}
		tomados[nome] = true
		out = append(out, sp)
	}
	return out
}
