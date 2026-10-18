// Testes do contexto vivo: a máquina de estado (aplicar), as três specs e a
// persistência. Um teste por comportamento que o modelo perceberia se quebrou —
// o corpo que volta de um write, a versão que sobe, o teto que bloqueia.
package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/tools"
)

// ── aplicar: read/write/append ──────────────────────────────────────────────

func TestCtxVivoAplicarWriteDepoisReadDevolveExato(t *testing.T) {
	cx := &contextoVivo{}
	corpo := "# Objetivo\nMigrar o store para sqlite.\n\n## Pendências\n- revisar índices"
	saida, err := cx.aplicar("write", corpo)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if !strings.Contains(saida, "versão 1") {
		t.Fatalf("write deveria anunciar versão 1: %q", saida)
	}
	lido, err := cx.aplicar("read", "")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if lido != corpo {
		t.Fatalf("roundtrip alterou o corpo\n querido: %q\n obtido:  %q", corpo, lido)
	}
}

func TestCtxVivoAplicarAppendConcatenaComDuplaQuebra(t *testing.T) {
	cx := &contextoVivo{}
	if _, err := cx.aplicar("write", "primeiro bloco"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := cx.aplicar("append", "segundo bloco"); err != nil {
		t.Fatalf("append: %v", err)
	}
	lido, err := cx.aplicar("read", "")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if lido != "primeiro bloco\n\nsegundo bloco" {
		t.Fatalf("append deveria colar com \\n\\n: %q", lido)
	}
}

func TestCtxVivoAplicarAppendEmVazioDefineCorpo(t *testing.T) {
	cx := &contextoVivo{}
	// Sem write anterior, o append não pode prefixar quebras — senão o arquivo
	// nasceria com linhas em branco de respiro sem nada antes.
	if _, err := cx.aplicar("append", "conteúdo inicial"); err != nil {
		t.Fatalf("append em vazio: %v", err)
	}
	lido, _ := cx.aplicar("read", "")
	if lido != "conteúdo inicial" {
		t.Fatalf("append em vazio deveria definir o corpo: %q", lido)
	}
}

func TestCtxVivoAplicarWriteSubstituiInteiro(t *testing.T) {
	cx := &contextoVivo{}
	cx.aplicar("write", "texto velho que precisa desaparecer")
	cx.aplicar("write", "texto novo")
	lido, _ := cx.aplicar("read", "")
	if lido != "texto novo" {
		t.Fatalf("write deveria substituir por inteiro: %q", lido)
	}
	if strings.Contains(lido, "velho") {
		t.Fatalf("write deixou resto do conteúdo anterior: %q", lido)
	}
}

// ── versões ─────────────────────────────────────────────────────────────────

func TestCtxVivoVersaoIncrementaEMutacaoENaoEmLeitura(t *testing.T) {
	cx := &contextoVivo{}
	if cx.versao() != 0 {
		t.Fatalf("estado novo deveria estar na versão 0, não %d", cx.versao())
	}
	// Leituras repetidas em estado vazio não podem criar versão: o contador
	// mede mutação, não curiosidade.
	for i := 0; i < 3; i++ {
		cx.aplicar("read", "")
	}
	if cx.versao() != 0 {
		t.Fatalf("read bumpou a versão: %d", cx.versao())
	}
	cx.aplicar("write", "a")
	cx.aplicar("read", "")
	cx.aplicar("append", "b")
	cx.aplicar("read", "")
	if cx.versao() != 2 {
		t.Fatalf("duas mutações deveriam dar versão 2, não %d", cx.versao())
	}

	saida, _ := cx.aplicar("write", "reescrito")
	if !strings.Contains(saida, "versão 3") {
		t.Fatalf("terceira mutação deveria anunciar versão 3: %q", saida)
	}
}

// ── teto de 256 KiB ─────────────────────────────────────────────────────────

func TestCtxVivoTetoDe256KiBÉExato(t *testing.T) {
	if tetoCorpoCtxVivo != 256<<10 {
		t.Fatalf("teto deveria ser 256 KiB, é %d", tetoCorpoCtxVivo)
	}
	if tetoLeituraCtxVivo != 48<<10 {
		t.Fatalf("leitura deveria ser tetada em 48 KiB, é %d", tetoLeituraCtxVivo)
	}
}

func TestCtxVivoWriteAcimaDoTetoFalhaComMensagemExata(t *testing.T) {
	cx := &contextoVivo{}
	grande := strings.Repeat("x", tetoCorpoCtxVivo+1)
	_, err := cx.aplicar("write", grande)
	if err == nil {
		t.Fatal("write acima do teto deveria falhar")
	}
	if err.Error() != ctxVivoTeto {
		t.Fatalf("mensagem deveria ser %q, foi %q", ctxVivoTeto, err.Error())
	}
	if strings.Contains(err.Error(), "\n") {
		t.Fatalf("erro de teto deveria ter uma linha só: %q", err.Error())
	}
	// Falhar não pode deixar o corpo anterior pela metade.
	if cx.corpo != "" {
		t.Fatalf("write recusado não deveria alterar o corpo, ficou %d bytes", len(cx.corpo))
	}
}

func TestCtxVivoAppendAlemDoTetoFalha(t *testing.T) {
	cx := &contextoVivo{}
	metade := strings.Repeat("a", tetoCorpoCtxVivo/2)
	if _, err := cx.aplicar("write", metade); err != nil {
		t.Fatalf("write inicial: %v", err)
	}
	_, err := cx.aplicar("append", strings.Repeat("b", tetoCorpoCtxVivo/2+1))
	if err == nil {
		t.Fatal("append que estoura o teto deveria falhar")
	}
	if err.Error() != ctxVivoTeto {
		t.Fatalf("mensagem do teto em append deveria ser %q, foi %q", ctxVivoTeto, err.Error())
	}
	// O append recusado não pode mutar: versão fica onde estava.
	if cx.versao() != 1 {
		t.Fatalf("append recusado não deveria contar versão, foi para %d", cx.versao())
	}
}

// O write substitui, então um corpo NOVO dentro do teto passa mesmo que o
// anterior fosse grande — o tamanho que conta é o do conteúdo que entra.
func TestCtxVivoWriteCompactoPassaDepoisDeCorpoGrande(t *testing.T) {
	cx := &contextoVivo{}
	cx.aplicar("write", strings.Repeat("g", tetoCorpoCtxVivo))
	if _, err := cx.aplicar("write", "compacto"); err != nil {
		t.Fatalf("write compacto deveria passar: %v", err)
	}
	if cx.corpo != "compacto" {
		t.Fatalf("write deveria ter trocado o corpo: %q", cx.corpo)
	}
}

// ── leitura cortada em 48 KiB ───────────────────────────────────────────────

func TestCtxVivoLeituraÉCortadaEm48KiB(t *testing.T) {
	cx := &contextoVivo{}
	cx.corpo = strings.Repeat("z", tetoCorpoCtxVivo) // corpo grande, leitura pequena
	lido, err := cx.aplicar("read", "")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(lido) <= tetoLeituraCtxVivo {
		t.Fatalf("leitura de corpo grande deveria vir cortada, veio %d bytes", len(lido))
	}
	if !strings.Contains(lido, "contexto vivo cortado em 49152 bytes") {
		t.Fatalf("falta o marcador de corte: %q", lido[tetoLeituraCtxVivo:])
	}
	if !strings.HasPrefix(lido, strings.Repeat("z", 100)) {
		t.Fatal("o corte deveria preservar o prefixo do corpo")
	}
}

func TestCtxVivoLeituraDentroDoTetoNãoTemMarcador(t *testing.T) {
	cx := &contextoVivo{}
	cx.corpo = strings.Repeat("z", tetoLeituraCtxVivo)
	lido, _ := cx.aplicar("read", "")
	if strings.Contains(lido, "cortado") {
		t.Fatal("corpo exatamente no teto não deveria ganhar marcador de corte")
	}
}

// ── estado vazio ────────────────────────────────────────────────────────────

func TestCtxVivoLeituraVaziaConvidaAEscrever(t *testing.T) {
	cx := &contextoVivo{}
	lido, err := cx.aplicar("read", "")
	if err != nil {
		t.Fatalf("read em vazio não deveria errar: %v", err)
	}
	if lido != ctxVivoVazio {
		t.Fatalf("vazio deveria devolver %q, devolveu %q", ctxVivoVazio, lido)
	}
	if !strings.Contains(lido, "ctx_write") {
		t.Fatalf("o convite deveria mencionar ctx_write: %q", lido)
	}
}

// Corpo só de espaço é vazio: o modelo que "limpou" com espaços segue vendo o
// convite, não uma tela em branco sem explicação.
func TestCtxVivoCorpoSoComEspaçoÉVazio(t *testing.T) {
	cx := &contextoVivo{}
	cx.corpo = "   \n\t\n  "
	lido, _ := cx.aplicar("read", "")
	if lido != ctxVivoVazio {
		t.Fatalf("corpo só com espaço deveria contar como vazio: %q", lido)
	}
}

// ── operações fora do slice ─────────────────────────────────────────────────

func TestCtxVivoOperacaoInexistenteErra(t *testing.T) {
	cx := &contextoVivo{}
	_, err := cx.aplicar("edit", "algo")
	if err == nil {
		t.Fatal("edit está fora do slice e deveria errar")
	}
	if !strings.Contains(err.Error(), "edit") {
		t.Fatalf("o erro deveria nomear a operação recusada: %q", err.Error())
	}
}

// ── specs ───────────────────────────────────────────────────────────────────

func TestEspecificacoesContextoVivoContemAsTresTools(t *testing.T) {
	lista := EspecificacoesContextoVivo()
	nomes := make([]string, 0, len(lista))
	for _, s := range lista {
		nomes = append(nomes, s.Name)
	}
	quero := []string{"ctx_read", "ctx_write", "ctx_append"}
	if !reflect.DeepEqual(nomes, quero) {
		t.Fatalf("specs deveriam ser %v, foram %v", quero, nomes)
	}
	for _, s := range lista {
		if s.Description == "" {
			t.Fatalf("%s sem description: o provider não sabe quandor usá-la", s.Name)
		}
		if s.Exec == nil {
			t.Fatalf("%s sem Exec: spec decorativa não executa", s.Name)
		}
		if s.Schema == nil {
			t.Fatalf("%s sem Schema", s.Name)
		}
	}
}

func TestEspecificacoesContextoVivoCamposObrigatorios(t *testing.T) {
	for _, s := range EspecificacoesContextoVivo() {
		obrigatorios, _ := s.Schema["required"].([]string)
		switch s.Name {
		case "ctx_read":
			if len(obrigatorios) != 0 {
				t.Fatalf("ctx_read não deveria exigir argumento, exige %v", obrigatorios)
			}
		case "ctx_write", "ctx_append":
			if !reflect.DeepEqual(obrigatorios, []string{"content"}) {
				t.Fatalf("%s deveria exigir exatamente [content], exige %v", s.Name, obrigatorios)
			}
			props, _ := s.Schema["properties"].(map[string]any)
			if props["content"] == nil {
				t.Fatalf("%s sem a propriedade content no schema", s.Name)
			}
			if props["content"].(map[string]any)["type"] != "string" {
				t.Fatalf("%s: content deveria ser string", s.Name)
			}
		default:
			t.Fatalf("spec inesperada: %s", s.Name)
		}
	}
}

func TestCtxVivoSpecReadEmEstadoVazio(t *testing.T) {
	cx := &contextoVivo{}
	specs := especificacoesContextoVivo(cx)
	for _, s := range specs {
		if s.Name != "ctx_read" {
			continue
		}
		saida, err := s.Exec(context.Background(), "", nil)
		if err != nil {
			t.Fatalf("ctx_read em vazio: %v", err)
		}
		if saida != ctxVivoVazio {
			t.Fatalf("ctx_read vazio deveria devolver %q, devolveu %q", ctxVivoVazio, saida)
		}
		return
	}
	t.Fatal("ctx_read ausente das specs")
}

func TestCtxVivoSpecWriteEReapendExecutam(t *testing.T) {
	cx := &contextoVivo{}
	specs := especificacoesContextoVivo(cx)
	porNome := make(map[string]tools.Spec, len(specs))
	for _, s := range specs {
		porNome[s.Name] = s
	}
	// JSON cru, como vem do provider — é o caminho que a Exec realmente recebe.
	saida, err := porNome["ctx_write"].Exec(context.Background(), "", json.RawMessage(`{"content":"estado inicial"}`))
	if err != nil {
		t.Fatalf("ctx_write: %v", err)
	}
	if !strings.Contains(saida, "contexto vivo reescrito (versão 1") {
		t.Fatalf("ctx_write deveria confirmar a versão: %q", saida)
	}
	if _, err := porNome["ctx_append"].Exec(context.Background(), "", json.RawMessage(`{"content":"pendência nova"}`)); err != nil {
		t.Fatalf("ctx_append: %v", err)
	}
	lido, _ := cx.aplicar("read", "")
	if lido != "estado inicial\n\npendência nova" {
		t.Fatalf("write+append pelas specs deveriam compor o corpo: %q", lido)
	}
}

func TestCtxVivoSpecSemContentErraClaro(t *testing.T) {
	cx := &contextoVivo{}
	specs := especificacoesContextoVivo(cx)
	for _, nome := range []string{"ctx_write", "ctx_append"} {
		for _, s := range specs {
			if s.Name != nome {
				continue
			}
			_, err := s.Exec(context.Background(), "", json.RawMessage(`{"outro":"x"}`))
			if err == nil {
				t.Fatalf("%s sem content deveria errar", nome)
			}
			if !strings.Contains(err.Error(), "content") {
				t.Fatalf("%s deveria explicar que falta content: %q", nome, err.Error())
			}
			if strings.Contains(err.Error(), "\n") {
				t.Fatalf("erro de %s deveria ter uma linha só: %q", nome, err.Error())
			}
		}
	}
}

func TestCtxVivoSpecWriteAcimaDoTetoPropagaErro(t *testing.T) {
	cx := &contextoVivo{}
	saida, err := cx.aplicar("write", strings.Repeat("x", tetoCorpoCtxVivo+1))
	if err == nil || saida != "" {
		t.Fatalf("aplicar deveria devolver só o erro no teto: %q / %v", saida, err)
	}
	// E a tool precisa propagar isso, não engolir em "ok".
	for _, s := range especificacoesContextoVivo(cx) {
		if s.Name != "ctx_write" {
			continue
		}
		if _, terr := s.Exec(context.Background(), "", json.RawMessage(`{"content":"`+strings.Repeat("x", tetoCorpoCtxVivo+1)+`"}`)); terr == nil {
			t.Fatal("ctx_write acima do teto deveria errar")
		}
		return
	}
	t.Fatal("ctx_write ausente")
}

// ── persistência ────────────────────────────────────────────────────────────

func TestCtxVivoSalvarECarregarRoundtrip(t *testing.T) {
	dir := t.TempDir()
	homeDe = func() string { return dir }
	t.Cleanup(func() { homeDe = homePadrao })

	cx := &contextoVivo{}
	cx.aplicar("write", "# Objetivo\nfechar o contexto vivo")
	cx.aplicar("append", "- tarefa 1")
	if err := cx.salvar(dir, "sess-1"); err != nil {
		t.Fatalf("salvar: %v", err)
	}
	// O arquivo tem o nome prometido: quem o procura no disco o acha.
	if _, err := os.Stat(filepath.Join(dir, "sess-1.ctx-vivo.md")); err != nil {
		t.Fatalf("arquivo deveria existir em %s: %v", filepath.Join(dir, "sess-1.ctx-vivo.md"), err)
	}
	volta, err := carregarContextoVivo(dir, "sess-1")
	if err != nil {
		t.Fatalf("carregar: %v", err)
	}
	lido, _ := volta.aplicar("read", "")
	if lido != "# Objetivo\nfechar o contexto vivo\n\n- tarefa 1" {
		t.Fatalf("roundtrip alterou o corpo:\n %q", lido)
	}
	// A versão tem de sobreviver: sem ela o modelo reescreveria "versão 1"
	// sobre uma terceira edição e perderia a noção de progresso.
	if volta.versao() != cx.versao() {
		t.Fatalf("versão deveria sobreviver ao roundtrip: %d → %d", cx.versao(), volta.versao())
	}
	if proxima, _ := volta.aplicar("append", "mais um"); !strings.Contains(proxima, "versão 3") {
		t.Fatalf("a mutação seguinte deveria continuar a contagem: %q", proxima)
	}
}

func TestCtxVivoCarregarArquivoAusenteÉVazioSemErro(t *testing.T) {
	dir := t.TempDir()
	cx, err := carregarContextoVivo(dir, "nunca-escrito")
	if err != nil {
		t.Fatalf("arquivo ausente não deveria errar: %v", err)
	}
	if cx == nil {
		t.Fatal("deveria devolver um estado utilizável, não nil")
	}
	lido, _ := cx.aplicar("read", "")
	if lido != ctxVivoVazio {
		t.Fatalf("sessão sem arquivo deveria nascer vazia: %q", lido)
	}
	if cx.versao() != 0 {
		t.Fatalf("sessão sem arquivo deveria estar na versão 0: %d", cx.versao())
	}
}

// Um modelo pode escrever um corpo que começa exatamente com o prefixo do
// cabeçalho; e um crash pode deixar o cabeçalho sem fechamento. Nos dois casos
// o arquivo inteiro é o contexto — nunca perder uma linha por causa de metadado.
func TestCtxVivoCarregarCabeçalhoMalformadoPreservaTudo(t *testing.T) {
	dir := t.TempDir()
	casos := []string{
		"<!-- ctx-vivo:3 -->\ncorpo real do modelo",
		"<!-- ctx-vivo: corpo sem fecho",
		"<!-- ctx-vivo:abc -->\ncorpo",
	}
	for i, conteudo := range casos {
		id := "mal-" + string(rune('a'+i))
		if err := os.WriteFile(filepath.Join(dir, id+".ctx-vivo.md"), []byte(conteudo), 0o600); err != nil {
			t.Fatalf("escrever caso %d: %v", i, err)
		}
		cx, err := carregarContextoVivo(dir, id)
		if err != nil {
			t.Fatalf("caso %d: %v", i, err)
		}
		lido, _ := cx.aplicar("read", "")
		if !strings.Contains(lido, "corpo") {
			t.Fatalf("caso %d perdeu o conteúdo: %q", i, lido)
		}
	}
	// O caso válido, porém, tem de separar cabeçalho de corpo: senão o modelo
	// leria o comentário como se fosse estado escrito por ele.
	cx, _ := carregarContextoVivo(dir, "mal-a")
	lido, _ := cx.aplicar("read", "")
	if strings.Contains(lido, "ctx-vivo:") {
		t.Fatalf("cabeçalho válido deveria ser retirado na leitura: %q", lido)
	}
	if cx.versao() != 3 {
		t.Fatalf("cabeçalho válido deveria restaurar a versão 3, foi %d", cx.versao())
	}
}

func TestCtxVivoSalvarCriaDiretorio(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ainda-nao-existe")
	cx := &contextoVivo{}
	cx.aplicar("write", "algo")
	if err := cx.salvar(dir, "sess-2"); err != nil {
		t.Fatalf("salvar deveria criar o diretório: %v", err)
	}
	volta, err := carregarContextoVivo(dir, "sess-2")
	if err != nil {
		t.Fatalf("carregar: %v", err)
	}
	if lido, _ := volta.aplicar("read", ""); lido != "algo" {
		t.Fatalf("conteúdo perdido: %q", lido)
	}
}

func TestCtxVivoSalvarRecusaSessionIDVazio(t *testing.T) {
	cx := &contextoVivo{}
	cx.aplicar("write", "algo")
	// sessionID vazio geraria um arquivo ".ctx-vivo.md" solto na raiz do home:
	// lixo que nenhum carregar acha de volta.
	if err := cx.salvar(t.TempDir(), "  "); err == nil {
		t.Fatal("sessionID vazio deveria ser recusado")
	}
}

func TestCtxVivoSalvarSobrescreveAtomicamente(t *testing.T) {
	dir := t.TempDir()
	cx := &contextoVivo{}
	cx.aplicar("write", "primeira versão")
	if err := cx.salvar(dir, "sess-3"); err != nil {
		t.Fatalf("primeiro salvar: %v", err)
	}
	cx.aplicar("write", "segunda versão")
	if err := cx.salvar(dir, "sess-3"); err != nil {
		t.Fatalf("segundo salvar: %v", err)
	}
	volta, _ := carregarContextoVivo(dir, "sess-3")
	if lido, _ := volta.aplicar("read", ""); lido != "segunda versão" {
		t.Fatalf("salvar repetido deveria sobrescrever: %q", lido)
	}
	// E não pode deixar temporário para trás: cada salvar deixa o diretório
	// como estava, com o arquivo definitivo apenas.
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ler dir: %v", err)
	}
	if len(ents) != 1 {
		t.Fatalf("deveria sobrar só o arquivo definitivo, sobraram %d entradas", len(ents))
	}
}

// ── estado por sessão ───────────────────────────────────────────────────────

func TestCtxVivoSessaoTemEstadoProprio(t *testing.T) {
	a := NewSession(cfgLocalVazia())
	b := NewSession(cfgLocalVazia())
	defer func() {
		ctxPorSessao.Delete(a)
		ctxPorSessao.Delete(b)
	}()
	// Duas sessões abertas ao mesmo tempo não podem compartilhar corpo: o
	// estado de uma é o trabalho daquela conversa.
	ctxDe(a).aplicar("write", "estado da sessão A")
	ctxDe(b).aplicar("write", "estado da sessão B")
	if lido, _ := ctxDe(a).aplicar("read", ""); lido != "estado da sessão A" {
		t.Fatalf("sessão A deveria manter o seu estado: %q", lido)
	}
	if lido, _ := ctxDe(b).aplicar("read", ""); lido != "estado da sessão B" {
		t.Fatalf("sessão B deveria manter o seu estado: %q", lido)
	}
	// E a mesma sessão devolve SEMPRE o mesmo ponteiro: spec aberta num turno
	// tem de ver a mutação do turno seguinte.
	if ctxDe(a) != ctxDe(a) {
		t.Fatal("ctxDe deveria devolver o mesmo estado para a mesma sessão")
	}
}

func TestCtxVivoSpecsDaSessaoFechamSobreOEstadoDela(t *testing.T) {
	s := NewSession(cfgLocalVazia())
	defer ctxPorSessao.Delete(s)
	for _, sp := range EspecificacoesContextoVivoSessao(s) {
		if sp.Name == "ctx_write" {
			if _, err := sp.Exec(context.Background(), "", json.RawMessage(`{"content":"estado desta sessão"}`)); err != nil {
				t.Fatalf("ctx_write da sessão: %v", err)
			}
		}
	}
	if lido, _ := ctxDe(s).aplicar("read", ""); lido != "estado desta sessão" {
		t.Fatalf("a spec deveria ter gravado no estado da sessão: %q", lido)
	}
	// E o catálogo sem sessão continua independente.
	if lido, _ := ctxVivoSemSessao.aplicar("read", ""); lido != ctxVivoVazio {
		t.Fatalf("estado sem sessão deveria continuar vazio: %q", lido)
	}
}

func TestCtxVivoSessaoSemPersistenciaSalvaSemErro(t *testing.T) {
	s := NewSession(cfgLocalVazia())
	// Nenhum arquivo deveria nascer: sem sessionID não há onde guardar.
	if err := s.ContextoVivoSalvar(); err != nil {
		t.Fatalf("salvar sem persistência deveria ser silencioso: %v", err)
	}
	if err := s.ContextoVivoCarregar(); err != nil {
		t.Fatalf("carregar sem persistência deveria ser silencioso: %v", err)
	}
}

func TestCtxVivoSessaoSalvaECarregaComHomeInjetado(t *testing.T) {
	dir := t.TempDir()
	homeDe = func() string { return dir }
	t.Cleanup(func() { homeDe = homePadrao })

	s := NewSession(cfgLocalVazia())
	defer ctxPorSessao.Delete(s)
	s.sessionID = "sess-inj"
	s.store = nil
	// Persistente() exige store; aqui o que interessa é o par
	// salvar/carregar lendo o home injetado.
	if s.Persistente() {
		t.Fatal("pré-condição: sessão deveria estar sem persistência")
	}
	ctxDe(s).aplicar("write", "estado injetado")
	ctxDe(s).salvar(dir, s.sessionID)

	// Estado novo, como um Resume num processo novo.
	outra := NewSession(cfgLocalVazia())
	defer ctxPorSessao.Delete(outra)
	outra.sessionID = "sess-inj"
	outra.store = nil
	if err := outra.ContextoVivoCarregar(); err == nil {
		// Sem store, o wrapper é silencioso; a leitura direta é o caminho real.
		t.Log("wrapper sem store não carrega — esperado")
	}
	cx, err := carregarContextoVivo(homeDe(), outra.sessionID)
	if err != nil {
		t.Fatalf("carregar: %v", err)
	}
	if lido, _ := cx.aplicar("read", ""); lido != "estado injetado" {
		t.Fatalf("o estado deveria voltar do disco: %q", lido)
	}
}

// homePadrao guarda a implementação original de homeDe, para que um teste que
// injetou um diretório temporário não vaze a injeção para os seguintes.
var homePadrao = func() string {
	if v := os.Getenv("BARROSO_HARNESS_HOME"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home, _ = os.Getwd()
	}
	return filepath.Join(home, ".barroso-harness")
}

// cfgLocalVazia monta uma configuração sem provider/model: as sessões destes
// testes nunca chamam Send, só precisam de um ponteiro estável para servir de
// chave em ctxPorSessao. Existe aqui (e não reaproveitando helper de outro
// arquivo) porque os arquivos irmãos estão sendo editados em paralelo.
func cfgLocalVazia() config.Config { return config.Config{} }
