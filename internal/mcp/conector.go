package mcp

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// ErroServidor é a falha de UM servidor MCP. ConectarTodos nunca derruba o
// conjunto por causa de um servidor: os que falharam voltam aqui, com nome e
// erro, e quem integra decide (seguir sem eles, avisar, ou abortar).
type ErroServidor struct {
	Nome string
	Err  error
}

// Error formata em UMA linha, no formato "nome: erro".
func (e ErroServidor) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("servidor MCP %q: falha sem detalhe", e.Nome)
	}
	return fmt.Sprintf("servidor MCP %q: %v", e.Nome, umaLinha(e.Err.Error()))
}

// Unwrap permite errors.Is / errors.As alcançarem o erro original.
func (e ErroServidor) Unwrap() error { return e.Err }

// ConectarTodos conecta cada servidor de forma independente: todos em
// paralelo, cada um com seu próprio contexto derivado.
//
// Um servidor que falha NÃO invalida os demais — volta como item do slice de
// erros (nunca como erro da função). A função só devolve erro não-nil quando
// ctx já chega cancelado/expirado, e aí devolve nil nos clientes.
//
// Clientes devolvidos já estão abertos E com handshake feito (Abre chama o
// initialize); o chamador só precisa chamar Ferramentas.
func ConectarTodos(ctx context.Context, servs []Servidor) ([]*Cliente, []ErroServidor) {
	if ctx == nil {
		ctx = context.Background()
	}
	if len(servs) == 0 {
		return nil, nil
	}

	type saida struct {
		c   *Cliente
		err ErroServidor
		ok  bool
	}

	canal := make(chan saida, len(servs))
	var wg sync.WaitGroup
	for _, s := range servs {
		wg.Add(1)
		go func(s Servidor) {
			defer wg.Done()
			c, err := Novo(s)
			if err != nil {
				canal <- saida{err: ErroServidor{Nome: s.Nome, Err: err}}
				return
			}
			if err := c.Abre(ctx); err != nil {
				// Abrir falhou: garante que o subprocesso (stdiio) não fique
				// zumbi e que a conexão seja liberada.
				_ = c.Fecha()
				canal <- saida{err: ErroServidor{Nome: s.Nome, Err: err}}
				return
			}
			canal <- saida{c: c, ok: true}
		}(s)
	}

	// O coletor fecha o canal quando todos terminarem, então o range abaixo
	// não trava nem com servidor pendurado (o timeout de cada cliente tampa).
	wg.Wait()
	close(canal)

	var clientes []*Cliente
	var erros []ErroServidor
	for out := range canal {
		if out.ok {
			clientes = append(clientes, out.c)
			continue
		}
		erros = append(erros, out.err)
	}

	// Ordem estável: clientes e erros na ordem em que os servidores foram
	// dados, para a saída não depender da corrida entre goroutines.
	ordem := make(map[string]int, len(servs))
	for i, s := range servs {
		if _, ja := ordem[s.Nome]; !ja {
			ordem[s.Nome] = i
		}
	}
	ordenarClientes(clientes, ordem)
	ordenarErros(erros, ordem)

	// ctx morto antes de qualquer conexão: nada a devolver.
	if err := ctx.Err(); err != nil && len(clientes) == 0 && len(erros) == 0 {
		return nil, nil
	}
	return clientes, erros
}

// FechaTodos fecha todos os clientes, juntando os erros numa só linha por
// servidor. Devolve nil se todos fecharam limpo.
func FechaTodos(cs []*Cliente) error {
	var errs []error
	for _, c := range cs {
		if c == nil {
			continue
		}
		if err := c.Fecha(); err != nil {
			errs = append(errs, fmt.Errorf("%s: %v", c.Fonte(), err))
		}
	}
	return errors.Join(errs...)
}

// ordenarClientes põe os clientes na ordem dos servidores de origem.
func ordenarClientes(cs []*Cliente, ordem map[string]int) {
	for i := 1; i < len(cs); i++ {
		for j := i; j > 0 && ordem[cs[j-1].Fonte()] > ordem[cs[j].Fonte()]; j-- {
			cs[j-1], cs[j] = cs[j], cs[j-1]
		}
	}
}

// ordenarErros faz o mesmo para o slice de falhas.
func ordenarErros(es []ErroServidor, ordem map[string]int) {
	for i := 1; i < len(es); i++ {
		for j := i; j > 0 && ordem[es[j-1].Nome] > ordem[es[j].Nome]; j-- {
			es[j-1], es[j] = es[j], es[j-1]
		}
	}
}
