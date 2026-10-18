// Testes de persistência do REPL: /resume (listar, escolher, retomar),
// /save (título), a sufixo `· sess <4hex>` da statusbar e o degrade best-effort
// quando o banco não abre.
package cli

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/store"
)

func TestIDFormatoEUnicidade(t *testing.T) {
	vistos := map[string]bool{}
	for i := 0; i < 50; i++ {
		id := novoIDSessao()
		partes := strings.Split(id, "-")
		if len(partes) != 3 {
			t.Fatalf("id devia ter 3 partes separadas por '-': %q", id)
		}
		if len(partes[1]) != 14 {
			t.Fatalf("timestamp YYYYMMDDHHMMSS devia ter 14 dígitos, veio %q", partes[1])
		}
		if len(partes[2]) != 4 {
			t.Fatalf("sufixo aleatório devia ter 4 hex, veio %q", partes[2])
		}
		if vistos[id] {
			t.Fatalf("id repetido: %q", id)
		}
		vistos[id] = true
	}
	if curta := sessaoCurta(novoIDSessao()); len(curta) != 4 {
		t.Fatalf("sessaoCurta devia devolver os 4 hex finais, veio %q", curta)
	}
}

func TestIdadeFormata(t *testing.T) {
	agora := time.Now().UTC()
	casos := []struct {
		atras    time.Duration
		contem   string
		querExat string
	}{
		{10 * time.Second, "", "agora"},
		{5 * time.Minute, "min", ""},
		{3 * time.Hour, "h", ""},
		{48 * time.Hour, "d", ""},
	}
	for _, c := range casos {
		ts := agora.Add(-c.atras).Format(store.TsFormato)
		veio := idadeFormata(ts)
		if c.querExat != "" && veio != c.querExat {
			t.Fatalf("idade de %v = %q, esperado %q", c.atras, veio, c.querExat)
		}
		if c.contem != "" && !strings.Contains(veio, c.contem) {
			t.Fatalf("idade de %v = %q, deveria conter %q", c.atras, veio, c.contem)
		}
	}
	if idadeFormata("lixo") != "?" {
		t.Fatal("timestamp inválido deveria devolver '?'")
	}
}

func TestResolverSessaoNumeroEPrefixo(t *testing.T) {
	ls := []linhaSessao{
		{Session: store.Session{ID: "bh-20240101100000-aaaa", Title: "uma"}},
		{Session: store.Session{ID: "bh-20240101110000-bbbb", Title: "duas"}},
		{Session: store.Session{ID: "bh-20240101120000-cccc", Title: "três"}},
	}
	// Número da lista (1-based).
	if id := resolverSessao("2", ls); id != "bh-20240101110000-bbbb" {
		t.Fatalf("resolver 2 = %q", id)
	}
	// Prefixo do id e id completo.
	if id := resolverSessao("bh-20240101120000", ls); id != "bh-20240101120000-cccc" {
		t.Fatalf("resolver prefixo = %q", id)
	}
	if id := resolverSessao("bh-20240101100000-aaaa", ls); id != "bh-20240101100000-aaaa" {
		t.Fatalf("resolver id completo = %q", id)
	}
	// Fora da lista ou vazio: nada escolhido.
	for _, entrada := range []string{"", "  ", "99", "0", "-1", "nao-existe"} {
		if id := resolverSessao(entrada, ls); id != "" {
			t.Fatalf("resolver %q deveria falhar, veio %q", entrada, id)
		}
	}
}

func TestStatusbarSessaoSufixo(t *testing.T) {
	cfg := &config.Config{CurrentProvider: "p1", Model: "mod"}
	var sb strings.Builder
	statusbarSessao(&sb, cfg, "bh-20240101100000-abcd")
	saida := sb.String()
	if !strings.Contains(saida, "· sess abcd") {
		t.Fatalf("statusbar sem o sufixo de sessão:\n%s", saida)
	}
	// A linha base continua igual à da ui.go (provider · model · effort).
	for _, quer := range []string{"p1", "mod", "effort off"} {
		if !strings.Contains(saida, quer) {
			t.Fatalf("statusbar perdeu %q:\n%s", quer, saida)
		}
	}
	// Sem sessão ativa, nenhum sufixo.
	sb.Reset()
	statusbarSessao(&sb, cfg, "")
	if strings.Contains(sb.String(), "sess") {
		t.Fatalf("sem sessão não devia haver sufixo:\n%s", sb.String())
	}
}

func TestUmaLinhaErro(t *testing.T) {
	veio := umaLinhaErro(errTeste{"primeira linha\ndetalhe de protocolo\nmais outra"})
	if strings.Contains(veio, "detalhe") || strings.Contains(veio, "\n") {
		t.Fatalf("umaLinhaErro deveria manter só a primeira linha: %q", veio)
	}
	if veio != "primeira linha" {
		t.Fatalf("umaLinhaErro = %q", veio)
	}
}

type errTeste struct{ s string }

func (e errTeste) Error() string { return e.s }

// ── ponta a ponta pelo REPL ───────────────────────────────────────────────

// semCores desliga a paleta para os asserts verem texto cru.
func semCores(t *testing.T) {
	t.Helper()
	antiga := coresLigadas
	coresLigadas = false
	t.Cleanup(func() { coresLigadas = antiga })
}

// /resume lista as sessões gravadas e retoma a escolhida.
func TestResumeRetomaSessaoDoLog(t *testing.T) {
	home := homeFake(t)
	semCores(t)
	// Uma sessão anterior com conteúdo no log. cfgPronta cria o próprio
	// diretório de dados, então ela vem primeiro e é o home REAL do teste.
	srv := providerFakeModelos(t, "barroso-chat")
	defer srv.Close()
	home = cfgPronta(t, srv.URL+"/v1", "k", "barroso-chat")

	loja, err := store.Open(filepath.Join(home, "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	const anterior = "bh-20240101100000-abcd"
	if err := loja.NewSession(anterior); err != nil {
		t.Fatal(err)
	}
	for _, ev := range []struct {
		kind    string
		payload map[string]any
	}{
		{"user_message", map[string]any{"role": "user", "content": "pergunta anterior"}},
		{"assistant", map[string]any{"role": "assistant", "content": "resposta anterior"}},
	} {
		if _, err := loja.StoreEvent(anterior, ev.kind, ev.payload); err != nil {
			t.Fatal(err)
		}
	}
	if err := loja.SetTitle(anterior, "conversa antiga"); err != nil {
		t.Fatal(err)
	}
	if err := loja.Fechar(); err != nil {
		t.Fatal(err)
	}

	// REPL novo: /resume, escolha pelo número, /exit.
	io, outIO, _ := ponteIO(t, "/resume\n1\n/exit\n")
	if code := RunInteractive(io); code != 0 {
		t.Fatalf("repl saiu com %d: %s", code, io.ErrOut)
	}
	tela := outIO.String()
	for _, quer := range []string{anterior, "conversa antiga", "2 eventos", "retomada"} {
		if !strings.Contains(tela, quer) {
			t.Fatalf("/resume sem %q:\n%s", quer, tela)
		}
	}

	// O log é append-only: a sessão anterior continua inteira, com título.
	loja2, err := store.Open(filepath.Join(home, "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer loja2.Fechar()
	se, err := loja2.GetSession(anterior)
	if err != nil {
		t.Fatalf("sessão anterior sumiu do log: %v", err)
	}
	if se.Title != "conversa antiga" {
		t.Fatalf("título = %q", se.Title)
	}
	if n, _ := loja2.ContarEventos(anterior); n != 2 {
		t.Fatalf("eventos da sessão anterior = %d; o log é append-only", n)
	}
}

// /save grava o título; a listagem seguinte o mostra.
func TestSaveGravaTitulo(t *testing.T) {
	semCores(t)
	srv := providerFakeModelos(t, "barroso-chat")
	defer srv.Close()
	cfgPronta(t, srv.URL+"/v1", "k", "barroso-chat")

	io, outIO, _ := ponteIO(t, "/save minha conversa de teste\n/exit\n")
	if code := RunInteractive(io); code != 0 {
		t.Fatalf("código %d: %s", code, io.ErrOut)
	}
	if !strings.Contains(outIO.String(), "salva") {
		t.Fatalf("/save não confirmou:\n%s", outIO.String())
	}

	// O banco é o do config.Dir() ativo (o mesmo que o REPL usou).
	loja, err := store.Open(filepath.Join(config.Dir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer loja.Fechar()
	ls, err := loja.ListSessions(10)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(ls) != 1 {
		t.Fatalf("sessões = %d; esperava 1: %+v", len(ls), ls)
	}
	if ls[0].Title != "minha conversa de teste" {
		t.Fatalf("título gravado = %q", ls[0].Title)
	}
}

// Uma sessão vazia não aparece em /resume — não há conversa para retomar.
func TestResumeSemSessoesAvisa(t *testing.T) {
	semCores(t)
	srv := providerFakeModelos(t, "barroso-chat")
	defer srv.Close()
	cfgPronta(t, srv.URL+"/v1", "k", "barroso-chat")

	io, outIO, _ := ponteIO(t, "/resume\n/exit\n")
	if code := RunInteractive(io); code != 0 {
		t.Fatalf("código %d: %s", code, io.ErrOut)
	}
	if !strings.Contains(outIO.String(), "nenhuma sessão") {
		t.Fatalf("/resume vazio não avisou:\n%s", outIO.String())
	}
}
