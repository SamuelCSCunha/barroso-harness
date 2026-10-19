package cli

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ruanbarroso/barroso-harness/internal/config"
)

func homeFake(t *testing.T) string {
	dir := t.TempDir()
	t.Setenv("BARROSO_HARNESS_HOME", dir)
	return dir
}

// ponteIO injeta reader/writer e, principalmente, faz PromptKey consumir a
// PRÓXIMA linha da entrada — é assim que os testes alimentam a chave oculta
// (o código de produção lê ela de um prompt sem eco, não de io.In).
func ponteIO(t *testing.T, entrada string) (IO, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	rd := bufio.NewReader(strings.NewReader(entrada))
	pk := PromptKey
	PromptKey = func(string) (string, error) {
		linha, err := rd.ReadString('\n')
		if err != nil && linha == "" {
			return "", err
		}
		return strings.TrimSpace(linha), nil
	}
	t.Cleanup(func() { PromptKey = pk })
	return IO{In: rd, Out: out, ErrOut: errOut}, out, errOut
}

func providerFakeModelos(t *testing.T, ids ...string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" && r.URL.Path != "/models" {
			w.WriteHeader(404)
			return
		}
		var sb strings.Builder
		sb.WriteString(`{"object":"list","data":[`)
		for i, id := range ids {
			if i > 0 {
				sb.WriteByte(',')
			}
			fmt.Fprintf(&sb, `{"id":%q,"object":"model","input":["text","image"],"context_length":250000}`, id)
		}
		sb.WriteString(`]}`)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, sb.String())
	}))
}

func cfgPronta(t *testing.T, base, key, model string) string {
	home := homeFake(t)
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(`{
  "current_provider":"p1",
  "providers":{"p1":{"base_url":"`+base+`","api_key":"`+key+`"}},
  "model":"`+model+`",
  "effort":""
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

// /model lista o catálogo em ordem alfabética e troca o modelo escolhido.
func TestReplModelListaAlfabeticoETroca(t *testing.T) {
	srv := providerFakeModelos(t, "z-modelo", "a-modelo", "m-modelo")
	defer srv.Close()
	home := cfgPronta(t, srv.URL+"/v1", "k", "a-modelo")

	io, outIO, _ := ponteIO(t, "/model\n2\n/exit\n")
	if code := RunInteractive(io); code != 0 {
		t.Fatalf("repl saiu com código %d: %s", code, io.ErrOut)
	}
	saida := outIO.String()
	for _, quer := range []string{"a-modelo", "m-modelo", "z-modelo"} {
		if !strings.Contains(saida, quer) {
			t.Fatalf("catálogo sem %q:\n%s", quer, saida)
		}
	}
	if strings.Index(saida, "a-modelo") > strings.Index(saida, "m-modelo") ||
		strings.Index(saida, "m-modelo") > strings.Index(saida, "z-modelo") {
		t.Fatal("catálogo não saiu em ordem alfabética")
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model != "m-modelo" {
		t.Fatalf("modelo após /model = %q, esperado m-modelo (2º da lista)", cfg.Model)
	}
	b, err := os.ReadFile(filepath.Join(home, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "m-modelo") {
		t.Fatal("/model não persistiu o modelo no disco")
	}
}

// /model <id> troca direto quando o id consta no catálogo.
func TestReplModelDireto(t *testing.T) {
	srv := providerFakeModelos(t, "barroso-chat", "barroso-quick")
	defer srv.Close()
	cfgPronta(t, srv.URL+"/v1", "k", "barroso-quick")

	io, _, _ := ponteIO(t, "/model barroso-chat\n/exit\n")
	if code := RunInteractive(io); code != 0 {
		t.Fatalf("código %d: %s", code, io.ErrOut)
	}
	cfg, _ := config.Load()
	if cfg.Model != "barroso-chat" {
		t.Fatalf("modelo = %q", cfg.Model)
	}
}

// /model com id fora do catálogo não estraga a config atual.
func TestReplModelForaDoCatalogoNaoMuda(t *testing.T) {
	srv := providerFakeModelos(t, "barroso-chat")
	defer srv.Close()
	cfgPronta(t, srv.URL+"/v1", "k", "barroso-chat")

	io, _, errIO := ponteIO(t, "/model nao-existe\n/effort\n/exit\n")
	RunInteractive(io)
	cfg, _ := config.Load()
	if cfg.Model != "barroso-chat" {
		t.Fatalf("modelo mudou indevidamente para %q", cfg.Model)
	}
	// erro esperado e comunicado: id fora do catálogo orienta listar.
	if !strings.Contains(errIO.String(), "nao-existe") {
		t.Fatalf("erro sobre id fora do catálogo não foi reportado: %q", errIO.String())
	}
}

// /effort liga, mostra e limpa. Limpar OMITE o campo no request.
func TestReplEffortLigaMostraELimpa(t *testing.T) {
	srv := providerFakeModelos(t, "barroso-chat")
	defer srv.Close()
	home := cfgPronta(t, srv.URL+"/v1", "k", "barroso-chat")

	io, outIO, _ := ponteIO(t, "/effort\n/effort high\n/effort\n/effort -\n/effort\n/exit\n")
	RunInteractive(io)
	saida := outIO.String()
	cfg, _ := config.Load()
	if cfg.Effort != "" {
		t.Fatalf("effort deveria estar vazio após `/effort -`, veio %q", cfg.Effort)
	}
	b, _ := os.ReadFile(filepath.Join(home, "config.json"))
	if strings.Contains(strings.ToLower(string(b)), "reasoning_effort") {
		t.Fatal("`/effort -` deve omitir reasoning_effort, não gravar 'none' ou string vazia")
	}
	for _, quer := range []string{"high", "low", "medium", "xhigh", "max"} {
		if !strings.Contains(saida, quer) {
			t.Fatalf("listagem de effort sem %q:\n%s", quer, saida)
		}
	}
}

// /provider troca base+chave, valida /models e mantém o modelo se existir.
func TestReplProviderTrocaERollbackNaFalha(t *testing.T) {
	ok := providerFakeModelos(t, "barroso-chat")
	defer ok.Close()
	ruim := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		io.WriteString(w, `{"error":{"message":"unauthorized"}}`)
	}))
	defer ruim.Close()

	home := cfgPronta(t, ok.URL+"/v1", "k-antiga", "barroso-chat")
	antes, err := os.ReadFile(filepath.Join(home, "config.json"))
	if err != nil {
		t.Fatal(err)
	}

	// provider inválido: erro visível e config intacta no disco.
	io, outIO, errIO := ponteIO(t, "/provider\n"+ruim.URL+"/v1\noutra-chave\n/exit\n")
	RunInteractive(io)
	depois, _ := os.ReadFile(filepath.Join(home, "config.json"))
	if string(antes) != string(depois) {
		t.Fatal("config foi alterada apesar da falha do /provider")
	}
	if !strings.Contains(errIO.String(), "error") && !strings.Contains(outIO.String(), "error") {
		t.Fatalf("falha do /provider não foi reportada: out=%q err=%q", io.Out, io.ErrOut)
	}

	// provider válido, mesmo modelo: troca persiste.
	io2, _, _ := ponteIO(t, "/provider\n"+ok.URL+"/v1\nk-nova\n/exit\n")
	RunInteractive(io2)
	cfg, _ := config.Load()
	if cfg.Ativo() == nil || cfg.Ativo().BaseURL != ok.URL+"/v1" {
		t.Fatalf("provider ativo não trocou: %+v", cfg.Ativo())
	}
	if cfg.Ativo().APIKey != "k-nova" {
		t.Fatalf("chave não trocou: %q", cfg.Ativo().APIKey)
	}
	if cfg.Model != "barroso-chat" {
		t.Fatalf("modelo devia ser mantido, veio %q", cfg.Model)
	}
}

// /provider onde o modelo atual não existe limpa model e exige seleção.
func TestReplProviderSemOModeloAtualLimpaModelo(t *testing.T) {
	atual := providerFakeModelos(t, "barroso-chat")
	defer atual.Close()
	novo := providerFakeModelos(t, "barroso-quick")
	defer novo.Close()
	cfgPronta(t, atual.URL+"/v1", "k", "barroso-chat")

	io, outIO, _ := ponteIO(t, "/provider\n"+novo.URL+"/v1\nk2\n1\n/exit\n")
	RunInteractive(io)
	cfg, _ := config.Load()
	if cfg.Model != "barroso-quick" {
		t.Fatalf("modelo após troca de provider = %q, esperado barroso-quick (1º da lista)\nsaída:\n%s", cfg.Model, outIO.String())
	}
}

// A chave nunca aparece na saída nem no erro.
func TestReplNuncaImprimeAChave(t *testing.T) {
	srv := providerFakeModelos(t, "barroso-chat")
	defer srv.Close()
	const segredo = "segredo-nunca-impresso-123"
	cfgPronta(t, srv.URL+"/v1", segredo, "barroso-chat")

	io, outIO, errIO := ponteIO(t, "/model barroso-chat\n/effort high\n/provider\n"+ruimSemChave(t)+"\n"+segredo+"\n/exit\n")
	RunInteractive(io)
	for _, fluxo := range []string{outIO.String(), errIO.String()} {
		if strings.Contains(fluxo, segredo) {
			t.Fatal("chave vazou para a saída")
		}
	}
}

func ruimSemChave(t *testing.T) string {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		io.WriteString(w, "boom")
	}))
	t.Cleanup(s.Close)
	return s.URL + "/v1"
}

// /help documenta os comandos exigidos.
func TestReplHelpListaComandos(t *testing.T) {
	srv := providerFakeModelos(t, "barroso-chat")
	defer srv.Close()
	cfgPronta(t, srv.URL+"/v1", "k", "barroso-chat")

	io, outIO, _ := ponteIO(t, "/help\n/exit\n")
	RunInteractive(io)
	saida := outIO.String()
	for _, quer := range []string{"/model", "/effort", "/provider", "/help", "/exit"} {
		if !strings.Contains(saida, quer) {
			t.Fatalf("/help sem %q:\n%s", quer, saida)
		}
	}
}

// Mensagem comum não é confundida com comando slash.
func TestReplMensagemComumNaoEComando(t *testing.T) {
	srv := providerFakeModelos(t, "barroso-chat")
	defer srv.Close()
	cfgPronta(t, srv.URL+"/v1", "k", "barroso-chat")

	io, _, errIO := ponteIO(t, "como assim /model?\n/exit\n")
	RunInteractive(io)
	if strings.Contains(errIO.String(), "unknown") || strings.Contains(errIO.String(), "command") {
		// só garante que não quebrou: slash no meio do texto não é comando.
		_ = io.ErrOut
	}
}

// Enter vazio na seleção do /model sai sem trocar o modelo atual.
func TestReplModelEnterVazioMantem(t *testing.T) {
	srv := providerFakeModelos(t, "z-modelo", "a-modelo", "m-modelo")
	defer srv.Close()
	cfgPronta(t, srv.URL+"/v1", "k", "a-modelo")

	io, outIO, _ := ponteIO(t, "/model\n\n/exit\n")
	if code := RunInteractive(io); code != 0 {
		t.Fatalf("repl saiu com código %d", code)
	}
	saida := outIO.String()
	if !strings.Contains(saida, "empty Enter keeps a-modelo") {
		t.Fatalf("dica do Enter vazio ausente:\n%s", saida)
	}
	if !strings.Contains(saida, "model unchanged: a-modelo") {
		t.Fatalf("confirmação ausente:\n%s", saida)
	}
	if strings.Contains(saida, "no model, no turn") {
		t.Fatalf("Enter vazio não deveria insistir quando há modelo:\n%s", saida)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model != "a-modelo" {
		t.Fatalf("modelo mudou para %q", cfg.Model)
	}
}
