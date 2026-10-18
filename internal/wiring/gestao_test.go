package wiring

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/runtime"
)

// servidorMCPFake responde initialize, notifications e tools/list com uma tool "eco".
func servidorMCPFake(t *testing.T) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.Unmarshal(b, &req)
		if len(req.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var res any
		switch req.Method {
		case "initialize":
			res = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "fake", "version": "1"}}
		case "tools/list":
			res = map[string]any{"tools": []any{map[string]any{"name": "eco", "description": "eco", "inputSchema": map[string]any{"type": "object"}}}}
		default:
			res = map[string]any{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": res})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func ferramenta(t *testing.T, s *runtime.Session, nome string) bool {
	for _, sp := range s.Catalogo(t.TempDir()) {
		if sp.Name == nome {
			return true
		}
	}
	return false
}

func TestGestorMCPAddListRemove(t *testing.T) {
	sess := runtime.NewSession(config.Config{})
	g := novoGestorMCP(context.Background(), sess)
	var gravado []config.MCPServidor
	g.salvar = func(l []config.MCPServidor) error { gravado = l; return nil }
	sess.Injetar(g.specs()...)
	sp := map[string]func(string) (string, error){}
	for _, s := range g.specs() {
		s := s
		sp[s.Name] = func(a string) (string, error) { return s.Exec(context.Background(), "", json.RawMessage(a)) }
	}

	out, err := sp["mcp_add"](`{"name":"fake","type":"http","url":"` + servidorMCPFake(t) + `"}`)
	if err != nil || !strings.Contains(out, "1 tools disponíveis: eco") {
		t.Fatalf("mcp_add: %q %v", out, err)
	}
	if len(gravado) != 1 || gravado[0].Nome != "fake" {
		t.Fatalf("não persistiu: %+v", gravado)
	}
	if !ferramenta(t, sess, "eco") {
		t.Fatal("tool do servidor deveria estar no catálogo")
	}
	if out, _ := sp["mcp_list"](`{}`); !strings.Contains(out, "fake [http]") || !strings.Contains(out, "conectado") {
		t.Fatalf("mcp_list: %q", out)
	}
	if out, err := sp["mcp_remove"](`{"name":"fake"}`); err != nil || !strings.Contains(out, "1 tools saíram") {
		t.Fatalf("mcp_remove: %q %v", out, err)
	}
	if ferramenta(t, sess, "eco") || len(gravado) != 0 {
		t.Fatal("remove deveria tirar a tool e a config")
	}
}

func TestGestorMCPAddQueNaoConectaFicaVisivel(t *testing.T) {
	sess := runtime.NewSession(config.Config{})
	g := novoGestorMCP(context.Background(), sess)
	g.salvar = func([]config.MCPServidor) error { return nil }
	var add, list func(string) (string, error)
	for _, s := range g.specs() {
		s := s
		f := func(a string) (string, error) { return s.Exec(context.Background(), "", json.RawMessage(a)) }
		switch s.Name {
		case "mcp_add":
			add = f
		case "mcp_list":
			list = f
		}
	}
	out, err := add(`{"name":"morto","type":"http","url":"http://127.0.0.1:1/mcp"}`)
	if err != nil || !strings.Contains(out, "NÃO conectou") {
		t.Fatalf("add sem servidor: %q %v", out, err)
	}
	if out, _ := list(`{}`); !strings.Contains(out, "morto") || !strings.Contains(out, "NÃO conectado") {
		t.Fatalf("list: %q", out)
	}
	if _, err := add(`{"name":"x","type":"stdio"}`); err == nil {
		t.Fatal("stdio sem command deveria ser recusado")
	}
}
