package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/ruanbarroso/barroso-harness/internal/dealer"
)

// printLastExchange shows, after /resume in the scrolling mode, the user's last
// request and the reply that followed it. Synthetic user messages (compaction,
// sub-agent notices, output-limit continuations, updates) are skipped: they
// are not something the user typed.
func printLastExchange(out io.Writer, h []dealer.Mensagem) {
	lastUser, lastReply := -1, -1
	for i, m := range h {
		switch m.Role {
		case "user":
			c := strings.TrimSpace(m.Conteudo)
			if c == "" || strings.HasPrefix(c, "[") || isWorkingState(c) {
				continue
			}
			lastUser, lastReply = i, -1
		case "assistant":
			if strings.TrimSpace(m.Conteudo) != "" {
				lastReply = i
			}
		}
	}
	if lastUser < 0 && lastReply < 0 {
		return
	}
	fmt.Fprintln(out)
	if lastUser >= 0 {
		for _, l := range strings.Split(wrap(strings.TrimSpace(h[lastUser].Conteudo), textWidth()-6), "\n") {
			if colorsOn {
				fmt.Fprintln(out, "  "+bluef(bar)+" "+color(colorText, l))
			} else {
				fmt.Fprintln(out, "> "+l)
			}
		}
	}
	if lastReply >= 0 {
		fmt.Fprintln(out)
		if colorsOn {
			fmt.Fprintln(out, renderMarkdown(h[lastReply].Conteudo, textWidth()))
		} else {
			fmt.Fprintln(out, strings.TrimSpace(h[lastReply].Conteudo))
		}
	}
	fmt.Fprintln(out)
}
