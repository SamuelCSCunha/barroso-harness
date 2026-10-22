// Extended turn catalog: everything that is NOT a built-in and NOT part of the
// session subsystem (session_search/session_retrieve/session_list, which live in
// tools_session.go because they close over the store).
//
// The split of responsibility is:
//
//   - runtime builds ON ITS OWN whatever is a pure function of configuration
//     and cwd — live context (live_context.go), web (internal/web) and skills
//     (internal/skills). None of them depends on the process lifecycle, so
//     they can be rebuilt every turn at no real cost.
//   - the CALLER injects what has an external owner — subagents
//     (internal/subagent) and MCP servers (internal/mcp). Both need process
//     resources (worker goroutines, stdio subprocess, HTTP connection) that
//     cannot be born and die within a turn. That is why they are not imported
//     here: subagent imports this package, and importing back would be a
//     cycle. See Injetar.
//
// A tool name is unique in the catalog. Two context_read in a schema is an
// ambiguous call for the provider, and an external tool called shell
// would hijack a built-in. dedupe and noShadow handle the two cases with
// different policies, on purpose.
package runtime

import (
	"path/filepath"
	"strings"

	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/instructions"
	"github.com/ruanbarroso/barroso-harness/internal/skills"
	"github.com/ruanbarroso/barroso-harness/internal/tools"
	"github.com/ruanbarroso/barroso-harness/internal/web"
)

// Retirar removes from the catalog the injected specs with these names — the
// mcp_remove path: the server goes away and its tools disappear from the next
// turn on.
func (s *Session) Retirar(names ...string) {
	if len(names) == 0 {
		return
	}
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[strings.TrimSpace(n)] = true
	}
	s.muExtras.Lock()
	defer s.muExtras.Unlock()
	kept := s.injected[:0]
	for _, sp := range s.injected {
		if !out[strings.TrimSpace(sp.Name)] {
			kept = append(kept, sp)
		}
	}
	s.injected = kept
}

// Injetar adds specs from external subsystems to the catalog of every turn of
// this session. Called by the wiring (CLI, ACP, main) after creating the
// session, with what the owning package returns:
//
//	sess := runtime.NewSession(cfg)
//	sess.Injetar(subagent.Especificacoes(gerente, cfg)...)
//	sess.Injetar(mcp.Especificacoes...)
//
// Injected specs NEVER shadow built-ins, live context, web or skills: see
// noShadow. Injecting after a turn has run applies to the following ones —
// the list is built on each Send, not cached in the session.
//
// Safe to call concurrently; it is not the hot path (the wiring calls it once),
// and the lock covers the case of a /model swapping the config midway.
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
		s.injected = append(s.injected, sp)
	}
}

// turnExtras returns the extended catalog: runtime-owned first, injected
// after, concatenated without name collisions.
func (s *Session) turnExtras(cwd string) []tools.Spec {
	s.muExtras.Lock()
	injected := append([]tools.Spec(nil), s.injected...)
	s.muExtras.Unlock()

	own := dedupe(append(
		EspecificacoesContextoVivoSessao(s),
		append(web.Especificacoes(s.search()), skillSpecs(cwd)...)...,
	))
	return noShadow(own, injected)
}

// search translates the configuration into the web package handle: the
// optional SearXNG (search_url) plus the anonymous hosted providers named by
// search_providers (absent = web.DefaultProviders). Providers are
// process-wide singletons, so their discovery cache survives across turns.
func (s *Session) search() *web.Busca {
	names := s.config.SearchProviders
	if names == nil {
		names = web.DefaultProviders
	}
	return &web.Busca{
		Endpoint:  strings.TrimSpace(s.config.SearchURL),
		Providers: web.Provedores(names),
	}
}

// skillSpecs discovers the skills on disk and returns their tools. Discovery
// only errors when ALL sources exist and none could be read; the right answer
// then is an empty catalog (skills explains there are no skills), not an
// aborted turn — a skill is a convenience, not a prerequisite.
//
// Discovery happens every turn, on purpose: reading three directories and the
// SKILL.md files costs microseconds, and caching would mean a skill created
// now only shows up in the next session — the agent being told about a skill
// it cannot yet see is worse than the cost.
func skillSpecs(cwd string) []tools.Spec {
	found, err := skills.Descobrir(skillSources(cwd))
	if err != nil {
		found = nil
	}
	// The scope labels ("projeto"/"global") are defined by package skills.
	management := skills.EspecificacoesGestao(map[string]string{
		"projeto": filepath.Join(cwd, ".barroso", "skills"),
		"global":  filepath.Join(config.Dir(), "skills"),
	})
	return append(skills.Especificacoes(found), management...)
}

// skillSources composes the skill roots in precedence order:
// cwd/.barroso/skills, cwd/skills (project) and <harness data>/skills
// (global).
//
// The global root comes from config.Dir(), not from the user's home: when
// BARROSO_HARNESS_HOME points elsewhere, the harness data — including the
// global skills — went with it. skills.FontesPadrao assumes the default home
// and would get exactly the case the environment variable exists for wrong.
func skillSources(cwd string) []skills.Fonte {
	var sources []skills.Fonte
	if c := strings.TrimSpace(cwd); c != "" {
		sources = append(sources,
			skills.Fonte{Raiz: filepath.Join(c, ".barroso", "skills"), Rotulo: "projeto"},
			skills.Fonte{Raiz: filepath.Join(c, "skills"), Rotulo: "projeto"},
		)
	}
	sources = append(sources, skills.Fonte{
		Raiz:   filepath.Join(config.Dir(), "skills"),
		Rotulo: "global",
	})
	return sources
}

// ── deduplication ─────────────────────────────────────────────────────────

// dedupe collapses repeated names keeping the LAST definition, at the
// POSITION of the first. It is the policy for catalogs the runtime controls:
// the placeholder context_read from tools/specs.go is replaced by the one from
// live_context.go (which has a real body), and the stable position keeps the
// schema order predictable across turns.
//
// A spec without a name is dropped: an anonymous tool can never be called by
// the provider, and keeping it in the list would only bloat the request.
func dedupe(specs []tools.Spec) []tools.Spec {
	index := make(map[string]int, len(specs))
	out := make([]tools.Spec, 0, len(specs))
	for _, sp := range specs {
		name := strings.TrimSpace(sp.Name)
		if name == "" {
			continue
		}
		if i, ok := index[name]; ok {
			out[i] = sp
			continue
		}
		index[name] = len(out)
		out = append(out, sp)
	}
	return out
}

// noShadow appends the external specs ONLY with names the base does not have
// yet. The opposite policy of dedupe, deliberately:
//
// An MCP server publishing a tool called shell must not take the place
// of the built-in — the name would become a Trojan horse looking like a local
// privilege. The same goes for a subagent. Coming from outside, the name loses
// to the inside one; the legitimate owner stays in the catalog and the
// external tool is simply not exposed (the MCP connection notice and the
// skills catalog remain the place to see what was published).
func noShadow(base, external []tools.Spec) []tools.Spec {
	if len(external) == 0 {
		return base
	}
	taken := make(map[string]bool, len(base))
	for _, sp := range base {
		taken[strings.TrimSpace(sp.Name)] = true
	}
	out := append([]tools.Spec(nil), base...)
	for _, sp := range external {
		name := strings.TrimSpace(sp.Name)
		if name == "" || taken[name] {
			continue
		}
		taken[name] = true
		out = append(out, sp)
	}
	return out
}

// toolCatalog is the SINGLE RECIPE of a turn's tool catalog, used by the three
// entry points (REPL, -p and ACP — ACP arrives through the same Session.Send).
// The order matters for dedup and for the provider:
//
//  1. built-ins (terminal, files, list) — the base;
//  2. log retrieval (session_search/session_retrieve/session_list) — closes over the
//     session, so it enters here and not in package tools;
//  3. live context (context_read/context_write/context_append) — replaces the context_read
//     placeholder that comes with the built-ins;
//  4. web + skills — a function of config and cwd, rebuilt every turn;
//  5. injected (subagents, MCP) — only with names the base does not have.
//
// dedupe resolves the duplicated context_read (placeholder vs live) keeping the
// LAST definition; noShadow prevents an external tool from hijacking a local
// name.
func (s *Session) toolCatalog(cwd string) []tools.Spec {
	base := append(tools.SpecsBuiltins(), SessaoSpecs(s)...)
	base = append(base, EspecificacoesContextoVivoSessao(s)...)
	base = append(base, web.Especificacoes(s.search())...)
	base = append(base, skillSpecs(cwd)...)
	base = append(base, instructions.Especificacoes()...)
	if policy(s.config.PoliticaContexto) == PoliticaAgente {
		base = append(base, compactSpec())
	}
	s.muExtras.Lock()
	injected := append([]tools.Spec(nil), s.injected...)
	s.muExtras.Unlock()
	return noShadow(dedupe(base), injected)
}

// Catalogo exposes the turn recipe so wiring and tests can check what the
// model will see.
func (s *Session) Catalogo(cwd string) []tools.Spec { return s.toolCatalog(cwd) }
