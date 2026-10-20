package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ruanbarroso/barroso-harness/internal/tools"
)

// Progressive mode keeps MCP and management tools out of the function list;
// tool_search finds them and tool_call runs them.
func TestProgressiveDisclosure(t *testing.T) {
	s := &Session{}
	s.config.ToolDisclosure = DisclosureProgressive
	ran := ""
	mcpTool := tools.Spec{Name: "browser_click", Description: "Click an element in the browser page",
		Schema: map[string]any{"type": "object", "properties": map[string]any{"ref": map[string]any{"type": "string"}}, "required": []string{"ref"}},
		Exec: func(_ context.Context, _ string, raw json.RawMessage) (string, error) {
			ran = string(raw)
			return "clicked", nil
		}}
	s.Injetar(mcpTool)
	list := append(tools.SpecsBuiltins(), tools.Spec{Name: "mcp_add"}, mcpTool)
	direct, exec, catalog := s.disclose(list)
	for _, sp := range direct {
		if sp.Name == "browser_click" || sp.Name == "mcp_add" {
			t.Fatalf("%s must be deferred", sp.Name)
		}
	}
	if !hasTool(direct, toolSearchName) || !hasTool(direct, toolCallName) || !hasTool(exec, "browser_click") {
		t.Fatal("tool_search/tool_call must be direct and deferred tools executable")
	}
	if !strings.Contains(catalog, "browser_click — Click") {
		t.Fatalf("catalog:\n%s", catalog)
	}
	var search, call tools.Spec
	for _, sp := range direct {
		switch sp.Name {
		case toolSearchName:
			search = sp
		case toolCallName:
			call = sp
		}
	}
	out, _ := search.Exec(context.Background(), "", json.RawMessage(`{"query":"click browser"}`))
	if !strings.Contains(out, `"ref"`) {
		t.Fatalf("search must return the schema:\n%s", out)
	}
	if _, err := call.Exec(context.Background(), "", json.RawMessage(`{"name":"browser_click","arguments":{}}`)); err == nil {
		t.Fatal("missing required argument must fail")
	}
	if out, err := call.Exec(context.Background(), "", json.RawMessage(`{"name":"browser_click","arguments":{"ref":"e1"}}`)); err != nil || out != "clicked" || !strings.Contains(ran, "e1") {
		t.Fatalf("tool_call: %q %v %q", out, err, ran)
	}
	if n, a := shownCall(toolCallName, `{"name":"browser_click","arguments":{"ref":"e1"}}`); n != "browser_click" || !strings.Contains(a, "e1") {
		t.Fatalf("shownCall: %s %s", n, a)
	}
}
