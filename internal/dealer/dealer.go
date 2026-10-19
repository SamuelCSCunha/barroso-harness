// Package dealer talks to the provider using the OpenAI Chat Completions
// standard (SSE streaming, tools, include_usage) and exposes the /models
// catalog in the same standard. The runtime builds the transcript; the dealer
// only translates both ways. Compaction is the runtime's concern.
package dealer

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// UserAgent identifies the client. By default Go sends "Go-http-client/1.1" —
// and some WAFs (Cloudflare among them) block exactly that with a 403 before
// looking at the key. A named UA avoids the false negative and also helps the
// provider identify the client in its logs.
const UserAgent = "barroso-harness"

// Config is the provider configuration. The key comes from the local config
// persisted by the CLI.
type Config struct {
	BaseURL      string `json:"base_url"`
	Model        string `json:"model"`
	APIKey       string `json:"-"`
	MaxTokens    int    `json:"max_tokens"`     // output cap; 0 = per-effort default (TetoPadrao)
	JanelaTokens int    `json:"context_window"` // 0 = unknown
	Effort       string `json:"effort"`         // empty = omit reasoning_effort from the request
}

// Usage is the provider's token usage.
type Usage struct {
	PromptTokens int `json:"prompt_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Mensagem is the internal protocol (serializable, byte-deterministic — it
// holds no maps). Roles: user, assistant, tool_call, tool_result.
type Mensagem struct {
	Role     string `json:"role"`
	Conteudo string `json:"conteudo,omitempty"`
	ToolID   string `json:"tool_id,omitempty"`
	ToolName string `json:"tool_name,omitempty"`
	ToolArgs string `json:"tool_args,omitempty"`
	IsErro   bool   `json:"is_erro,omitempty"`
	Usage    *Usage `json:"usage,omitempty"`
}

// ToolChamada is a tool call emitted by the model in a round.
type ToolChamada struct {
	ID   string `json:"id"`
	Nome string `json:"nome"`
	Args string `json:"args"` // raw input JSON
}

// Rodada is the result of one call to the model.
type Rodada struct {
	Texto       string
	Tools       []ToolChamada
	Usage       *Usage
	FimPorTexto bool
	// Cortada: the provider stopped at the output cap (finish_reason
	// "length"). Text and tool calls may be incomplete.
	Cortada bool
	// Raciocinio is the reasoning the provider exposed (reasoning or
	// reasoning_content in the delta) and DurRaciocinio the time from the
	// start of the request to its last chunk. Display only; never sent back
	// to the model.
	Raciocinio    string
	DurRaciocinio time.Duration
}

// Cliente is instantiated per turn.
type Cliente struct {
	cfg    Config
	maxOut int // output cap for this request (ComTeto); 0 = MaxTokens()
}

// Novo returns a client for cfg.
func Novo(cfg Config) *Cliente { return &Cliente{cfg: cfg} }

// MaxTokens is the output cap requested from the provider: the config's
// max_tokens, or TetoPadrao for the effort.
func (c *Cliente) MaxTokens() int {
	if c.maxOut > 0 {
		return c.maxOut
	}
	if c.cfg.MaxTokens > 0 {
		return c.cfg.MaxTokens
	}
	return TetoPadrao(c.cfg.Effort)
}

// ComTeto returns a copy of the client with a different output cap for its
// subsequent requests only (retry with a larger cap, trimming to the window).
func (c *Cliente) ComTeto(n int) *Cliente {
	c2 := *c
	c2.maxOut = n
	return &c2
}

// TetoPadrao: 32k, as OpenCode and Claude Code do for models with no known
// limit, plus the effort's reasoning budget — a high-effort model used to
// spend the old 4096 entirely on thinking and answer nothing.
func TetoPadrao(effort string) int {
	return max(32<<10, OrcamentoRaciocinio(effort)+(16<<10))
}

// OrcamentoRaciocinio is how much reasoning consumes per effort (the same
// table as the barroso-keys clamp).
func OrcamentoRaciocinio(effort string) int {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "max", "xhigh":
		return 32 << 10
	case "high":
		return 16 << 10
	case "medium":
		return 8 << 10
	case "low", "minimal":
		return 4 << 10
	}
	return 0
}

type oaiCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type oaiMsg struct {
	Role       string    `json:"role"`
	Content    *string   `json:"content,omitempty"`
	ToolCallID string    `json:"tool_call_id,omitempty"`
	ToolCalls  []oaiCall `json:"tool_calls,omitempty"`
}

func strNil(s string) *string { return &s }

// Corpo serializes the full chat/completions request. Public so the runtime
// can measure bytes (the bytes/tokens pair feeds calibration).
func (c *Cliente) Corpo(system string, msgs []Mensagem, tools []map[string]any) ([]byte, error) {
	list := []oaiMsg{{Role: "system", Content: strNil(system)}}
	for _, m := range msgs {
		switch m.Role {
		case "user":
			list = append(list, oaiMsg{Role: "user", Content: strNil(m.Conteudo)})
		case "assistant", "tool_call":
			a := oaiMsg{Role: "assistant", Content: strNil(m.Conteudo)}
			if m.ToolID != "" {
				var call oaiCall
				call.ID = m.ToolID
				call.Type = "function"
				call.Function.Name = m.ToolName
				call.Function.Arguments = m.ToolArgs
				a.ToolCalls = append(a.ToolCalls, call)
			}
			list = append(list, a)
		case "tool_result":
			list = append(list, oaiMsg{Role: "tool", Content: strNil(m.Conteudo), ToolCallID: m.ToolID})
		default:
			return nil, fmt.Errorf("unknown role: %q", m.Role)
		}
	}
	body := map[string]any{
		"model":          c.cfg.Model,
		"messages":       list,
		"stream":         true,
		"max_tokens":     c.MaxTokens(),
		"stream_options": map[string]any{"include_usage": true},
	}
	if effort := strings.TrimSpace(c.cfg.Effort); effort != "" && !strings.EqualFold(effort, "none") {
		// Omit an empty field or `none` (the provider rejects both).
		body["reasoning_effort"] = effort
	}
	if len(tools) > 0 {
		body["tools"] = tools
		body["tool_choice"] = "auto"
	}
	return json.Marshal(body)
}

type sseChunk struct {
	Choices []struct {
		Delta struct {
			Content          *string `json:"content"`
			Reasoning        *string `json:"reasoning"`
			ReasoningContent *string `json:"reasoning_content"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// RodarContext is Rodar with real cancellation: the request and the stream
// read abort when ctx expires or is canceled (the REPL uses Ctrl+C).
func (c *Cliente) RodarContext(ctx context.Context, system string, msgs []Mensagem, tools []map[string]any) (*Rodada, error) {
	if ctx == nil {
		return c.Rodar(system, msgs, tools)
	}
	body, err := c.Corpo(system, msgs, tools)
	if err != nil {
		return nil, err
	}
	endpoint := NormalizarURL(c.cfg.BaseURL) + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", UserAgent)
	if c.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}
	req.Header.Set("Accept", "text/event-stream")
	httpc := &http.Client{Timeout: 15 * time.Minute}
	resp, err := httpc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("provider: %w", err)
	}
	defer resp.Body.Close()
	return readRound(resp, ctx)
}

// Rodar runs one round: streams, accumulates text, assembles tool calls and
// captures usage. system and tools go in untouched — the dealer never edits
// history.
func (c *Cliente) Rodar(system string, msgs []Mensagem, tools []map[string]any) (*Rodada, error) {
	body, err := c.Corpo(system, msgs, tools)
	if err != nil {
		return nil, err
	}
	url := NormalizarURL(c.cfg.BaseURL) + "/chat/completions"
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", UserAgent)
	if c.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}
	req.Header.Set("Accept", "text/event-stream")

	httpc := &http.Client{Timeout: 15 * time.Minute}
	resp, err := httpc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("provider: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("provider HTTP %d: %s", resp.StatusCode, errorSummary(b, 300))
	}
	return readRound(resp, context.Background())
}

// readRound consumes the already-open SSE stream and builds the Rodada.
func readRound(resp *http.Response, ctx context.Context) (*Rodada, error) {
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("provider HTTP %d: %s", resp.StatusCode, errorSummary(b, 300))
	}
	r := &Rodada{}
	start := time.Now()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 1<<20), 4<<20)
	pending := map[int]*oaiCall{}
	for sc.Scan() {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}
		var ch sseChunk
		if err := json.Unmarshal([]byte(payload), &ch); err != nil {
			return nil, fmt.Errorf("invalid chunk: %w", err)
		}
		if len(ch.Choices) > 0 {
			d := &ch.Choices[0].Delta
			for _, tc := range d.ToolCalls {
				if pending[tc.Index] == nil {
					pending[tc.Index] = &oaiCall{ID: tc.ID, Type: "function"}
				}
				if tc.ID != "" {
					pending[tc.Index].ID = tc.ID
				}
				if tc.Function.Name != "" {
					pending[tc.Index].Function.Name += tc.Function.Name
				}
				if tc.Function.Arguments != "" {
					pending[tc.Index].Function.Arguments += tc.Function.Arguments
				}
			}
			if d.Content != nil {
				r.Texto += *d.Content
			}
			// Different providers use one field or the other (OpenRouter sends
			// both with the same text): the first non-empty one wins.
			if p := d.Reasoning; p != nil && *p != "" {
				r.Raciocinio += *p
				r.DurRaciocinio = time.Since(start)
			} else if p := d.ReasoningContent; p != nil && *p != "" {
				r.Raciocinio += *p
				r.DurRaciocinio = time.Since(start)
			}
			if f := ch.Choices[0].FinishReason; f != nil && *f == "length" {
				r.Cortada = true
			}
		}
		if ch.Usage != nil && r.Usage == nil {
			r.Usage = &Usage{PromptTokens: ch.Usage.PromptTokens, OutputTokens: ch.Usage.CompletionTokens}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("stream: %w", err)
	}
	// close tool calls in index order
	idx := make([]int, 0, len(pending))
	for i := range pending {
		idx = append(idx, i)
	}
	for i := 0; i < len(idx); i++ {
		for j := i + 1; j < len(idx); j++ {
			if idx[j] < idx[i] {
				idx[i], idx[j] = idx[j], idx[i]
			}
		}
	}
	for _, i := range idx {
		call := pending[i]
		r.Tools = append(r.Tools, ToolChamada{ID: call.ID, Nome: call.Function.Name, Args: call.Function.Arguments})
	}
	r.FimPorTexto = len(r.Tools) == 0
	return r, nil
}

// ── /models catalog ──────────────────────────────────────────────────────

// CatalogModel is a /models item, tolerant of unknown fields. ContextLength
// and Input may be missing (the provider does not publish them) — then the
// zero values apply and input filtering stays conservative (keeps it).
type CatalogModel struct {
	ID            string   `json:"id"`
	OwnedBy       string   `json:"owned_by,omitempty"`
	Created       int64    `json:"created,omitempty"`
	ContextLength int      `json:"context_length,omitempty"`
	Type          string   `json:"type,omitempty"`
	Input         []string `json:"input,omitempty"` // ["text"] | ["text","image"] | ["audio"]...
}

// ModelsResponse is the OpenAI /models envelope. It may carry extra metadata
// at the same level — ignored.
type ModelsResponse struct {
	Data    []CatalogModel `json:"data"`
	Object  string         `json:"object,omitempty"`
	RawJSON []byte         `json:"-"`
}

// NormalizarURL tolerates a base with or without a trailing slash and avoids
// duplicated suffixes (/v1/v1, /chat/completions/chat/completions).
func NormalizarURL(base string) string {
	b := strings.TrimSuffix(strings.TrimSpace(base), "/")
	for _, suffix := range []string{"/chat/completions", "/completions", "/models"} {
		if strings.HasSuffix(b, suffix) {
			b = strings.TrimSuffix(b, suffix)
		}
	}
	return b
}

const (
	catalogTimeout = 15 * time.Second
	catalogBodyCap = 1 << 20 // 1 MiB: a catalog is never huge; cuts off abuse
)

// errorSummary turns the body of an error response into ONE useful line.
// No provider returns plain HTML from an API — when it happens (WAF block
// page, edge 403 on the catalog), dumping the raw body into the terminal
// buries the real error under markup. So: if it is not JSON, extract the
// <title> or the first visible text and truncate.
func errorSummary(body []byte, max int) string {
	s := strings.TrimSpace(string(body))
	if s == "" {
		return "(empty body)"
	}
	if strings.HasPrefix(s, "{") || strings.HasPrefix(s, "[") {
		// JSON: try the OpenAI format {"error":{"message":...}} before truncating.
		var env struct {
			Error json.RawMessage `json:"error"`
		}
		if json.Unmarshal([]byte(s), &env) == nil && len(env.Error) > 0 {
			var msg struct {
				Message string `json:"message"`
			}
			if json.Unmarshal(env.Error, &msg) == nil && msg.Message != "" {
				return truncateLine(msg.Message, max)
			}
			return truncateLine(string(env.Error), max)
		}
		return truncateLine(s, max)
	}
	// HTML: <title> is usually the most informative ("Attention Required!",
	// "Sorry, you have been blocked").
	if i := strings.Index(strings.ToLower(s), "<title>"); i >= 0 {
		if j := strings.Index(s[i:], "</title>"); j > 0 {
			title := strings.TrimSpace(stripTags(s[i+7 : i+j]))
			if title != "" {
				return truncateLine(title, max)
			}
		}
	}
	// fallback: first chunk of visible text, without markup.
	if txt := stripTags(s); txt != "" {
		return truncateLine(txt, max)
	}
	return truncateLine(s, max)
}

// stripTags removes markup and collapses whitespace: what is left is the text
// the user reads.
func stripTags(s string) string {
	var sb strings.Builder
	inside := false
	for _, r := range s {
		switch {
		case r == '<':
			inside = true
		case r == '>':
			inside = false
			sb.WriteRune(' ')
		case !inside:
			sb.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(sb.String()), " ")
}

func truncateLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ") // a single line, no breaks
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

// Catalogo does GET {baseURL}/models with Bearer auth and returns the raw
// envelope.
func Catalogo(ctx context.Context, baseURL, apiKey string) (*ModelsResponse, error) {
	endpoint := NormalizarURL(baseURL) + "/models"
	req, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", UserAgent)
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	httpc := &http.Client{Timeout: catalogTimeout}
	resp, err := httpc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("catalog %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("catalog HTTP %d: %s", resp.StatusCode, errorSummary(b, 300))
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, catalogBodyCap+1))
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	if len(b) > catalogBodyCap {
		return nil, fmt.Errorf("catalog: response exceeded %d bytes", catalogBodyCap)
	}
	var out ModelsResponse
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("catalog: invalid JSON: %w", err)
	}
	out.RawJSON = b
	if len(out.Data) == 0 {
		return nil, fmt.Errorf("empty catalog at %s", endpoint)
	}
	return &out, nil
}

// Modelos returns the processed catalog: deduplicated, sorted alphabetically,
// only items that are at least plausibly chat models. Filtering is LENIENT:
// a provider may present {"input": ["text"]} as a list, a single string or
// publish nothing; with no signal, the model is kept. Explicit non-chat
// markers (embedding/tts/whisper/image/music) are dropped when the input does
// not confirm text.
func Modelos(ctx context.Context, baseURL, apiKey string) ([]CatalogModel, error) {
	resp, err := Catalogo(ctx, baseURL, apiKey)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []CatalogModel
	for _, m := range resp.Data {
		if m.ID == "" || seen[m.ID] {
			continue
		}
		if !plausibleChat(m) {
			continue
		}
		seen[m.ID] = true
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func plausibleChat(m CatalogModel) bool {
	if len(m.Input) > 0 {
		hasText := false
		for _, in := range m.Input {
			if strings.EqualFold(strings.TrimSpace(in), "text") {
				hasText = true
				break
			}
		}
		if !hasText {
			return false // published input WITHOUT text: audio/image/embedding
		}
		return true
	}
	// No input metadata: conservative — keep it, unless explicitly marked.
	marker := strings.ToLower(m.Type) + " " + strings.ToLower(m.ID)
	for _, bad := range []string{"embedding", "tts", "whisper", "transcri", "dall-e", "image-gen", "stabili", "stable-", "music", "moderation", "rerank"} {
		if strings.Contains(marker, bad) {
			return false
		}
	}
	return true
}

// ContextoDe returns the published window for model ID (0 = unknown).
func ContextoDe(list []CatalogModel, id string) int {
	for _, m := range list {
		if m.ID == id {
			return m.ContextLength
		}
	}
	return 0
}
