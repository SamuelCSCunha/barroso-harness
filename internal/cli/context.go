// /context: shows the active context policy and aggregates, from sessions.db,
// the compactions done per policy — the instrument to compare "agente" and
// "limiar" against real sessions, under the same harness.
package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/store"
)

// policyAggregate sums the compactions of one policy.
type policyAggregate struct {
	Sessoes     map[string]bool
	Agente      int // context_compact called by the model
	Proativa    int // guardrail on predicted overflow
	Reativa     int // guardrail on provider rejection
	BytesAntes  int
	BytesDepois int
}

// aggregateContext reads the compaction events of the latest sessions.
// Guardrail events older than this feature have no "politica": they count as
// "limiar", which was the only existing behaviour.
func aggregateContext(db *store.Store, maxSessions int) map[string]*policyAggregate {
	out := map[string]*policyAggregate{}
	get := func(p string) *policyAggregate {
		if out[p] == nil {
			out[p] = &policyAggregate{Sessoes: map[string]bool{}}
		}
		return out[p]
	}
	ss, err := db.ListSessions(maxSessions)
	if err != nil {
		return out
	}
	for _, s := range ss {
		evs, err := db.ListEvents(s.ID, 0, 0)
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
			a := get(pol)
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

func cmdContext(out io.Writer, cfg *config.Config, db *store.Store) {
	pol := strings.TrimSpace(cfg.PoliticaContexto)
	if pol == "" {
		pol = "agente"
	}
	fmt.Fprintln(out, cyanf("active policy: ")+pol+dimf(` — change it with "context_policy": "agente" | "limiar" in config.json`))
	if db == nil {
		warnFmt(out, "session not persisted: no log to aggregate")
		return
	}
	ag := aggregateContext(db, 200)
	if len(ag) == 0 {
		fmt.Fprintln(out, dimf("no compactions recorded yet in the last 200 sessions"))
		return
	}
	for _, name := range []string{"agente", "limiar"} {
		a := ag[name]
		if a == nil {
			continue
		}
		line := fmt.Sprintf("%-7s %3d sessions · %d context_compact · %d proactive guardrail · %d reactive",
			name, len(a.Sessoes), a.Agente, a.Proativa, a.Reativa)
		if a.BytesAntes > 0 {
			line += fmt.Sprintf(" · history %d→%d KiB (%.0f%% smaller)",
				a.BytesAntes>>10, a.BytesDepois>>10, 100*(1-float64(a.BytesDepois)/float64(a.BytesAntes)))
		}
		fmt.Fprintln(out, line)
	}
	fmt.Fprintln(out, dimf("reactive = the provider rejected the request size; a sign the policy acted too late"))
}
