// Package config — tests: BARROSO_BASE_URL / BARROSO_AUTH_TOKEN override the
// active provider in memory and never reach config.json.
package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnvOverridesActiveProvider(t *testing.T) {
	home(t)
	c := setTestProvider(Config{Model: "m"}, "keys-barroso", DefaultBaseURL, "disk-key")
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvBaseURL, "https://llm-gateway.example.com/v1/")
	t.Setenv(EnvAuthToken, "env-token")
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	p := got.Ativo()
	if !got.FromEnv() || p.BaseURL != "https://llm-gateway.example.com/v1" || p.APIKey != "env-token" {
		t.Fatalf("env did not win: %+v", p)
	}
	if got.BaseURL != p.BaseURL || got.APIKey != p.APIKey || !got.Pronta() {
		t.Fatalf("resolved fields out of sync: %+v", got)
	}
	if strings.Contains(got.Resumo(), "env-token") {
		t.Fatal("status line leaked the token")
	}
}

func TestEnvTokenAloneKeepsConfiguredURL(t *testing.T) {
	home(t)
	c := setTestProvider(Config{Model: "m"}, "other", "https://other.example/v1", "disk-key")
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvAuthToken, "env-token")
	got, _ := Load()
	if p := got.Ativo(); p.BaseURL != "https://other.example/v1" || p.APIKey != "env-token" {
		t.Fatalf("got %+v", p)
	}
}

func TestEnvWithoutConfigFileUsesDefaultURL(t *testing.T) {
	home(t)
	t.Setenv(EnvAuthToken, "env-token")
	got, _ := Load()
	if p := got.Ativo(); p == nil || p.BaseURL != DefaultBaseURL || p.APIKey != "env-token" {
		t.Fatalf("got %+v", p)
	}
	if got.Pronta() {
		t.Fatal("still needs a model")
	}
}

func TestEnvNeverSaved(t *testing.T) {
	dir, _ := home(t)
	c := setTestProvider(Config{Model: "m"}, "keys-barroso", DefaultBaseURL, "disk-key")
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvBaseURL, "https://llm-gateway.example.com")
	t.Setenv(EnvAuthToken, "env-token")
	got, _ := Load()
	got.Model = "other-model"
	if err := got.Save(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if s := string(b); strings.Contains(s, "env-token") || strings.Contains(s, "llm-gateway") || !strings.Contains(s, "disk-key") {
		t.Fatalf("env leaked to disk or disk key lost:\n%s", s)
	}
}
