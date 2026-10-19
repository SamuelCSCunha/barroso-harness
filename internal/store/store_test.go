package store

import (
	"path/filepath"
	"strings"
	"testing"
)

// lojaAbre em diretório temporário; nada de rede, nada de disco do usuário.
func lojaAbre(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatalf("abrir store: %v", err)
	}
	t.Cleanup(func() { _ = st.Fechar() })
	return st
}

func TestOpenCriaArquivoESchema(t *testing.T) {
	dir := t.TempDir()
	caminho := filepath.Join(dir, "sessions.db")
	st, err := Open(caminho)
	if err != nil {
		t.Fatalf("abrir: %v", err)
	}
	defer st.Fechar()

	// WAL deixa o arquivo em disco; se o schema não rodou, toda consulta abaixo
	// estouraria, então o teste já cobre as duas coisas.
	for _, tabela := range []string{"sessions", "events", "blobs"} {
		var n int
		if err := st.db.QueryRow(`SELECT COUNT(*) FROM ` + tabela).Scan(&n); err != nil {
			t.Fatalf("tabela %s não existe após o schema: %v", tabela, err)
		}
	}
	if st.Caminho() != caminho {
		// Caminho absoluto é normalizado; no Windows o case pode divergir, então
		// só conferimos a terminação.
		if !strings.HasSuffix(st.Caminho(), "sessions.db") {
			t.Fatalf("caminho inesperado: %q", st.Caminho())
		}
	}
}

func TestOpenMesmoCaminhoDevolveMesmoStore(t *testing.T) {
	caminho := filepath.Join(t.TempDir(), "sessions.db")
	a, err := Open(caminho)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Fechar()
	b, err := Open(caminho)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatal("abrir o mesmo caminho duas vezes criou duas conexões")
	}
}

func TestAppendELeEventos(t *testing.T) {
	st := lojaAbre(t)
	const id = "bh-20240101120000-abcd"
	if err := st.NewSession(id); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	seq1, err := st.StoreEvent(id, "user_message", map[string]any{"role": "user", "content": "oi"})
	if err != nil {
		t.Fatalf("StoreEvent 1: %v", err)
	}
	seq2, err := st.StoreEvent(id, "assistant", map[string]any{"role": "assistant", "content": "olá"})
	if err != nil {
		t.Fatalf("StoreEvent 2: %v", err)
	}
	if seq1 != 1 || seq2 != 2 {
		t.Fatalf("seqs = %d, %d; esperados 1, 2 (autoincrement do zero)", seq1, seq2)
	}

	todos, err := st.ListEvents(id, 0, 0)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(todos) != 2 {
		t.Fatalf("eventos = %d; esperado 2", len(todos))
	}
	if todos[0].Kind != "user_message" || todos[1].Kind != "assistant" {
		t.Fatalf("ordem dos eventos invertida: %q, %q", todos[0].Kind, todos[1].Kind)
	}
	if todos[0].Payload != `{"content":"oi","role":"user"}` {
		t.Fatalf("payload com chaves fora de ordem: %s", todos[0].Payload)
	}

	// fromSeq é inclusive.
	parcial, err := st.ListEvents(id, seq2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(parcial) != 1 || parcial[0].Seq != seq2 {
		t.Fatalf("fromSeq inclusivo falhou: %+v", parcial)
	}

	// limit conta do fim para trás do intervalo pedido.
	limitado, err := st.ListEvents(id, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(limitado) != 1 || limitado[0].Seq != seq1 {
		t.Fatalf("limit deveria trazer o primeiro evento: %+v", limitado)
	}

	um, err := st.GetEvent(id, seq2)
	if err != nil {
		t.Fatalf("GetEvent: %v", err)
	}
	if um.Kind != "assistant" {
		t.Fatalf("GetEvent seq %d devolveu %q", seq2, um.Kind)
	}
	if _, err := st.GetEvent(id, 999); err == nil {
		t.Fatal("GetEvent de seq inexistente deveria falhar")
	}
}

func TestBlobRoundtrip(t *testing.T) {
	st := lojaAbre(t)
	const id = "bh-20240101130000-0001"
	dados := []byte(strings.Repeat("saída grande de tool\n", 2000))

	ref, err := st.PutBlob(dados)
	if err != nil {
		t.Fatalf("PutBlob: %v", err)
	}
	hash, _ := ref["blob"].(string)
	if len(hash) != 64 {
		t.Fatalf("hash sha256 deveria ter 64 hex, veio %q", hash)
	}
	if ref["size"].(int) != len(dados) {
		t.Fatalf("size = %v, esperado %d", ref["size"], len(dados))
	}

	volta, err := st.GetBlob(hash)
	if err != nil {
		t.Fatalf("GetBlob: %v", err)
	}
	if string(volta) != string(dados) {
		t.Fatal("blob não voltou igual")
	}

	// Idempotente: o mesmo conteúdo não duplica linha nem muda o hash.
	ref2, err := st.PutBlob(dados)
	if err != nil {
		t.Fatal(err)
	}
	if ref2["blob"] != ref["blob"] {
		t.Fatalf("hash instável: %v vs %v", ref2["blob"], ref["blob"])
	}
	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM blobs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("blobs duplicados: %d linhas", n)
	}

	// Evento que referencia o blob: o payload fica curto e a reidratação
	// devolve o conteúdo inteiro.
	payload := map[string]any{
		"role":    "tool_result",
		"content": "conteúdo enorme",
		"blob":    hash,
		"size":    len(dados),
		"preview": "primeiros bytes…",
	}
	if _, err := st.StoreEvent(id, "tool_result", payload); err != nil {
		t.Fatal(err)
	}
	eventos, err := st.ListEvents(id, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(eventos) != 1 {
		t.Fatalf("eventos = %d", len(eventos))
	}
	if len(eventos[0].Payload) > 512 {
		t.Fatalf("payload com blob ficou grande (%d bytes)", len(eventos[0].Payload))
	}
	cheio, err := st.Rehydrate(eventos[0].Payload)
	if err != nil {
		t.Fatal(err)
	}
	// Reidratar devolve o conteúdo INTEIRO, sem repetir o preview: o preview é
	// o começo do próprio blob, e somá-los duplicaria os primeiros bytes.
	if cheio != string(dados) {
		t.Fatalf("reidratação deveria devolver o blob inteiro (%d bytes), veio %d", len(dados), len(cheio))
	}

	// Blob inexistente não quebra: devolve o payload como está.
	if _, err := st.GetBlob(strings.Repeat("0", 64)); err == nil {
		t.Fatal("GetBlob de hash inexistente deveria falhar")
	}
	semBlob, err := st.Rehydrate(`{"role":"user","content":"oi"}`)
	if err != nil || semBlob != `{"role":"user","content":"oi"}` {
		t.Fatalf("payload sem blob voltou alterado: %q (%v)", semBlob, err)
	}
}

func TestListSessionsOrdenaPorUpdatedAtDesc(t *testing.T) {
	st := lojaAbre(t)
	ids := []string{"s1", "s2", "s3"}
	for _, id := range ids {
		if err := st.NewSession(id); err != nil {
			t.Fatal(err)
		}
	}
	// updated_at tem resolução de milissegundo; um evento em cada uma garante
	// timestamps distintos sem dormir no teste.
	for _, id := range []string{"s1", "s2", "s3"} {
		if _, err := st.StoreEvent(id, "user_message", map[string]any{"content": id}); err != nil {
			t.Fatal(err)
		}
	}
	// s2 foi a última usada → topo da lista.
	if err := st.TouchSession("s2"); err != nil {
		t.Fatal(err)
	}
	lista, err := st.ListSessions(10)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(lista) != 3 {
		t.Fatalf("sessões = %d", len(lista))
	}
	if lista[0].ID != "s2" {
		t.Fatalf("a mais recente devia ser s2, veio %q", lista[0].ID)
	}

	n, err := st.ContarEventos("s1")
	if err != nil || n != 1 {
		t.Fatalf("ContarEventos(s1) = %d, %v", n, err)
	}
	if n, _ := st.ContarEventos("nao-existe"); n != 0 {
		t.Fatalf("ContarEventos de inexistente = %d", n)
	}

	// Título e GetSession.
	if err := st.SetTitle("s2", "conversa sobre o log"); err != nil {
		t.Fatal(err)
	}
	se, err := st.GetSession("s2")
	if err != nil {
		t.Fatal(err)
	}
	if se.Title != "conversa sobre o log" {
		t.Fatalf("título = %q", se.Title)
	}
	if se.CreatedAt == "" || se.UpdatedAt == "" {
		t.Fatal("timestamps não preenchidos")
	}
	if _, err := st.GetSession("nao-existe"); err == nil {
		t.Fatal("GetSession de inexistente deveria falhar")
	}

	// limit respeitado.
	soDuas, _ := st.ListSessions(2)
	if len(soDuas) != 2 {
		t.Fatalf("limit=2 devolveu %d", len(soDuas))
	}
}

func TestBuscarPorSubstringSemCase(t *testing.T) {
	st := lojaAbre(t)
	const id = "bh-20240101140000-0002"
	corpus := []map[string]any{
		{"role": "user", "content": "preciso corrigir o Terminal_Run do windows"},
		{"role": "assistant", "content": "claro, vamos olhar o shell"},
		{"role": "tool_call", "tool_name": "terminal_run", "tool_id": "call_1", "args": `{"command":"echo oi"}`},
		{"role": "tool_result", "content": "exit_code: 0\noi"},
	}
	for _, p := range corpus {
		if _, err := st.StoreEvent(id, p["role"].(string), p); err != nil {
			t.Fatal(err)
		}
	}
	// Busca sem distinguir maiúscula, sobre o campo content.
	hits, err := st.Buscar(id, "TERMINAL_RUN", 10)
	if err != nil {
		t.Fatalf("Buscar: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("busca sem resultado")
	}
	if !strings.Contains(strings.ToLower(hits[0].Snippet), "terminal_run") {
		t.Fatalf("snippet sem o termo: %q", hits[0].Snippet)
	}
	// Busca sobre tool_name (campo humano indexado).
	hits, err = st.Buscar(id, "terminal_run", 10)
	if err != nil {
		t.Fatal(err)
	}
	var viuToolCall bool
	for _, h := range hits {
		if h.Kind == "tool_call" {
			viuToolCall = true
		}
	}
	if !viuToolCall {
		t.Fatalf("tool_name não foi indexado na busca: %+v", hits)
	}
	// Hit carrega seq, tempo e kind.
	if hits[0].Seq <= 0 || hits[0].Time == "" || hits[0].Kind == "" {
		t.Fatalf("Hit incompleto: %+v", hits[0])
	}
	// Termo ausente não é erro.
	vazio, err := st.Buscar(id, "zzz-nao-existe", 10)
	if err != nil || len(vazio) != 0 {
		t.Fatalf("busca sem casar devolveu %v, %v", vazio, err)
	}
	// Consulta vazia é erro do caller.
	if _, err := st.Buscar(id, "", 10); err == nil {
		t.Fatal("consulta vazia deveria falhar")
	}
}

func TestJanelaDeContexto(t *testing.T) {
	longo := strings.Repeat("a", 400) + "AGULHA" + strings.Repeat("b", 400)
	ini := strings.Index(longo, "AGULHA")
	trecho := janela(longo, ini, 6)
	if len(trecho) > 300 {
		t.Fatalf("janela não cortou: %d bytes", len(trecho))
	}
	if !strings.Contains(trecho, "AGULHA") {
		t.Fatalf("janela perdeu a agulha: %q", trecho)
	}
	if !strings.HasPrefix(trecho, "…") || !strings.HasSuffix(trecho, "…") {
		t.Fatalf("janela devia elipsar os dois lados: %q", trecho)
	}
	// Começo e fim da string não levam elipse.
	if j := janela("abc", 0, 3); j != "abc" {
		t.Fatalf("janela de string inteira = %q", j)
	}
}

func TestMarshalSortedDeterministico(t *testing.T) {
	// O mesmo mapa, em iterações diferentes, tem de gerar os MESMOS bytes —
	// é o que torna o log reproduzível.
	fazer := func() map[string]any {
		return map[string]any{
			"zebra":  1,
			"alpha":  "dois",
			"meio":   []any{3, 4},
			"cento":  map[string]any{"b": true, "a": nil},
			"vazio":  "",
			"numero": 7,
		}
	}
	primeiro, err := marshalSorted(fazer())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		outro, err := marshalSorted(fazer())
		if err != nil {
			t.Fatal(err)
		}
		if outro != primeiro {
			t.Fatalf("marshalSorted instável:\n%s\n%s", primeiro, outro)
		}
	}
	quero := `{"alpha":"dois","cento":{"a":null,"b":true},"meio":[3,4],"numero":7,"vazio":"","zebra":1}`
	if primeiro != quero {
		t.Fatalf("ordem/valor errados:\n quer %s\n veio %s", quero, primeiro)
	}
	if vazio, _ := marshalSorted(nil); vazio != "{}" {
		t.Fatalf("payload vazio = %q", vazio)
	}
	if vazio, _ := marshalSorted(map[string]any{}); vazio != "{}" {
		t.Fatalf("payload vazio (mapa) = %q", vazio)
	}
	// Valor não serializável devolve erro, não linha parcial.
	if _, err := marshalSorted(map[string]any{"ruim": func() {}}); err == nil {
		t.Fatal("payload com função deveria falhar")
	}
}

func TestStoreEventRejeitaEntradaRuim(t *testing.T) {
	st := lojaAbre(t)
	if _, err := st.StoreEvent("", "k", nil); err == nil {
		t.Fatal("sessionID vazio deveria falhar")
	}
	if _, err := st.StoreEvent("s", "", nil); err == nil {
		t.Fatal("kind vazio deveria falhar")
	}
	if _, err := st.StoreEvent("s", "k", map[string]any{"ruim": func() {}}); err == nil {
		t.Fatal("payload não serializável deveria falhar")
	}
	// Nada foi gravado com as entradas inválidas.
	eventos, err := st.ListEvents("s", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(eventos) != 0 {
		t.Fatalf("eventos gravados apesar do erro: %+v", eventos)
	}
}

func TestStoreEventCriaSessaoAutomaticamente(t *testing.T) {
	st := lojaAbre(t)
	if _, err := st.StoreEvent("nova", "user_message", map[string]any{"content": "x"}); err != nil {
		t.Fatalf("StoreEvent em sessão inexistente: %v", err)
	}
	if _, err := st.GetSession("nova"); err != nil {
		t.Fatalf("sessão não foi indexada: %v", err)
	}
}

func TestTouchSessionCriaSeNaoExiste(t *testing.T) {
	st := lojaAbre(t)
	if err := st.TouchSession("fantasma"); err != nil {
		t.Fatalf("TouchSession de inexistente deveria criar: %v", err)
	}
	if _, err := st.GetSession("fantasma"); err != nil {
		t.Fatalf("sessão fantasma não existe: %v", err)
	}
}

// PrimeiraFala é o nome provisório da sessão sem título no /resume.
func TestPrimeiraFala(t *testing.T) {
	st := lojaAbre(t)
	if got := st.PrimeiraFala("x"); got != "" {
		t.Fatalf("sessão inexistente = %q", got)
	}
	_, _ = st.StoreEvent("x", "user_message", map[string]any{"role": "user", "content": "revisar o parser"})
	_, _ = st.StoreEvent("x", "user_message", map[string]any{"role": "user", "content": "segunda"})
	if got := st.PrimeiraFala("x"); got != "revisar o parser" {
		t.Fatalf("primeira fala = %q", got)
	}
}
