package dealer

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func streamResp(lines ...string) *http.Response {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString("data: " + l + "\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(b.String()))}
}

func TestReadRoundArgsFragments(t *testing.T) {
	r, err := readRound(streamResp(
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","type":"function","function":{"name":"shell","arguments":""}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"command\": \"echo"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":" um\"}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	), context.Background(), time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Tools) != 1 || r.Tools[0].Args != `{"command": "echo um"}` {
		t.Fatalf("tools = %+v", r.Tools)
	}
}

// Upstreams that send the arguments as an object, or calls without an index,
// must not lose the arguments.
func TestReadRoundArgsObjectAndMissingIndex(t *testing.T) {
	r, err := readRound(streamResp(
		`{"choices":[{"delta":{"tool_calls":[{"id":"a","type":"function","function":{"name":"read","arguments":{"path":"x"}}},{"id":"b","type":"function","function":{"name":"read","arguments":{"path":"y"}}}]}}]}`,
	), context.Background(), time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Tools) != 2 || r.Tools[0].Args != `{"path":"x"}` || r.Tools[1].Args != `{"path":"y"}` {
		t.Fatalf("tools = %+v", r.Tools)
	}
}

// Two calls sharing index 0 with different ids are two calls, not one.
func TestReadRoundSameIndexNewID(t *testing.T) {
	r, err := readRound(streamResp(
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"read","arguments":"{\"path\":\"x\"}"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"b","function":{"name":"read","arguments":"{\"path\":\"y\"}"}}]}}]}`,
	), context.Background(), time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Tools) != 2 || r.Tools[0].ID != "a" || r.Tools[1].Args != `{"path":"y"}` {
		t.Fatalf("tools = %+v", r.Tools)
	}
}

// parallel_tool_calls=false goes out only when the config asks for it.
func TestBodyDisableParallelToolCalls(t *testing.T) {
	if _, ok := requestBody(t, Config{BaseURL: "https://x/v1", Model: "m"}, nil)["parallel_tool_calls"]; ok {
		t.Fatal("parallel_tool_calls sent without the option")
	}
	m := requestBody(t, Config{BaseURL: "https://x/v1", Model: "m", DisableParallelToolCalls: true}, nil)
	if m["parallel_tool_calls"] != false {
		t.Fatalf("parallel_tool_calls = %v", m["parallel_tool_calls"])
	}
}
