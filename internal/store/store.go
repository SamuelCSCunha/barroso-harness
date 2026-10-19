// Package store é o log append-only das sessões: SQLite puro-Go
// (modernc.org/sqlite, sem CGO) com as mensagens do transcript gravadas como
// eventos. O log é a fonte de verdade — a sessão em memória é reconstruível a
// partir dele (Resume), e a busca/recuperação de conteúdo arquivado ou
// compactado (Search/Retrieve) passa por aqui.
//
// Regra de serialização: todo payload é JSON produzido por marshalSorted, com
// chaves ordenadas. Mapas em Go têm ordem de iteração aleatória; sem ordenar,
// o mesmo payload geraria bytes diferentes a cada gravação e o log deixaria de
// ser reprodutível (hash de blob, diffs e testes dependem disso).
package store

import (
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	// Driver puro-Go (sem CGO): registrar o nome "sqlite" no database/sql.
	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var sqlSchema string

// NomeDriver é o driver registrado por modernc.org/sqlite. Exportado para que
// quem integra o store possa reconhecer a origem de um erro.
const NomeDriver = "sqlite"

// ErrSessaoInexistente é devolvido quando um id não existe em sessions.
var ErrSessaoInexistente = errors.New("sessão inexistente")

// TsFormato é o formato dos timestamps no log: RFC3339 com precisão de
// milissegundo, sempre em UTC. Fixo para que a ordenação lexicográfica de
// updated_at (ListSessions) coincida com a ordenação cronológica.
const TsFormato = "2006-01-02T15:04:05.000Z07:00"

func tsAgora() string { return time.Now().UTC().Format(TsFormato) }

// Event é uma linha do log, já materializada.
type Event struct {
	Seq     int    `json:"seq"`
	Session string `json:"session_id"`
	Ts      string `json:"ts"`
	Kind    string `json:"kind"`
	Payload string `json:"payload_json"`
}

// Session é a linha de índice de uma conversa.
type Session struct {
	ID        string `json:"id"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	Title     string `json:"title"`
}

// Hit é um resultado de busca: o evento inteiro não volta, só o trecho que
// casa com a consulta e a janela de contexto em volta dele.
type Hit struct {
	Seq     int    `json:"seq"`
	Time    string `json:"ts"`
	Kind    string `json:"kind"`
	Snippet string `json:"snippet"`
}

// Store é o acesso ao banco. Um *sql.DB do database/sql já é seguro para uso
// concorrente; o mutex protege apenas o contador de escrita, porque rodadas de
// tool executam em paralelo no runtime.
type Store struct {
	caminho string
	db      *sql.DB
	mu      sync.Mutex
}

// Open abre (ou cria) o banco em path, aplica o schema e liga o modo WAL.
//
// WAL é o que permite ao REPL escrever no log enquanto outra conexão lê sem
// travar; sem ele, cada gravação de evento competiria com a leitura de
// /resume. `busy_timeout` dá folga extra ao escritor antes de desistir.
//
// Mesmo caminho duas vezes devolve o MESMO *Store (cache por caminho), para
// que two sessions do mesmo processo compartilhem uma conexão em vez de
// brigarem pelo lock do SQLite.
var (
	registro   = map[string]*Store{}
	registroMu sync.Mutex
)

// Open abre o banco em path. Erro de abertura é do caller decidir: na CLI a
// persistência é opcional (o REPL segue sem ela), então quem chama trata a
// falha como aviso e não como parada.
func Open(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("store: caminho vazio")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	registroMu.Lock()
	defer registroMu.Unlock()
	if s, ok := registro[abs]; ok {
		return s, nil
	}
	dsn := "file:" + filepath.ToSlash(abs) + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open(NomeDriver, dsn)
	if err != nil {
		return nil, fmt.Errorf("store: abrir %s: %w", abs, err)
	}
	// Uma conexão só: o log é sequencial por natureza e escrita concorrente em
	// WAL ainda serializa no lock do arquivo. Duas conexões berma êxito no
	// cache acima de qualquer forma.
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: ping %s: %w", abs, err)
	}
	st := &Store{caminho: abs, db: db}
	if err := st.schema(); err != nil {
		_ = db.Close()
		return nil, err
	}
	registro[abs] = st
	return st, nil
}

// Fechar libera a conexão e remove o store do cache por caminho.
func (s *Store) Fechar() error {
	if s == nil {
		return nil
	}
	registroMu.Lock()
	defer registroMu.Unlock()
	if cur, ok := registro[s.caminho]; ok && cur == s {
		delete(registro, s.caminho)
	}
	return s.db.Close()
}

// Caminho devolve o caminho absoluto do banco.
func (s *Store) Caminho() string { return s.caminho }

func (s *Store) schema() error {
	if _, err := s.db.Exec(sqlSchema); err != nil {
		return fmt.Errorf("store: schema: %w", err)
	}
	return nil
}

// ── serialização determinística ───────────────────────────────────────────

// marshalSorted serializa com chaves em ordem lexicográfica, estável entre
// execuções. Números podem variar de representação (int vs float64) e por isso
// payloads que carregam contagens usam int em vez de aceitar qualquer número.
func marshalSorted(payload map[string]any) (string, error) {
	if len(payload) == 0 {
		return "{}", nil
	}
	chaves := make([]string, 0, len(payload))
	for k := range payload {
		chaves = append(chaves, k)
	}
	sort.Strings(chaves)
	var sb strings.Builder
	sb.WriteByte('{')
	for i, k := range chaves {
		if i > 0 {
			sb.WriteByte(',')
		}
		kb, err := json.Marshal(k)
		if err != nil {
			return "", err
		}
		sb.Write(kb)
		sb.WriteByte(':')
		vb, err := json.Marshal(payload[k])
		if err != nil {
			return "", err
		}
		sb.Write(vb)
	}
	sb.WriteByte('}')
	return sb.String(), nil
}

// ── sessões ───────────────────────────────────────────────────────────────

// NewSession cria a linha de índice da sessão (INSERT OR IGNORE: chamar duas
// vezes com o mesmo id não zera created_at nem apaga eventos).
func (s *Store) NewSession(id string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("store: id de sessão vazio")
	}
	agora := tsAgora()
	_, err := s.db.Exec(
		`INSERT INTO sessions (id, created_at, updated_at, title, metadata_json)
		 VALUES (?, ?, ?, '', '{}')
		 ON CONFLICT(id) DO NOTHING`,
		id, agora, agora,
	)
	if err != nil {
		return fmt.Errorf("store: nova sessão %s: %w", id, err)
	}
	return nil
}

// TouchSession renova updated_at — é o que ordena ListSessions e o que faz
// /resume mostrar as conversas na ordem em que foram usadas. Sessão ainda não
// indexada fica como está: quem cria a linha é o primeiro evento (ou o
// título), para abrir o bh sem conversar não deixar sessão vazia no banco.
func (s *Store) TouchSession(id string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("store: id de sessão vazio")
	}
	if _, err := s.db.Exec(`UPDATE sessions SET updated_at = ? WHERE id = ?`, tsAgora(), id); err != nil {
		return fmt.Errorf("store: toque %s: %w", id, err)
	}
	return nil
}

// SetTitle grava o título da sessão (o que /save escreve e /resume mostra).
func (s *Store) SetTitle(id, titulo string) error {
	if err := s.NewSession(id); err != nil {
		return err
	}
	if err := s.TouchSession(id); err != nil {
		return err
	}
	if _, err := s.db.Exec(`UPDATE sessions SET title = ? WHERE id = ?`, titulo, id); err != nil {
		return fmt.Errorf("store: título %s: %w", id, err)
	}
	return nil
}

// GetSession devolve a sessão; segunda volta distingue "não existe" de erro.
func (s *Store) GetSession(id string) (*Session, error) {
	row := s.db.QueryRow(`SELECT id, created_at, updated_at, title FROM sessions WHERE id = ?`, id)
	var se Session
	err := row.Scan(&se.ID, &se.CreatedAt, &se.UpdatedAt, &se.Title)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSessaoInexistente
	}
	if err != nil {
		return nil, fmt.Errorf("store: sessão %s: %w", id, err)
	}
	return &se, nil
}

// ListSessions devolve as sessões mais recentes primeiro (updated_at DESC).
// ContaEventos entra no resultado porque /resume precisa mostrar quantas
// linhas de log cada conversa tem.
func (s *Store) ListSessions(limit int) ([]Session, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(
		`SELECT s.id, s.created_at, s.updated_at, s.title,
		        (SELECT COUNT(*) FROM events e WHERE e.session_id = s.id) AS n
		   FROM sessions s
		  ORDER BY s.updated_at DESC, s.id ASC
		  LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: listar sessões: %w", err)
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var se Session
		var n int
		if err := rows.Scan(&se.ID, &se.CreatedAt, &se.UpdatedAt, &se.Title, &n); err != nil {
			return nil, fmt.Errorf("store: ler sessão: %w", err)
		}
		out = append(out, se)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterar sessões: %w", err)
	}
	return out, nil
}

// PrimeiraFala devolve o texto da primeira mensagem do usuário na sessão
// ("" se não houver) — o nome provisório de uma sessão sem título.
func (s *Store) PrimeiraFala(id string) string {
	var corpo string
	err := s.db.QueryRow(
		`SELECT payload_json FROM events
		  WHERE session_id = ? AND kind = 'user_message'
		    AND payload_json NOT LIKE '%[estado de trabalho%'
		    AND payload_json NOT LIKE '%[output limit]%'
		    AND payload_json NOT LIKE '%[subagents]%'
		  ORDER BY seq LIMIT 1`, id).Scan(&corpo)
	if err != nil {
		return ""
	}
	var m struct {
		Content string `json:"content"`
	}
	if json.Unmarshal([]byte(corpo), &m) != nil {
		return ""
	}
	return m.Content
}

// ContarEventos devolve quantos eventos a sessão tem (0 se não existe).
func (s *Store) ContarEventos(id string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM events WHERE session_id = ?`, id).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: contar eventos %s: %w", id, err)
	}
	return n, nil
}

// ── eventos ───────────────────────────────────────────────────────────────

// StoreEvent anexa um evento ao log. O payload é serializado com chaves
// ordenadas; valores não serializáveis (canal, função) devolvem erro e NADA é
// gravado — o log não aceita linha parcial.
//
// O lock é do processo inteiro porque seq é atribuído pelo SQLite: duas
// goroutines gravando ao mesmo tempo (rodadas de tool em paralelo) precisam
// sair em ordem determinística, e o mutex garante que a ordem de chamada é a
// ordem do log.
func (s *Store) StoreEvent(sessionID, kind string, payload map[string]any) (int, error) {
	if strings.TrimSpace(sessionID) == "" {
		return 0, fmt.Errorf("store: sessionID vazio")
	}
	if strings.TrimSpace(kind) == "" {
		return 0, fmt.Errorf("store: kind vazio")
	}
	corpo, err := marshalSorted(payload)
	if err != nil {
		return 0, fmt.Errorf("store: payload de %s: %w", kind, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.NewSession(sessionID); err != nil {
		return 0, err
	}
	res, err := s.db.Exec(
		`INSERT INTO events (session_id, ts, kind, payload_json) VALUES (?, ?, ?, ?)`,
		sessionID, tsAgora(), kind, corpo)
	if err != nil {
		return 0, fmt.Errorf("store: gravar %s em %s: %w", kind, sessionID, err)
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: seq de %s: %w", kind, err)
	}
	if _, err := s.db.Exec(`UPDATE sessions SET updated_at = ? WHERE id = ?`, tsAgora(), sessionID); err != nil {
		return 0, fmt.Errorf("store: toque após %s: %w", kind, err)
	}
	return int(seq), nil
}

// ListEvents devolve os eventos da sessão a partir de fromSeq (inclusive), no
// máximo limit. limit <= 0 significa "todos". Ordem: seq crescente.
func (s *Store) ListEvents(sessionID string, fromSeq, limit int) ([]Event, error) {
	q := `SELECT seq, session_id, ts, kind, payload_json FROM events WHERE session_id = ? AND seq >= ? ORDER BY seq ASC`
	args := []any{sessionID, fromSeq}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: eventos de %s: %w", sessionID, err)
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.Seq, &e.Session, &e.Ts, &e.Kind, &e.Payload); err != nil {
			return nil, fmt.Errorf("store: ler evento: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterar eventos: %w", err)
	}
	return out, nil
}

// GetEvent devolve UM evento pelo seq (retrieve de conteúdo completo).
func (s *Store) GetEvent(sessionID string, seq int) (*Event, error) {
	row := s.db.QueryRow(
		`SELECT seq, session_id, ts, kind, payload_json FROM events WHERE session_id = ? AND seq = ?`,
		sessionID, seq)
	var e Event
	err := row.Scan(&e.Seq, &e.Session, &e.Ts, &e.Kind, &e.Payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("store: evento %d não existe em %s: %w", seq, sessionID, ErrSessaoInexistente)
	}
	if err != nil {
		return nil, fmt.Errorf("store: evento %d: %w", seq, err)
	}
	return &e, nil
}

// ── blobs ─────────────────────────────────────────────────────────────────

// PutBlob guarda o conteúdo e devolve a referência pronta para entrar no
// payload do evento. Hash é sha256 do conteúdo: o mesmo bytes sempre gera a
// mesma referência, então gravar duas vezes não duplica linha (INSERT OR
// IGNORE) e o log continua reprodutível.
func (s *Store) PutBlob(dados []byte) (map[string]any, error) {
	if len(dados) == 0 {
		return nil, fmt.Errorf("store: blob vazio")
	}
	soma := sha256Hex(dados)
	if _, err := s.db.Exec(
		`INSERT OR IGNORE INTO blobs (hash, size, data) VALUES (?, ?, ?)`,
		soma, len(dados), dados); err != nil {
		return nil, fmt.Errorf("store: gravar blob: %w", err)
	}
	return map[string]any{"blob": soma, "size": len(dados)}, nil
}

// GetBlob devolve o conteúdo de um hash.
func (s *Store) GetBlob(hash string) ([]byte, error) {
	var dados []byte
	err := s.db.QueryRow(`SELECT data FROM blobs WHERE hash = ?`, hash).Scan(&dados)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("store: blob %s não existe", hash)
	}
	if err != nil {
		return nil, fmt.Errorf("store: blob %s: %w", hash, err)
	}
	return dados, nil
}

// sha256Hex em hexadecimal minúsculo (o formato que a referência carrega).
func sha256Hex(b []byte) string {
	soma := sha256.Sum256(b)
	return hex.EncodeToString(soma[:])
}

// ── busca ─────────────────────────────────────────────────────────────────

// Buscar varre os eventos da sessão por substring (sem distinguir maiúscula)
// nos campos humanos do payload — role, content, tool name — e devolve um
// trecho com janela de contexto em volta da primeira ocorrência.
//
// A varredura é no banco porque o log é a fonte de verdade; e é limitada a
// `limit` eventos porque ninguém procura no log de 10k linhas esperando tudo.
func (s *Store) Buscar(sessionID, query string, limit int) ([]Hit, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("store: consulta vazia")
	}
	if limit <= 0 {
		limit = 20
	}
	eventos, err := s.ListEvents(sessionID, 0, 0)
	if err != nil {
		return nil, err
	}
	agulha := strings.ToLower(query)
	var hits []Hit
	for _, e := range eventos {
		campos, err := camposTextuais(e.Payload)
		if err != nil {
			continue // payload corrompido/ilegível não pode parar a busca
		}
		for _, c := range campos {
			i := strings.Index(strings.ToLower(c), agulha)
			if i < 0 {
				continue
			}
			hits = append(hits, Hit{
				Seq:     e.Seq,
				Time:    e.Ts,
				Kind:    e.Kind,
				Snippet: janela(c, i, len(agulha)),
			})
			break // um hit por evento: o evento já está no resultado
		}
		if limit > 0 && len(hits) >= limit {
			break
		}
	}
	return hits, nil
}

// camposTextuais extrai do payload só o que um humano procura: role, content,
// tool name/id e título do evento. Ignora os campos de bookkeeping (ts, kind,
// usage, blob...) porque eles não são o conteúdo da conversa.
func camposTextuais(payloadJSON string) ([]string, error) {
	var p map[string]any
	if err := json.Unmarshal([]byte(payloadJSON), &p); err != nil {
		return nil, err
	}
	var out []string
	for _, k := range []string{"role", "content", "tool_name", "title", "text", "preview"} {
		if s, ok := p[k].(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out, nil
}

// janela devolve o trecho em volta da ocorrência, com elipses quando corta.
func janela(s string, ini, tamAgulha int) string {
	const roda = 120
	de := ini - roda
	if de < 0 {
		de = 0
	}
	ate := ini + tamAgulha + roda
	if ate > len(s) {
		ate = len(s)
	}
	trecho := s[de:ate]
	if de > 0 {
		trecho = "…" + trecho
	}
	if ate < len(s) {
		trecho += "…"
	}
	return strings.Join(strings.Fields(trecho), " ")
}

// Rehydrate devolve o conteúdo completo de um payload que referencia blob.
//
// O preview NÃO é concatenado: ele é sempre os primeiros bytes do próprio
// blob, então somá-los devolveria o começo duplicado. O preview existe para
// quem lê o LOG (busca, inspeção) sem precisar ir ao blob; quem reidrata quer
// o conteúdo inteiro, exatamente como era.
//
// Payloads sem blob voltam intactos — é o caminho comum, a maioria dos
// eventos cabe no log.
func (s *Store) Rehydrate(payloadJSON string) (string, error) {
	var p map[string]any
	if err := json.Unmarshal([]byte(payloadJSON), &p); err != nil {
		return payloadJSON, nil // não é JSON de payload: devolve cru
	}
	hash, ok := p["blob"].(string)
	if !ok || hash == "" {
		return payloadJSON, nil
	}
	dados, err := s.GetBlob(hash)
	if err != nil {
		// Blob ausente (banco copiado sem os dados?): o preview é o que sobrou.
		return payloadJSON, nil
	}
	return string(dados), nil
}
