package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"

	"github.com/ruanbarroso/barroso-harness/internal/instructions"
	"github.com/ruanbarroso/barroso-harness/internal/skills"
)

// Live updates: skills, BARROSO.md and MCP servers can change while a session
// is open — by this session's tools, by another bh process or by hand. The
// system prompt stays frozen (the provider's prompt cache matches it byte for
// byte), so each change reaches the model as a "[system update]" message
// appended to the history at the next request boundary. The history stays
// append-only and every open session picks the change up on its next round.

// UpdatePrefix marks harness-generated update messages (the screen draws them
// as events, not as user speech).
const UpdatePrefix = "[system update]"

// resourceState is what the session last told the model about.
type resourceState struct {
	baroMD string
	skills map[string]string // name → description + body hash
}

func hashText(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:8])
}

func currentResources(cwd string) resourceState {
	st := resourceState{skills: map[string]string{}}
	st.baroMD, _ = instructions.Resolve(cwd)
	found, _ := skills.Descobrir(skillSources(cwd))
	for _, sk := range found {
		st.skills[sk.Nome] = sk.Descricao + "\x00" + hashText(sk.Corpo)
	}
	return st
}

// Watcher reports changes owned by another subsystem (the MCP manager
// reconciling config.json): each returned line becomes part of the update.
type Watcher func() []string

// Observar registers a watcher checked at every request boundary.
func (s *Session) Observar(w Watcher) {
	if w == nil {
		return
	}
	s.muExtras.Lock()
	s.watchers = append(s.watchers, w)
	s.muExtras.Unlock()
}

// pendingUpdate compares the resources with what the model was last told and
// returns the update message ("" when nothing changed). The first call only
// records the baseline: that state is already in the frozen system prompt.
func (s *Session) pendingUpdate(cwd string) string {
	var lines []string
	s.muExtras.Lock()
	watchers := append([]Watcher(nil), s.watchers...)
	s.muExtras.Unlock()
	for _, w := range watchers {
		lines = append(lines, w()...)
	}

	cur := currentResources(cwd)
	prev := s.resources
	s.resources = &cur
	if prev != nil {
		if cur.baroMD != prev.baroMD {
			if strings.TrimSpace(cur.baroMD) == "" {
				lines = append(lines, "BARROSO.md was deleted: its earlier instructions no longer apply.")
			} else {
				lines = append(lines, "BARROSO.md changed. These are now the system instructions, replacing the BARROSO.md section of the system prompt and any earlier update:\n\n"+strings.TrimSpace(cur.baroMD))
			}
		}
		var names []string
		for n := range cur.skills {
			names = append(names, n)
		}
		for n := range prev.skills {
			if _, ok := cur.skills[n]; !ok {
				names = append(names, n)
			}
		}
		sort.Strings(names)
		for _, n := range names {
			was, had := prev.skills[n]
			now, has := cur.skills[n]
			switch {
			case !had && has:
				lines = append(lines, "skill "+n+" was created: "+strings.SplitN(now, "\x00", 2)[0]+" (load it with skill_read)")
			case had && !has:
				lines = append(lines, "skill "+n+" was removed")
			case was != now:
				lines = append(lines, "skill "+n+" was edited; load it again with skill_read before using it")
			}
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return UpdatePrefix + " " + strings.Join(lines, "\n"+UpdatePrefix+" ")
}
