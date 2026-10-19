// Persistência do REPL: abre o log das sessões sob config.Dir() e mantém UM id
// ativo por processo. A regra é best-effort — se o banco não abrir, o REPL
// segue em memória com um aviso discreto. Sessão persistente é conveniência
// (retomar, buscar, recuperar), nunca pré-requisito para o turno rodar.
package cli

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/runtime"
	"github.com/ruanbarroso/barroso-harness/internal/store"
)

// nomeBanco é o arquivo do log, dentro do diretório de dados da harness —
// mesma casa do config.json, nunca solto no cwd do usuário.
const nomeBanco = "sessions.db"

// linhaSessao é uma sessão com a contagem de eventos já resolvida: /resume
// precisa mostrar os dois juntos, e consultar o banco linha por linha
// exigiria outra volta por sessão.
type linhaSessao struct {
	store.Session
	Eventos int
	Previa  string // primeira fala do usuário, para quando não há título
}

// abrirStore abre o log. Devolve (nil, "") quando não dá — permissão, disco
// cheio, driver indisponível — e o REPL continua em memória.
func abrirStore(io IO) (*store.Store, string) {
	caminho := filepath.Join(config.Dir(), nomeBanco)
	loja, err := store.Open(caminho)
	if err != nil {
		avisoFmt(io.ErrOut, "sessão sem persistência: "+umaLinhaErro(err))
		return nil, ""
	}
	return loja, novoIDSessao()
}

// novoIDSessao: bh-<YYYYMMDDHHMMSS>-<4hex>. O timestamp torna a ordenação
// legível a olho; os 4 hex aleatórios garantem unicidade quando dois REPLs
// começam no mesmo segundo.
func novoIDSessao() string {
	return fmt.Sprintf("bh-%s-%s", time.Now().Format("20060102150405"), hex4())
}

func hex4() string {
	var b [2]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Sem entropia, o relógio vira a segunda fonte do aleatório.
		return fmt.Sprintf("%04x", time.Now().UnixNano()&0xffff)
	}
	return hex.EncodeToString(b[:])
}

// sessaoCurta é o sufixo da statusbar: os 4 hex finais do id.
func sessaoCurta(id string) string {
	if i := strings.LastIndexByte(id, '-'); i >= 0 && i+1 < len(id) {
		return id[i+1:]
	}
	return id
}

// idadeFormata resume há quanto tempo a sessão foi usada ("agora", "12min",
// "3h", "5d"). Uma linha e sem segundos: ninguém escolhe sessão por segundo.
func idadeFormata(ts string) string {
	t, err := time.Parse(store.TsFormato, ts)
	if err != nil {
		return "?"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "agora"
	case d < time.Hour:
		return fmt.Sprintf("%dmin", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// listarSessoesMonta resume as sessões mais recentes com a contagem de
// eventos — o que /resume lista e o que a escolha do usuário consome.
//
// Sessão com zero eventos fica de fora: ela não tem conversa para retomar. Sem
// esse filtro, um REPL recém-aberto (que já indexa o próprio id vazio) aparece
// no topo da própria lista, e a pessoa vê uma opção que não devolve nada.
func listarSessoesMonta(loja *store.Store, limite int) []linhaSessao {
	ls, err := loja.ListSessions(limite)
	if err != nil {
		return nil
	}
	out := make([]linhaSessao, 0, len(ls))
	for _, se := range ls {
		n, _ := loja.ContarEventos(se.ID)
		if n == 0 {
			continue
		}
		l := linhaSessao{Session: se, Eventos: n}
		if strings.TrimSpace(se.Title) == "" {
			l.Previa = loja.PrimeiraFala(se.ID)
		}
		out = append(out, l)
	}
	return out
}

// listarSessaoResumo imprime a lista numerada de /resume.
func listarSessaoResumo(out io.Writer, ls []linhaSessao) {
	if len(ls) == 0 {
		fmt.Fprintln(out, dimf("nenhuma sessão anterior encontrada"))
		return
	}
	fmt.Fprintln(out, dimf("sessões anteriores — número para retomar:"))
	for i, l := range ls {
		titulo := strings.TrimSpace(l.Title)
		if titulo == "" {
			previa := strings.Join(strings.Fields(l.Previa), " ")
			if r := []rune(previa); len(r) > 60 {
				previa = string(r[:60]) + "…"
			}
			if previa == "" {
				previa = "(sem título)"
			}
			titulo = dimf(previa)
		} else {
			titulo = negritof(titulo)
		}
		fmt.Fprintf(out, "%s %s %s %s %s\n",
			dimf(fmt.Sprintf("%3d", i+1)),
			cianof(l.ID),
			titulo,
			dimf(idadeFormata(l.UpdatedAt)),
			dimf(fmt.Sprintf("%d eventos", l.Eventos)),
		)
	}
}

// resolverSessao traduz a resposta do usuário em id: número da lista (1-based)
// ou prefixo do id. "" quando não casa com nada.
func resolverSessao(entrada string, ls []linhaSessao) string {
	v := strings.TrimSpace(entrada)
	if v == "" {
		return ""
	}
	if n, err := strconv.Atoi(v); err == nil {
		if n >= 1 && n <= len(ls) {
			return ls[n-1].ID
		}
		return ""
	}
	for _, l := range ls {
		if l.ID == v || strings.HasPrefix(l.ID, v) {
			return l.ID
		}
	}
	return ""
}

// umaLinhaErro corta a mensagem na primeira quebra: o aviso de persistência
// é UMA linha no REPL.
func umaLinhaErro(err error) string {
	msg := strings.TrimSpace(err.Error())
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	const teto = 120
	if len(msg) > teto {
		msg = msg[:teto] + "…"
	}
	return msg
}

// statusbarSessao é a faixa de estado com o sufixo de sessão. A linha base
// (provider · model · effort · janela) é a MESMA de statusbar em ui.go; ela
// está repetida aqui porque a faixa não pode ser alterada lá, e o sufixo
// `· sess <4hex>` só aparece quando há log ativo. Se um dia a faixa base
// mudar em ui.go, este espelho precisa acompanhar.
func statusbarSessao(out io.Writer, cfg *config.Config, id string) {
	provider := cfg.CurrentProvider
	if provider == "" {
		provider = "sem-provider"
	}
	modelo := cfg.Model
	if modelo == "" {
		modelo = "selecione um modelo"
	}
	linha := provider + " · " + modelo
	if cfg.Effort != "" {
		linha += " · effort " + cfg.Effort
	} else {
		linha += " · effort off"
	}
	if cfg.Janela > 0 {
		linha += fmt.Sprintf(" · %dk", cfg.Janela/1000)
	}
	if id != "" {
		linha += " · sess " + sessaoCurta(id)
	}
	fmt.Fprintln(out, dimf("╭─ "+linha))
}

// tituloPadrao dá um nome provisório à sessão quando `/save` vem sem
// argumento: os primeiros 60 caracteres da última fala do usuário. Uma sessão
// sem título é indistinguível de outra na lista do /resume.
func tituloPadrao(sess *runtime.Session) string {
	h := sess.History()
	for i := len(h) - 1; i >= 0; i-- {
		if h[i].Role != "user" {
			continue
		}
		t := strings.TrimSpace(h[i].Conteudo)
		if t == "" {
			continue
		}
		t = strings.Join(strings.Fields(t), " ")
		const teto = 60
		if len(t) > teto {
			t = t[:teto] + "…"
		}
		return t
	}
	return "sessão sem título"
}
