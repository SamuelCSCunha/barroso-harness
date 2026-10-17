package cli

import "github.com/ruanbarroso/barroso-harness/internal/config"

// configNovoTeste monta uma config pronta provider+model sem tocar disco.
func configNovoTeste(base, key, model string) config.Config {
	c := config.Config{Model: model}
	nome := "local-test"
	c.Providers = map[string]*config.Provider{
		nome: {BaseURL: base, APIKey: key},
	}
	c.CurrentProvider = nome
	c.BaseURL, c.APIKey = base, key
	return c
}
