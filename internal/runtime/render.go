package runtime

// Camada de saída do turno. O runtime não decide COR nem glifo: ele descreve o
// que aconteceu e quem escreve escolhe a roupa. Duas implementações:
//
//   - RendererPuro: texto cru, um caractere de escape sequer. É o que `-p` usa
//     (as receitas do site e do painel passam a saída por pipe e não podem
//     ganhar ANSI) e o que os testes usam — o prefixo "[tool <nome> ok" continua
//     LITERAL, porque runtime_test.go o procura como substring.
//   - a TUI do REPL (pacote cli): paleta 256, glifos UTF-8 e rodapé de tokens.

import (
	"fmt"
	"io"
	"time"
)

// Renderer é a saída de um turno: texto do modelo, eco de tool, avisos do
// guardrail e o rodapé de tokens.
type Renderer interface {
	// Resposta escreve a fala do modelo com a quebra final.
	Resposta(texto string)
	// Tool desenha o cabeçalho de uma tool executada (nome, duração, falha).
	Tool(nome string, dur time.Duration, erro bool)
	// Eco ecoa um trecho do resultado da tool (o histórico já recebeu tudo).
	Eco(saida string)
	// Guardrail relata uma compactação: proativa (previsão de estouro) ou
	// reativa (o provider recusou por contexto).
	Guardrail(proativa bool)
	// Tokens é o rodapé do turno: tamanho do request enviado, tokens gerados,
	// duração e se alguma compactação rodou. `compactou` sozinho já distingue
	// um turno normal de um que passou pelo guardrail.
	Tokens(prompt, saida int, dur time.Duration, compactou bool)
}

// RendererDetalhado é OPCIONAL: quem implementa recebe também o início de
// cada tool (com os argumentos, antes de executar) e o tamanho do contexto do
// último request. A TUI usa para mostrar "executando <tool>" e o % de contexto;
// ACP, sub-agente e modo puro não precisam e seguem só com Renderer.
type RendererDetalhado interface {
	// Pensando: o request ao modelo vai sair (a TUI anima a espera).
	Pensando()
	ToolInicio(nome, args string)
	Contexto(promptTokens int)
}

// RendererPuro é a implementação sem adorno.
type RendererPuro struct{ out io.Writer }

// NovoRendererPuro cria o renderer de texto cru sobre out.
func NovoRendererPuro(out io.Writer) *RendererPuro { return &RendererPuro{out: out} }

// RendererDe adapta um io.Writer à interface — compatibilidade com chamadores
// antigos e com o caminho `-p`, que só sabe escrever bytes.
func RendererDe(out io.Writer) Renderer { return NovoRendererPuro(out) }

func (p *RendererPuro) Resposta(texto string) { fmt.Fprintln(p.out, texto) }

func (p *RendererPuro) Tool(nome string, dur time.Duration, erro bool) {
	estado := "ok"
	if erro {
		estado = "error"
	}
	fmt.Fprintf(p.out, "[tool %s %s in %s]\n", nome, estado, dur.Round(time.Millisecond))
}

func (p *RendererPuro) Eco(saida string) {
	if saida == "" {
		return
	}
	fmt.Fprintln(p.out, saida)
}

func (p *RendererPuro) Guardrail(proativa bool) {
	if proativa {
		fmt.Fprintln(p.out, guardrailProativo)
		return
	}
	fmt.Fprintln(p.out, guardrailReativo)
}

// Tokens é deliberadamente silenciosa no modo puro: `-p` alimenta receitas via
// pipe e um rodapé de status poluiria cada uma. O contador por turno é coisa da
// UI interativa, onde alguém está olhando para a tela.
func (p *RendererPuro) Tokens(int, int, time.Duration, bool) {}

// Textos do guardrail. Ficam aqui porque o modo puro os imprime literal.
const (
	guardrailProativo = "[guardrail] proactive compaction (predicted overflow)"
	guardrailReativo  = "[guardrail] provider rejected the request size; reactive compaction"
)
