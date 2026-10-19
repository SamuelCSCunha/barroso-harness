// /contexto: mostra a política de contexto ativa e agrega, do sessions.db, as
// compactações já feitas por política — o instrumento para comparar "agente"
// e "limiar" com as sessões reais, sob o mesmo harness.
package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/store"
)

// agregadoPolitica soma as compactações de uma política.
type agregadoPolitica struct {
	Sessoes     map[string]bool
	Agente      int // ctx_compact chamado pelo modelo
	Proativa    int // guardrail por previsão de estouro
	Reativa     int // guardrail por recusa do provider
	BytesAntes  int
	BytesDepois int
}

// agregarContexto lê os eventos de compactação das últimas sessões. Eventos
// de guardrail anteriores a este recurso não têm "politica": contam como
// "limiar", que era o único comportamento existente.
func agregarContexto(loja *store.Store, maxSessoes int) map[string]*agregadoPolitica {
	out := map[string]*agregadoPolitica{}
	pegar := func(p string) *agregadoPolitica {
		if out[p] == nil {
			out[p] = &agregadoPolitica{Sessoes: map[string]bool{}}
		}
		return out[p]
	}
	ss, err := loja.ListSessions(maxSessoes)
	if err != nil {
		return out
	}
	for _, s := range ss {
		evs, err := loja.ListEvents(s.ID, 0, 0)
		if err != nil {
			continue
		}
		for _, e := range evs {
			if e.Kind != "compactacao" && e.Kind != "guardrail" {
				continue
			}
			var p struct {
				Politica    string `json:"politica"`
				Tipo        string `json:"tipo"`
				BytesAntes  int    `json:"bytes_antes"`
				BytesDepois int    `json:"bytes_depois"`
			}
			if json.Unmarshal([]byte(e.Payload), &p) != nil {
				continue
			}
			pol := strings.TrimSpace(p.Politica)
			if pol == "" {
				pol = "limiar"
			}
			a := pegar(pol)
			a.Sessoes[s.ID] = true
			switch {
			case e.Kind == "compactacao":
				a.Agente++
				a.BytesAntes += p.BytesAntes
				a.BytesDepois += p.BytesDepois
			case p.Tipo == "reativa":
				a.Reativa++
			default:
				a.Proativa++
			}
		}
	}
	return out
}

func cmdContexto(out io.Writer, cfg *config.Config, loja *store.Store) {
	pol := strings.TrimSpace(cfg.PoliticaContexto)
	if pol == "" {
		pol = "agente"
	}
	fmt.Fprintln(out, cianof("active policy: ")+pol+dimf(` — change it with "context_policy": "agente" | "limiar" in config.json`))
	if loja == nil {
		avisoFmt(out, "session not persisted: no log to aggregate")
		return
	}
	ag := agregarContexto(loja, 200)
	if len(ag) == 0 {
		fmt.Fprintln(out, dimf("no compactions recorded yet in the last 200 sessions"))
		return
	}
	for _, nome := range []string{"agente", "limiar"} {
		a := ag[nome]
		if a == nil {
			continue
		}
		linha := fmt.Sprintf("%-7s %3d sessions · %d ctx_compact · %d proactive guardrail · %d reactive",
			nome, len(a.Sessoes), a.Agente, a.Proativa, a.Reativa)
		if a.BytesAntes > 0 {
			linha += fmt.Sprintf(" · history %d→%d KiB (%.0f%% smaller)",
				a.BytesAntes>>10, a.BytesDepois>>10, 100*(1-float64(a.BytesDepois)/float64(a.BytesAntes)))
		}
		fmt.Fprintln(out, linha)
	}
	fmt.Fprintln(out, dimf("reactive = the provider rejected the request size; a sign the policy acted too late"))
}
