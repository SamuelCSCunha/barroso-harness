// Package dealer fala com o provider no padrão OpenAI Chat Completions
// (streaming SSE, tools, include_usage). O runtime monta o transcript;
// o dealer só traduz ida e volta. Compactação é problema do runtime.
package dealer

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Config provider. A chave nunca vive no arquivo: entra por BARROSO_API_KEY.
type Config struct {
	BaseURL      string `json:"base_url"`
	Model        string `json:"model"`
	APIKey       string `json:"-"`
	MaxTokens    int    `json:"max_tokens"`     // teto de saída; 0 = 4096
	JanelaTokens int    `json:"context_window"` // manual; 0 = desconhecida
}

// Usage do provider.
type Usage struct {
	PromptTokens int `json:"prompt_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Mensagem é o protocolo interno (serializável, determinístico em bytes —
// não guarda mapas). Roles: user, assistant, tool_call, tool_result.
type Mensagem struct {
	Role     string `json:"role"`
	Conteudo string `json:"conteudo,omitempty"`
	ToolID   string `json:"tool_id,omitempty"`
	ToolName string `json:"tool_name,omitempty"`
	ToolArgs string `json:"tool_args,omitempty"`
	IsErro   bool   `json:"is_erro,omitempty"`
	Usage    *Usage `json:"usage,omitempty"`
}

// ToolChamada é uma chamada emitida pelo modelo numa rodada.
type ToolChamada struct {
	ID   string `json:"id"`
	Nome string `json:"nome"`
	Args string `json:"args"` // JSON cru do input
}

// Rodada é o resultado de uma chamada ao modelo.
type Rodada struct {
	Texto       string
	Tools       []ToolChamada
	Usage       *Usage
	FimPorTexto bool
}

// Cliente é instanciado por turno.
type Cliente struct {
	cfg Config
}

func Novo(cfg Config) *Cliente { return &Cliente{cfg: cfg} }

func (c *Cliente) MaxTokens() int {
	if c.cfg.MaxTokens > 0 {
		return c.cfg.MaxTokens
	}
	return 4096
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

// Corpo serializa o request chat/completions completo. Público para o
// runtime medir bytes (o par bytes/tokens alimenta a calibragem).
func (c *Cliente) Corpo(system string, msgs []Mensagem, tools []map[string]any) ([]byte, error) {
	lista := []oaiMsg{{Role: "system", Content: strNil(system)}}
	for _, m := range msgs {
		switch m.Role {
		case "user":
			lista = append(lista, oaiMsg{Role: "user", Content: strNil(m.Conteudo)})
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
			lista = append(lista, a)
		case "tool_result":
			lista = append(lista, oaiMsg{Role: "tool", Content: strNil(m.Conteudo), ToolCallID: m.ToolID})
		default:
			return nil, fmt.Errorf("role desconhecido: %q", m.Role)
		}
	}
	body := map[string]any{
		"model":          c.cfg.Model,
		"messages":       lista,
		"stream":         true,
		"max_tokens":     c.MaxTokens(),
		"stream_options": map[string]any{"include_usage": true},
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
			Content   *string `json:"content"`
			ToolCalls []struct {
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

// Rodar executa uma rodada: streaming, acumula texto, monta tool calls e
// captura usage. system e tools entram intocados — dealer não edita histórico.
func (c *Cliente) Rodar(system string, msgs []Mensagem, tools []map[string]any) (*Rodada, error) {
	body, err := c.Corpo(system, msgs, tools)
	if err != nil {
		return nil, err
	}
	url := strings.TrimSuffix(c.cfg.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
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
		return nil, fmt.Errorf("provider HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}

	r := &Rodada{}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 1<<20), 4<<20)
	emCurso := map[int]*oaiCall{}
	for sc.Scan() {
		linha := sc.Text()
		if !strings.HasPrefix(linha, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(linha, "data:"))
		if payload == "[DONE]" {
			break
		}
		var ch sseChunk
		if err := json.Unmarshal([]byte(payload), &ch); err != nil {
			return nil, fmt.Errorf("chunk inválido: %w", err)
		}
		if len(ch.Choices) > 0 {
			d := &ch.Choices[0].Delta
			for _, tc := range d.ToolCalls {
				if emCurso[tc.Index] == nil {
					emCurso[tc.Index] = &oaiCall{ID: tc.ID, Type: "function"}
				}
				if tc.ID != "" {
					emCurso[tc.Index].ID = tc.ID
				}
				if tc.Function.Name != "" {
					emCurso[tc.Index].Function.Name += tc.Function.Name
				}
				if tc.Function.Arguments != "" {
					emCurso[tc.Index].Function.Arguments += tc.Function.Arguments
				}
			}
			if d.Content != nil {
				r.Texto += *d.Content
			}
		}
		if ch.Usage != nil && r.Usage == nil {
			r.Usage = &Usage{PromptTokens: ch.Usage.PromptTokens, OutputTokens: ch.Usage.CompletionTokens}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("stream: %w", err)
	}
	// fecha tool calls em ordem de index
	idx := make([]int, 0, len(emCurso))
	for i := range emCurso {
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
		call := emCurso[i]
		r.Tools = append(r.Tools, ToolChamada{ID: call.ID, Nome: call.Function.Name, Args: call.Function.Arguments})
	}
	r.FimPorTexto = len(r.Tools) == 0
	return r, nil
}
