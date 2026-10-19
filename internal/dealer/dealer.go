// Package dealer fala com o provider no padrão OpenAI Chat Completions
// (streaming SSE, tools, include_usage) e expõe o catálogo /models no mesmo
// padrão. O runtime monta o transcript; o dealer só traduz ida e volta.
// Compactação é problema do runtime.
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

// UserAgent identificado. Go, por padrão, manda "Go-http-client/1.1" — e há
// WAF (Cloudflare entre eles) que bloqueia exatamente isso com 403 antes de
// olhar a chave. Um UA nomeado evita o falso-negativo e ainda ajuda o
// provider a identificar o cliente nos logs dele.
const UserAgent = "barroso-harness"

// Config provider. A chave vem da config local persistida pela CLI.
type Config struct {
	BaseURL      string `json:"base_url"`
	Model        string `json:"model"`
	APIKey       string `json:"-"`
	MaxTokens    int    `json:"max_tokens"`     // teto de saída; 0 = padrão por effort (TetoPadrao)
	JanelaTokens int    `json:"context_window"` // 0 = desconhecida
	Effort       string `json:"effort"`         // vazio = omitir reasoning_effort do request
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
	// Cortada: o provider encerrou por teto de saída (finish_reason
	// "length"). Texto e tool calls podem estar incompletos.
	Cortada bool
	// Raciocinio é o pensamento que o provider expôs (reasoning ou
	// reasoning_content no delta) e DurRaciocinio o tempo do início do
	// request até o último pedaço dele. Só para mostrar; não volta ao modelo.
	Raciocinio    string
	DurRaciocinio time.Duration
}

// Cliente é instanciado por turno.
type Cliente struct {
	cfg  Config
	teto int // teto deste request (ComTeto); 0 = MaxTokens()
}

func Novo(cfg Config) *Cliente { return &Cliente{cfg: cfg} }

// MaxTokens é o teto de saída pedido ao provider: o max_tokens da config,
// ou TetoPadrao para o effort.
func (c *Cliente) MaxTokens() int {
	if c.teto > 0 {
		return c.teto
	}
	if c.cfg.MaxTokens > 0 {
		return c.cfg.MaxTokens
	}
	return TetoPadrao(c.cfg.Effort)
}

// ComTeto devolve uma cópia do cliente com outro teto de saída só para os
// próximos requests dela (nova tentativa com teto maior, corte pela janela).
func (c *Cliente) ComTeto(n int) *Cliente {
	c2 := *c
	c2.teto = n
	return &c2
}

// TetoPadrao: 32k como OpenCode e Claude Code fazem para modelo sem limite
// conhecido, mais o orçamento de raciocínio do effort — um modelo com effort
// alto gastava os 4096 antigos inteiros pensando e não respondia nada.
func TetoPadrao(effort string) int {
	return max(32<<10, OrcamentoRaciocinio(effort)+(16<<10))
}

// OrcamentoRaciocinio é quanto o raciocínio consome por effort (a mesma
// tabela do clamp do barroso-keys).
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
	if effort := strings.TrimSpace(c.cfg.Effort); effort != "" && !strings.EqualFold(effort, "none") {
		// Omitimos campo vazio ou `none` (provider recusa ambos).
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

// RodarContext é Rodar com cancelamento real: o request e a leitura do stream
// abortam quando o ctx expira ou é cancelado (o REPL usa Ctrl+C).
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
	return lerRodada(resp, ctx)
}

// Rodar executa uma rodada: streaming, acumula texto, monta tool calls e
// captura usage. system e tools entram intocados — dealer não edita histórico.
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
		return nil, fmt.Errorf("provider HTTP %d: %s", resp.StatusCode, resumoErro(b, 300))
	}
	return lerRodada(resp, context.Background())
}

// lerRodada consome o stream SSE já aberto e monta a Rodada.
func lerRodada(resp *http.Response, ctx context.Context) (*Rodada, error) {
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("provider HTTP %d: %s", resp.StatusCode, resumoErro(b, 300))
	}
	r := &Rodada{}
	inicio := time.Now()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 1<<20), 4<<20)
	emCurso := map[int]*oaiCall{}
	for sc.Scan() {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
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
			// Providers diferentes usam um ou outro campo (OpenRouter manda
			// os dois com o mesmo texto): vale o primeiro preenchido.
			if p := d.Reasoning; p != nil && *p != "" {
				r.Raciocinio += *p
				r.DurRaciocinio = time.Since(inicio)
			} else if p := d.ReasoningContent; p != nil && *p != "" {
				r.Raciocinio += *p
				r.DurRaciocinio = time.Since(inicio)
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

// ── catálogo /models ─────────────────────────────────────────────────────

// CatalogModel é um item do /models, tolerante a campos desconhecidos.
// ContextLength e Input podem faltar (provider não publica) — aí valem os
// valores zero e a filtragem por input fica conservadora (mantém).
type CatalogModel struct {
	ID            string   `json:"id"`
	OwnedBy       string   `json:"owned_by,omitempty"`
	Created       int64    `json:"created,omitempty"`
	ContextLength int      `json:"context_length,omitempty"`
	Type          string   `json:"type,omitempty"`
	Input         []string `json:"input,omitempty"` // ["text"] | ["text","image"] | ["audio"]...
}

// ModelsResponse é o envelope OpenAI do /models. Pode vir com metadata
// extra no mesmo nível — ignorada.
type ModelsResponse struct {
	Data    []CatalogModel `json:"data"`
	Object  string         `json:"object,omitempty"`
	RawJSON []byte         `json:"-"`
}

// NormalizarURL tolera base com ou sem barra final e evita duplicação de
// sufixos (/v1/v1, /chat/completions/chat/completions).
func NormalizarURL(base string) string {
	b := strings.TrimSuffix(strings.TrimSpace(base), "/")
	for _, sufixo := range []string{"/chat/completions", "/completions", "/models"} {
		if strings.HasSuffix(b, sufixo) {
			b = strings.TrimSuffix(b, sufixo)
		}
	}
	return b
}

const (
	timeoutCatalogo = 15 * time.Second
	tetoBodyCatalog = 1 << 20 // 1 MiB: catálogo nunca é gigante; corta abuse
)

// resumoErro transforma o corpo de uma resposta de erro em UMA linha útil.
// Provider nenhum devolve HTML puro em API — quando acontece (página de
// bloqueio do WAF, catálogo 403 de edge), despejar o corpo cru no terminal
// enterra o erro real sob markup. Então: se não for JSON, extrai o <title>
// ou o primeiro texto visível e corta.
func resumoErro(body []byte, max int) string {
	s := strings.TrimSpace(string(body))
	if s == "" {
		return "(corpo vazio)"
	}
	if strings.HasPrefix(s, "{") || strings.HasPrefix(s, "[") {
		// JSON: tenta o formato OpenAI {"error":{"message":...}} antes de cortar.
		var env struct {
			Error json.RawMessage `json:"error"`
		}
		if json.Unmarshal([]byte(s), &env) == nil && len(env.Error) > 0 {
			var msg struct {
				Message string `json:"message"`
			}
			if json.Unmarshal(env.Error, &msg) == nil && msg.Message != "" {
				return cortarLinha(msg.Message, max)
			}
			return cortarLinha(string(env.Error), max)
		}
		return cortarLinha(s, max)
	}
	// HTML: <title> costuma ser o mais informativo ("Attention Required!",
	// "Sorry, you have been blocked").
	if i := strings.Index(strings.ToLower(s), "<title>"); i >= 0 {
		if j := strings.Index(s[i:], "</title>"); j > 0 {
			titulo := strings.TrimSpace(tirarTags(s[i+7 : i+j]))
			if titulo != "" {
				return cortarLinha(titulo, max)
			}
		}
	}
	// fallback: primeiro pedaço de texto visível, sem markup.
	if txt := tirarTags(s); txt != "" {
		return cortarLinha(txt, max)
	}
	return cortarLinha(s, max)
}

// tirarTags remove markup e colapsa espaços: sobra o texto que o usuário lê.
func tirarTags(s string) string {
	var sb strings.Builder
	dentro := false
	for _, r := range s {
		switch {
		case r == '<':
			dentro = true
		case r == '>':
			dentro = false
			sb.WriteRune(' ')
		case !dentro:
			sb.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(sb.String()), " ")
}

func cortarLinha(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ") // uma linha só, sem quebra
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

// Catalogo faz GET {baseURL}/models com Bearer e devolve o envelope cru.
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
	httpc := &http.Client{Timeout: timeoutCatalogo}
	resp, err := httpc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("catálogo %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("catálogo HTTP %d: %s", resp.StatusCode, resumoErro(b, 300))
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, tetoBodyCatalog+1))
	if err != nil {
		return nil, fmt.Errorf("catálogo: %w", err)
	}
	if len(b) > tetoBodyCatalog {
		return nil, fmt.Errorf("catálogo: resposta excedeu %d bytes", tetoBodyCatalog)
	}
	var out ModelsResponse
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("catálogo: JSON inválido: %w", err)
	}
	out.RawJSON = b
	if len(out.Data) == 0 {
		return nil, fmt.Errorf("catálogo vazio em %s", endpoint)
	}
	return &out, nil
}

// Modelos devolve o catálogo processado: sem duplicatas, ordenado
// alfabeticamente, só itens com pelo menos plausibilidade de chat. A
// filtragem é TOLERANTE: provider pode vestir {"input": ["text"]} como
// lista, string única ou não publicar nada; sem sinal, o modelo entra.
// Marcadores explícitos de não-chat (embedding/tts/whisper/image/music)
// saem quando o input não confirma texto.
func Modelos(ctx context.Context, baseURL, apiKey string) ([]CatalogModel, error) {
	resp, err := Catalogo(ctx, baseURL, apiKey)
	if err != nil {
		return nil, err
	}
	vistos := map[string]bool{}
	var out []CatalogModel
	for _, m := range resp.Data {
		if m.ID == "" || vistos[m.ID] {
			continue
		}
		if !plausivelChat(m) {
			continue
		}
		vistos[m.ID] = true
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func plausivelChat(m CatalogModel) bool {
	if len(m.Input) > 0 {
		temTexto := false
		for _, in := range m.Input {
			if strings.EqualFold(strings.TrimSpace(in), "text") {
				temTexto = true
				break
			}
		}
		if !temTexto {
			return false // input publicado SEM text: é áudio/imagem/embedding
		}
		return true
	}
	// Sem metadata de input: conservador — mantém, salvo marcador explícito.
	marcador := strings.ToLower(m.Type) + " " + strings.ToLower(m.ID)
	for _, ruim := range []string{"embedding", "tts", "whisper", "transcri", "dall-e", "image-gen", "stabili", "stable-", "music", "moderation", "rerank"} {
		if strings.Contains(marcador, ruim) {
			return false
		}
	}
	return true
}

// ContextoDe devolve a janela publicada para o modelo ID (0 = desconhecida).
func ContextoDe(lista []CatalogModel, id string) int {
	for _, m := range lista {
		if m.ID == id {
			return m.ContextLength
		}
	}
	return 0
}
