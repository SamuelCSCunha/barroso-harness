// Package budget calcula o guardrail da janela e estimadorchars/token
// calibrável. Nada aqui decide o momento semântico — isso é política do
// runtime/agente; o budget só diz se o próximo request cabe fisicamente.
package budget

import "fmt"

const (
	// ReservaSaida: espaço que o modelo precisa ter disponível para
	// responder (o clamp de max_tokens varia por provider).
	ReservaSaida = 16 << 10
	// ReservaOperacional: overhead estimado do formato (tool schemas,
	// estrutura das mensagens, tokens invisíveis do provider).
	ReservaOperacional = 8 << 10
)

// Window refere-se à janela do modelo (tokens). 0 = desconhecida: sem janela
// publicada, o guardrail fica inativo e a política age apenas depois de
// erro real do provider.
type Window struct {
	Tokens int
}

// Estimador estimate tokens ~ len/charsPorToken, calibrável por modelo a
// partir dos pares (bytes enviados, prompt_tokens reportados).
type Estimador struct {
	CharsPorToken float64
}

func (e Estimador) Tokens(texto string) int {
	cpt := e.CharsPorToken
	if cpt <= 0 {
		cpt = 4 // conservador para texto latino
	}
	return int(float64(len(texto)) / cpt)
}

// Calibrar recebe o segundo par (bytes enviados, tokens reais) e move o
// fator em direção à verdade medida. Amostras absurdas são descartadas.
func (e *Estimador) Calibrar(bytes int, tokensReais int) {
	if bytes <= 0 || tokensReais <= 0 {
		return
	}
	novo := float64(bytes) / float64(tokensReais)
	if novo < 1.5 || novo > 12 {
		return
	}
	if e.CharsPorToken <= 0 {
		e.CharsPorToken = novo
	} else {
		e.CharsPorToken = 0.7*e.CharsPorToken + 0.3*novo
	}
}

// Request é a previsão do próximo request físico.
type Request struct {
	TokensEntrada int
	TokensSaida   int
	Janela        int // 0 = desconhecida
	Cabe          bool
}

func (r Request) Veredito() string {
	switch {
	case r.Janela == 0:
		return "guardrail inativo: janela do modelo desconhecida"
	case r.Cabe:
		return fmt.Sprintf("cabe: %d + %d reservado ≤ %d", r.TokensEntrada, r.TokensSaida, r.Janela)
	default:
		return fmt.Sprintf("estoura: %d + %d reservado > %d", r.TokensEntrada, r.TokensSaida, r.Janela)
	}
}

// Avaliar decide (na estimativa) se o prompt cabe na janela.
func Avaliar(prompt string, janela int, est Estimador) Request {
	e := est.Tokens(prompt)
	if e < ReservaOperacional {
		e = ReservaOperacional
	}
	r := Request{TokensEntrada: e, TokensSaida: ReservaSaida, Janela: janela}
	if janela == 0 {
		r.Cabe = true
		return r
	}
	r.Cabe = e+r.TokensSaida <= janela
	return r
}
