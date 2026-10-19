// Package budget computes the context-window guardrail and a calibratable
// chars/token estimator. Nothing here decides the semantic moment — that is
// runtime/agent policy; budget only says whether the next request physically fits.
package budget

import "fmt"

const (
	// ReservaSaida: room the model needs available to respond (the
	// max_tokens clamp varies per provider).
	ReservaSaida = 16 << 10
	// ReservaOperacional: estimated format overhead (tool schemas, message
	// structure, provider-invisible tokens).
	ReservaOperacional = 8 << 10
)

// Window is the model's context window (tokens). 0 = unknown: with no
// published window the guardrail stays inactive and the policy only acts
// after a real provider error.
type Window struct {
	Tokens int
}

// Estimador estimates tokens ~ len/charsPerToken, calibratable per model from
// (bytes sent, reported prompt_tokens) pairs.
type Estimador struct {
	CharsPorToken float64
}

func (e Estimador) Tokens(text string) int {
	cpt := e.CharsPorToken
	if cpt <= 0 {
		cpt = 4 // conservative for Latin-script text
	}
	return int(float64(len(text)) / cpt)
}

// Calibrar takes a (bytes sent, actual tokens) pair and moves the factor
// toward the measured truth. Absurd samples are discarded.
func (e *Estimador) Calibrar(bytes int, actualTokens int) {
	if bytes <= 0 || actualTokens <= 0 {
		return
	}
	sample := float64(bytes) / float64(actualTokens)
	if sample < 1.5 || sample > 12 {
		return
	}
	if e.CharsPorToken <= 0 {
		e.CharsPorToken = sample
	} else {
		e.CharsPorToken = 0.7*e.CharsPorToken + 0.3*sample
	}
}

// Request is the forecast of the next physical request.
type Request struct {
	TokensEntrada int
	TokensSaida   int
	Janela        int // 0 = unknown
	Cabe          bool
}

func (r Request) Veredito() string {
	switch {
	case r.Janela == 0:
		return "guardrail inactive: model context window unknown"
	case r.Cabe:
		return fmt.Sprintf("fits: %d + %d reserved ≤ %d", r.TokensEntrada, r.TokensSaida, r.Janela)
	default:
		return fmt.Sprintf("overflows: %d + %d reserved > %d", r.TokensEntrada, r.TokensSaida, r.Janela)
	}
}

// Avaliar decides (by estimate) whether the prompt fits in the window.
func Avaliar(prompt string, window int, est Estimador) Request {
	e := est.Tokens(prompt)
	if e < ReservaOperacional {
		e = ReservaOperacional
	}
	r := Request{TokensEntrada: e, TokensSaida: ReservaSaida, Janela: window}
	if window == 0 {
		r.Cabe = true
		return r
	}
	r.Cabe = e+r.TokensSaida <= window
	return r
}
