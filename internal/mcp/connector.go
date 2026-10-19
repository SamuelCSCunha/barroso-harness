package mcp

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// ErroServidor is the failure of ONE MCP server. ConectarTodos never brings
// down the whole set because of a single server: the failed ones come back
// here, with name and error, and the integrator decides (continue without
// them, warn, or abort).
type ErroServidor struct {
	Nome string
	Err  error
}

// Error formats on ONE line, as "name: error".
func (e ErroServidor) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("MCP server %q: failure without detail", e.Nome)
	}
	return fmt.Sprintf("MCP server %q: %v", e.Nome, oneLine(e.Err.Error()))
}

// Unwrap lets errors.Is / errors.As reach the original error.
func (e ErroServidor) Unwrap() error { return e.Err }

// ConectarTodos connects each server independently: all in parallel, each
// with its own derived context.
//
// A failing server does NOT invalidate the others — it comes back as an item
// of the errors slice (never as the function's error). The function only
// returns non-nil when ctx arrives already cancelled/expired, and then
// returns nil clients.
//
// Returned clients are already open AND handshaken (Abre calls initialize);
// the caller only needs to call Ferramentas.
func ConectarTodos(ctx context.Context, servs []Servidor) ([]*Cliente, []ErroServidor) {
	if ctx == nil {
		ctx = context.Background()
	}
	if len(servs) == 0 {
		return nil, nil
	}

	type result struct {
		c   *Cliente
		err ErroServidor
		ok  bool
	}

	results := make(chan result, len(servs))
	var wg sync.WaitGroup
	for _, s := range servs {
		wg.Add(1)
		go func(s Servidor) {
			defer wg.Done()
			c, err := Novo(s)
			if err != nil {
				results <- result{err: ErroServidor{Nome: s.Nome, Err: err}}
				return
			}
			if err := c.Abre(ctx); err != nil {
				// Open failed: make sure the (stdio) subprocess does not become
				// a zombie and the connection is released.
				_ = c.Fecha()
				results <- result{err: ErroServidor{Nome: s.Nome, Err: err}}
				return
			}
			results <- result{c: c, ok: true}
		}(s)
	}

	// The channel is closed once everyone is done, so the range below does
	// not hang even with a stuck server (each client's timeout caps it).
	wg.Wait()
	close(results)

	var clients []*Cliente
	var errs []ErroServidor
	for out := range results {
		if out.ok {
			clients = append(clients, out.c)
			continue
		}
		errs = append(errs, out.err)
	}

	// Stable order: clients and errors in the order the servers were given,
	// so the output does not depend on the goroutine race.
	order := make(map[string]int, len(servs))
	for i, s := range servs {
		if _, seen := order[s.Nome]; !seen {
			order[s.Nome] = i
		}
	}
	sortClients(clients, order)
	sortErrors(errs, order)

	// ctx dead before any connection: nothing to return.
	if err := ctx.Err(); err != nil && len(clients) == 0 && len(errs) == 0 {
		return nil, nil
	}
	return clients, errs
}

// FechaTodos closes all clients, joining the errors into one line per
// server. Returns nil if all closed cleanly.
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

// sortClients puts the clients in the order of their source servers.
func sortClients(cs []*Cliente, order map[string]int) {
	for i := 1; i < len(cs); i++ {
		for j := i; j > 0 && order[cs[j-1].Fonte()] > order[cs[j].Fonte()]; j-- {
			cs[j-1], cs[j] = cs[j], cs[j-1]
		}
	}
}

// sortErrors does the same for the failures slice.
func sortErrors(es []ErroServidor, order map[string]int) {
	for i := 1; i < len(es); i++ {
		for j := i; j > 0 && order[es[j-1].Nome] > order[es[j].Nome]; j-- {
			es[j-1], es[j] = es[j], es[j-1]
		}
	}
}
