// Contexto vivo da sessão: o arquivo que o PRÓPRIO modelo edita para guardar o
// estado de trabalho entre turnos — objetivo, fatos confirmados, decisões,
// restrições, artefatos/IDs, progresso, pendências e perguntas abertas.
//
// Não é log. O log (internal/store) é append-only e registra o que aconteceu
// (user_message, assistant, tool_call...). O contexto vivo é o EXTRATO curado
// pelo modelo do que importa agora: um descreve o passado, o outro carrega o
// presente. Por isso ele tem arquivo separado e é substituível por inteiro —
// enquanto o log só cresce.
//
// O estado mora num mapa por ponteiro de Session (ctxPorSessao), e não em campo
// da struct: Session é editada por outros fios deste pacote, e o mapa mantém
// este arquivo ignorante daqueles. Um ponteiro de Session é estável durante a
// vida do processo, o que o torna chave segura.
package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/ruanbarroso/barroso-harness/internal/tools"
)

// Tetos do contexto vivo. A LEITURA é cortada em 48 KiB porque ela entra na
// janela do modelo a cada ctx_read: o corpo pode passar de 256 KiB no disco,
// mas uma única leitura não pode. O teto de GRAVAÇÃO é o que força a
// compactação — sem ele, um modelo que só anexa engorda o arquivo até ele virar
// ruído que não cabe mais em nenhuma leitura.
const (
	tetoLeituraCtxVivo = 48 << 10 // 48 KiB por ctx_read
	tetoCorpoCtxVivo   = 256 << 10
)

// Textos fixos. Ficam nomeados porque as tools e os testes os comparam ao pé da
// letra: mudar a String é mudar o que o modelo lê.
const (
	// ctxVivoVazio é o que ctx_read devolve antes da primeira escrita — o
	// convite à ação, não um erro.
	ctxVivoVazio = "(contexto vivo vazio — use ctx_write para começar)"

	// ctxVivoTeto é o erro de gravação: uma linha só, porque mensagem de tool
	// com newline no meio vira evento de log malformado.
	ctxVivoTeto = "contexto vivo excedeu 256 KiB; compacte com ctx_write"

	// marcadorCorteCtxVivo vai no fim do corpo quando a leitura foi cortada.
	marcadorCorteCtxVivo = "... (contexto vivo cortado em %d bytes; use ctx_write para reescrever compacto)"

	// prefixoVersaoCtxVivo abre a linha de cabeçalho do arquivo. É comentário
	// HTML: invisível em qualquer renderização de markdown, e removido na
	// leitura — o corpo em memória nunca o carrega.
	prefixoVersaoCtxVivo = "<!-- ctx-vivo:"
)

// contextoVivo é o corpo curado + o livro de versões. A versão não é um
// histórico de conteúdo (para isso existe o log): é a contagem de mutações
// confirmadas, que diz ao modelo se a gravação dele pegou e à UI se algo mudou.
type contextoVivo struct {
	corpo   string
	versoes []int
}

// versao devolve a versão atual (0 = nenhuma mutação ainda).
func (cx *contextoVivo) versao() int {
	if len(cx.versoes) == 0 {
		return 0
	}
	return cx.versoes[len(cx.versoes)-1]
}

// registrarVersao conta uma mutação. O livro guarda os números em ordem, então
// o último elemento é sempre a versão corrente.
func (cx *contextoVivo) registrarVersao() {
	cx.versoes = append(cx.versoes, len(cx.versoes)+1)
}

// aplicar é o único caminho de estado — leitura e mutação passam por aqui.
// Devolve o texto para o modelo e, quando impede a escrita, o erro.
//
// A máquina é deliberadamente burra: não sabe de JSON, de Session nem de disco.
// Quem traduz argumento de tool em chamada é a camada de spec.
func (cx *contextoVivo) aplicar(operacao, conteudo string) (string, error) {
	switch operacao {
	case "read":
		// Corpo só de espaço é vazio: um arquivo em branco não carrega estado.
		if strings.TrimSpace(cx.corpo) == "" {
			return ctxVivoVazio, nil
		}
		return cortarLeituraCtxVivo(cx.corpo), nil

	case "write":
		// write SUBSTITUI: o tamanho que importa é só o do conteúdo novo — o
		// corpo velho será descartado, então não conta contra o teto.
		if len(conteudo) > tetoCorpoCtxVivo {
			return "", errors.New(ctxVivoTeto)
		}
		cx.corpo = conteudo
		cx.registrarVersao()
		return fmt.Sprintf("ok: contexto vivo reescrito (versão %d, %d bytes)", cx.versao(), len(cx.corpo)), nil

	case "append":
		// Duas quebras de linha de folga entre o que havia e o novo trecho:
		// markdown cola parágrafo sem respiro e o modelo perde a fronteira.
		candidato := len(conteudo)
		if cx.corpo != "" {
			candidato = len(cx.corpo) + 2 + len(conteudo)
		}
		if candidato > tetoCorpoCtxVivo {
			return "", errors.New(ctxVivoTeto)
		}
		if cx.corpo == "" {
			cx.corpo = conteudo
		} else {
			cx.corpo += "\n\n" + conteudo
		}
		cx.registrarVersao()
		return fmt.Sprintf("ok: anexado ao contexto vivo (versão %d)", cx.versao()), nil

	default:
		// "edit" não existe neste slice de propósito: ele substituiria a
		// disciplina de reescrever compacto por remendos incrementais, que é
		// como o arquivo apodrece.
		return "", fmt.Errorf("operação %q não existe no contexto vivo (use read, write ou append)", operacao)
	}
}

// cortarLeituraCtxVivo aplica o teto da leitura. Corpo dentro do teto volta
// inteiro, sem marcador — marcador em leitura pequena é ruído.
func cortarLeituraCtxVivo(s string) string {
	if len(s) <= tetoLeituraCtxVivo {
		return s
	}
	return s[:tetoLeituraCtxVivo] + "\n\n" + fmt.Sprintf(marcadorCorteCtxVivo, tetoLeituraCtxVivo)
}

// ── estado por sessão ──────────────────────────────────────────────────────

// ctxPorSessao guarda um *contextoVivo por ponteiro de Session. sync.Map porque
// tool calls de um mesmo turno rodam em sequência, mas nada impede um integrador
// de usar a sessão de fios diferentes. O valor nunca é removido: a sessão morre
// com o processo, e o que precisa sobreviver está no arquivo.
var ctxPorSessao sync.Map

// ctxDe devolve o contexto vivo da sessão, criando vazio na primeira vez.
// Session nil devolve um estado descartável: uma spec órfã continua respondendo
// em vez de derrubar o turno.
func ctxDe(s *Session) *contextoVivo {
	if s == nil {
		return &contextoVivo{}
	}
	if v, ok := ctxPorSessao.Load(s); ok {
		return v.(*contextoVivo)
	}
	cx := &contextoVivo{}
	v, _ := ctxPorSessao.LoadOrStore(s, cx)
	return v.(*contextoVivo)
}

// ctxVivoSemSessao é o estado usado quando a tool não sabe de qual sessão vem
// (modo -p, testes, e o EspecificacoesContextoVivo sem sessão). Sessão
// persistente tem o seu em ctxPorSessao — o global existe para o caminho sem
// Session, e a sessão é sempre o estado preferido.
var ctxVivoSemSessao = &contextoVivo{}

// ── persistência ───────────────────────────────────────────────────────────

// homeDe resolve o diretório de dados. É um var, e não uma função simples, para
// os testes injetarem um diretório temporário sem depender do pacote config —
// que é editado por outros fios deste pacote.
var homeDe = func() string {
	if v := os.Getenv("BARROSO_HARNESS_HOME"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home, _ = os.Getwd()
	}
	return filepath.Join(home, ".barroso-harness")
}

// caminhoCtxVivo monta <dir>/<sessionID>.ctx-vivo.md.
func caminhoCtxVivo(dir, sessionID string) string {
	return filepath.Join(dir, sessionID+".ctx-vivo.md")
}

// salvar grava atomicamente: escreve num temporário DO MESMO diretório e
// renomeia por cima do destino. A renomeação dentro do mesmo filesystem é
// atômica — um crash no meio deixa o arquivo anterior intacto em vez de um
// meio-escrito que o próximo Resume leria como contexto legítimo.
func (cx *contextoVivo) salvar(dir, sessionID string) error {
	if strings.TrimSpace(sessionID) == "" {
		return fmt.Errorf("sessionID vazio")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	conteudo := fmt.Sprintf("%s%d -->\n%s", prefixoVersaoCtxVivo, cx.versao(), cx.corpo)
	destino := caminhoCtxVivo(dir, sessionID)
	tmp, err := os.CreateTemp(dir, ".ctx-vivo-*.tmp")
	if err != nil {
		return err
	}
	nome := tmp.Name()
	// O temporário já nasce 0600 (CreateTemp) e a renomeação preserva o modo:
	// estado de sessão não é arquivo de leitura livre.
	if _, err := tmp.WriteString(conteudo); err != nil {
		tmp.Close()
		os.Remove(nome)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(nome)
		return err
	}
	if err := os.Rename(nome, destino); err != nil {
		os.Remove(nome)
		return err
	}
	return nil
}

// separarVersaoCtxVivo tira o cabeçalho de versão do arquivo. Cabeçalho
// malformado (ou corpo do modelo que casualmente começa com o prefixo) é
// devolvido INTEIRO: perder uma linha do contexto é pior que recomeçar a
// contagem de versões do zero.
func separarVersaoCtxVivo(b []byte) (string, int) {
	s := string(b)
	if !strings.HasPrefix(s, prefixoVersaoCtxVivo) {
		return s, 0
	}
	fim := strings.IndexByte(s, '\n')
	linha := s
	if fim >= 0 {
		linha = s[:fim]
	}
	if !strings.HasSuffix(linha, "-->") {
		return s, 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(linha, prefixoVersaoCtxVivo), "-->")))
	if err != nil || n < 0 {
		return s, 0
	}
	if fim < 0 {
		return "", n
	}
	return s[fim+1:], n
}

// carregarContextoVivo lê o arquivo da sessão. Arquivo AUSENTE não é erro: é
// sessão que ainda não gravou nada, e o estado vazio é a resposta certa.
func carregarContextoVivo(dir, sessionID string) (*contextoVivo, error) {
	b, err := os.ReadFile(caminhoCtxVivo(dir, sessionID))
	if err != nil {
		if os.IsNotExist(err) {
			return &contextoVivo{}, nil
		}
		return nil, err
	}
	corpo, versoes := separarVersaoCtxVivo(b)
	cx := &contextoVivo{corpo: corpo}
	// O livro de versões é refeito a partir do cabeçalho: sem isso, o primeiro
	// ctx_write depois de um Resume anunciaria "versão 1" para uma terceira
	// edição do arquivo.
	for i := 1; i <= versoes; i++ {
		cx.registrarVersao()
	}
	return cx, nil
}

// ContextoVivoSalvar grava o contexto vivo desta sessão em
// <home>/<sessionID>.ctx-vivo.md. Silencioso sem persistência: sessão em
// memória (modo -p) não tem onde guardar, e isso não pode derrubar um turno.
func (s *Session) ContextoVivoSalvar() error {
	if !s.Persistente() {
		return nil
	}
	return ctxDe(s).salvar(homeDe(), s.sessionID)
}

// ContextoVivoCarregar traz o contexto vivo desta sessão do disco para a
// memória. Sessão sem persistência não tem o que ler e segue vazia.
func (s *Session) ContextoVivoCarregar() error {
	if !s.Persistente() {
		return nil
	}
	cx, err := carregarContextoVivo(homeDe(), s.sessionID)
	if err != nil {
		return err
	}
	ctxPorSessao.Store(s, cx)
	return nil
}

// ── specs ──────────────────────────────────────────────────────────────────

// EspecificacoesContextoVivo devolve as TRÊS tools do contexto vivo, ligadas ao
// estado sem sessão. É o catálogo para quem não tem uma Session na mão (modo
// -p, testes); integrador com sessão deve preferir
// EspecificacoesContextoVivoSessao, que liga as mesmas specs ao estado daquela
// sessão.
//
// ATENÇÃO PARA QUEM INTEGRA: o catálogo estático de tools (SpecsBuiltins) ainda
// traz um ctx_read placeholder, que lê o BARROSO.md. As listas têm de ser
// mescladas SEM duplicar nomes — dois ctx_read no schema é chamada ambígua para
// o provider.
func EspecificacoesContextoVivo() []tools.Spec {
	return especificacoesContextoVivo(ctxVivoSemSessao)
}

// EspecificacoesContextoVivoSessao devolve as mesmas três tools ligadas ao
// contexto vivo da sessão dada — é a forma usada pelo REPL, onde o estado
// precisa sobreviver ao turno e ser salvo/carregado por sessionID.
func EspecificacoesContextoVivoSessao(s *Session) []tools.Spec {
	return especificacoesContextoVivo(ctxDe(s))
}

func especificacoesContextoVivo(cx *contextoVivo) []tools.Spec {
	return []tools.Spec{
		specCtxReadVivo(cx),
		specCtxWriteVivo(cx),
		specCtxAppendVivo(cx),
	}
}

// specCtxReadVivo lê o corpo atual, cortado em 48 KiB. Não recebe argumento: o
// estado já é conhecido pela spec.
func specCtxReadVivo(cx *contextoVivo) tools.Spec {
	return tools.Spec{
		Name: "ctx_read",
		Description: "Lê o contexto vivo da sessão: o arquivo que VOCÊ mantém entre turnos com o " +
			"estado do trabalho (objetivo, fatos confirmados, decisões, restrições, artefatos, " +
			"progresso, pendências). Leia no começo de uma tarefa longa para recuperar o fio, e " +
			"depois de escrever para conferir o que ficou. Devolve até 48 KiB.",
		Schema: map[string]any{"type": "object", "properties": map[string]any{}},
		Exec: func(_ context.Context, _ string, _ json.RawMessage) (string, error) {
			return cx.aplicar("read", "")
		},
	}
}

// specCtxWriteVivo SUBSTITUI o corpo inteiro. A description carrega a receita
// do que guardar e o aviso de proveniência: como o arquivo é substituível,
// evento que só existiu no log precisa ter o seq/caminho anotado AQUI para ser
// reencontrado depois por session_search/session_retrieve.
func specCtxWriteVivo(cx *contextoVivo) tools.Spec {
	return tools.Spec{
		Name: "ctx_write",
		Description: "REESCREVE INTEIRO o contexto vivo da sessão (o arquivo de estado do trabalho " +
			"que você mantém entre turnos). O conteúdo novo substitui o anterior por completo — não " +
			"anexa. Guarde: objetivo e critérios de conclusão; fatos confirmados COM referências " +
			"(caminho de arquivo, seq do evento no log); decisões e suas justificativas; restrições " +
			"descobertas; artefatos e IDs relevantes; progresso; pendências e perguntas abertas. " +
			"As referências importam porque o log é append-only e resultados grandes podem sair da " +
			"janela: anotar o seq/caminho permite reencontrar tudo com session_search e " +
			"session_retrieve. Reescreva também para COMPACTAR — tirar o que já não importa — sempre " +
			"que o arquivo engordar.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"content": map[string]any{"type": "string", "description": "o contexto vivo inteiro, em markdown"},
			},
			"required": []string{"content"},
		},
		Exec: func(_ context.Context, _ string, raw json.RawMessage) (string, error) {
			conteudo, err := conteudoCtxVivo(raw)
			if err != nil {
				return "", err
			}
			return cx.aplicar("write", conteudo)
		},
	}
}

// specCtxAppendVivo anexa ao fim. É o atalho para registrar um fato sem
// reescrever tudo — e a via que engorda o arquivo, por isso a description manda
// compactar com ctx_write quando pesar.
func specCtxAppendVivo(cx *contextoVivo) tools.Spec {
	return tools.Spec{
		Name: "ctx_append",
		Description: "ANEXA um trecho ao fim do contexto vivo da sessão, preservando o que já estava. " +
			"Use para registrar um fato novo, uma decisão tomada ou uma pendência sem reescrever o " +
			"arquivo inteiro. Se o arquivo já estiver grande, prefira ctx_write: anexar sem compactar " +
			"é o que faz o contexto vivo deixar de caber na leitura. Inclua referências (caminho/seq) " +
			"quando o fato vier de algo que pode sair da janela depois.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"content": map[string]any{"type": "string", "description": "o trecho a anexar, em markdown"},
			},
			"required": []string{"content"},
		},
		Exec: func(_ context.Context, _ string, raw json.RawMessage) (string, error) {
			conteudo, err := conteudoCtxVivo(raw)
			if err != nil {
				return "", err
			}
			return cx.aplicar("append", conteudo)
		},
	}
}

// conteudoCtxVivo traduz o JSON da chamada em conteúdo. Mora na camada de spec,
// e não em aplicar, porque JSON malformado é problema de tool — a máquina de
// estado não precisa saber disso.
func conteudoCtxVivo(raw json.RawMessage) (string, error) {
	var c struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return "", fmt.Errorf("entrada inválida: %w", err)
	}
	if strings.TrimSpace(c.Content) == "" {
		return "", fmt.Errorf("content vazio: escreva o estado do trabalho (ou ctx_read para ver o atual)")
	}
	return c.Content, nil
}
