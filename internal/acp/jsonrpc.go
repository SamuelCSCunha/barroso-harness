// Framing e tipos de JSON-RPC 2.0 sobre transporte de bytes newline-delimited.
// O agente fala por stdio, então cada mensagem é UMA linha JSON terminada em
// "\n": nenhum Content-Length header, nenhum framing adicional. Um
// bufio.Scanner com teto de 8 MiB impede que uma linha gigante (ou um cliente
// malcomportado) estoure a memória do processo; passar do teto encerra o
// scanner com ErrTooLong e o laço de serviço morre com erro honesto, não com
// pânico.
//
// Regras de identificador: o id é preservado como json.RawMessage, então um
// request com id numérico recebe resposta com id numérico e um com id string
// recebe id string. Notificação é o que não tem id (id ausente ou null) — e
// notificação NUNCA ganha resposta.
package acp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
)

// MaxLinha é o teto de uma mensagem JSON-RPC em bytes (8 MiB). Documentos
// grandes de sessão (transcripts, resultados de tool) cabem; uma linha sem
// fim não.
const MaxLinha = 8 << 20

// Versão do protocolo JSON-RPC aceita neste transporte.
const versaoJSONRPC = "2.0"

// Códigos de erro JSON-RPC 2.0 + a faixa customizada do agente.
const (
	CodParse        = -32700 // reservado; framing inteiro por linha, quase inerte
	CodInvalido     = -32600 // invalid request
	CodMetodo       = -32601 // method not found
	CodParams       = -32602 // invalid params
	CodInterno      = -32603 // internal error
	CodTurnoRodando = -32001 // agente: já existe um turno em voo nesta sessão
	CodErroAgente   = -32000 // agente: falha do turno / cancelamento
)

// Request é uma mensagem com id: exige resposta.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// TemID distingue request de notificação. id ausente ou `null` = notificação.
func (r Request) TemID() bool {
	id := bytes.TrimSpace(r.ID)
	return len(id) > 0 && !bytes.Equal(id, []byte("null"))
}

// Notification é uma mensagem sem id (mão única, sem resposta).
type Notification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

// Response é a resposta a um Request. Result e Error são mutuamente
// exclusivos; Error não-nulo silencia Result.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *Erro           `json:"error,omitempty"`
}

// Erro é o objeto error do JSON-RPC. Data é livre e some quando vazio.
type Erro struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *Erro) Error() string {
	if e == nil {
		return "<erro json-rpc nil>"
	}
	return e.Message
}

// NovaResposta empacota um resultado de sucesso preservando o id cru.
func NovaResposta(id json.RawMessage, result any) Response {
	return Response{JSONRPC: versaoJSONRPC, ID: id, Result: result}
}

// NovoErro empacota uma falha preservando o id cru.
func NovoErro(id json.RawMessage, code int, msg string) Response {
	return Response{JSONRPC: versaoJSONRPC, ID: id, Error: &Erro{Code: code, Message: msg}}
}

// NovaNotificacao empacota uma notificação de mão única.
func NovaNotificacao(method string, params any) Notification {
	return Notification{JSONRPC: versaoJSONRPC, Method: method, Params: params}
}

// escrever serializa v, acrescenta o "\n" do framing e escreve TUDO numa única
// chamada de Write — duas escritas separadas poderiam intercalar com a linha
// de outro goroutine e produzir JSON partido no fio.
func escrever(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = w.Write(b)
	return err
}

// EscreverLinha é a fronteira testável de escrever: serializa e emite uma
// linha JSON com "\n" final, com flush se o writerBuffer tiver.
func EscreverLinha(w io.Writer, v any) error {
	if err := escrever(w, v); err != nil {
		return err
	}
	return flush(w)
}

// flush é seguro para writers sem flush (os.Stdout não bufferiza).
func flush(w io.Writer) error {
	if f, ok := w.(interface{ Flush() error }); ok {
		return f.Flush()
	}
	return nil
}

// WriteError responde um request com falha. Mantida exportada porque main.go
// (e outros hosts de protocolo) precisam responder fora do laço do agente.
func WriteError(w io.Writer, id json.RawMessage, code int, msg string) error {
	return EscreverLinha(w, NovoErro(id, code, msg))
}

// DecodificarLinha converte uma linha em Request. Linha vazia (só espaços)
// devolve ok=false sem erro: branco não é mensagem.
func DecodificarLinha(linha []byte) (Request, bool, error) {
	if len(bytes.TrimSpace(linha)) == 0 {
		return Request{}, false, nil
	}
	var r Request
	if err := json.Unmarshal(linha, &r); err != nil {
		return Request{}, false, err
	}
	return r, true, nil
}

// NovoScanner devolve o scanner de linhas da conexão, já com o teto aplicado.
func NovoScanner(r io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), MaxLinha)
	return sc
}
