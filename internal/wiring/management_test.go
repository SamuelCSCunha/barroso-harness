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

// fakeMCPServer answers initialize, notifications and tools/list with an "eco" tool.
func fakeMCPServer(t *testing.T) string {
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

func hasTool(t *testing.T, s *runtime.Session, name string) bool {
	for _, sp := range s.Catalogo(t.TempDir()) {
		if sp.Name == name {
			return true
		}
	}
	return false
}

func TestMCPManagerAddListRemove(t *testing.T) {
	sess := runtime.NewSession(config.Config{})
	m := newMCPManager(context.Background(), sess)
	var saved []config.MCPServidor
	m.save = func(l []config.MCPServidor) error { saved = l; return nil }
	sess.Injetar(m.specs()...)
	sp := map[string]func(string) (string, error){}
	for _, s := range m.specs() {
		s := s
		sp[s.Name] = func(a string) (string, error) { return s.Exec(context.Background(), "", json.RawMessage(a)) }
	}

	out, err := sp["mcp_add"](`{"name":"fake","type":"http","url":"` + fakeMCPServer(t) + `"}`)
	if err != nil || !strings.Contains(out, "1 tools available: eco") {
		t.Fatalf("mcp_add: %q %v", out, err)
	}
	if len(saved) != 1 || saved[0].Nome != "fake" {
		t.Fatalf("did not persist: %+v", saved)
	}
	if !hasTool(t, sess, "eco") {
		t.Fatal("the server's tool should be in the catalog")
	}
	if out, _ := sp["mcp_list"](`{}`); !strings.Contains(out, "fake [http]") || !strings.Contains(out, "connected") {
		t.Fatalf("mcp_list: %q", out)
	}
	if out, err := sp["mcp_remove"](`{"name":"fake"}`); err != nil || !strings.Contains(out, "1 tools left") {
		t.Fatalf("mcp_remove: %q %v", out, err)
	}
	if hasTool(t, sess, "eco") || len(saved) != 0 {
		t.Fatal("remove should drop the tool and the config")
	}
}

func TestMCPManagerAddThatDoesNotConnectStaysVisible(t *testing.T) {
	sess := runtime.NewSession(config.Config{})
	m := newMCPManager(context.Background(), sess)
	m.save = func([]config.MCPServidor) error { return nil }
	var add, list func(string) (string, error)
	for _, s := range m.specs() {
		s := s
		f := func(a string) (string, error) { return s.Exec(context.Background(), "", json.RawMessage(a)) }
		switch s.Name {
		case "mcp_add":
			add = f
		case "mcp_list":
			list = f
		}
	}
	out, err := add(`{"name":"dead","type":"http","url":"http://127.0.0.1:1/mcp"}`)
	if err != nil || !strings.Contains(out, "did NOT connect") {
		t.Fatalf("add without server: %q %v", out, err)
	}
	if out, _ := list(`{}`); !strings.Contains(out, "dead") || !strings.Contains(out, "NOT connected") {
		t.Fatalf("list: %q", out)
	}
	if _, err := add(`{"name":"x","type":"stdio"}`); err == nil {
		t.Fatal("stdio without command should be rejected")
	}
}
