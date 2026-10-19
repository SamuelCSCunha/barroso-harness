package cli

import "github.com/ruanbarroso/barroso-harness/internal/config"

// newTestConfig builds a ready provider+model config without touching disk.
func newTestConfig(base, key, model string) config.Config {
	c := config.Config{Model: model}
	name := "local-test"
	c.Providers = map[string]*config.Provider{
		name: {BaseURL: base, APIKey: key},
	}
	c.CurrentProvider = name
	c.BaseURL, c.APIKey = base, key
	return c
}
