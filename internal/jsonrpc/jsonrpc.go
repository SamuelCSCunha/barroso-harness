// Package jsonrpc is the JSON-RPC 2.0 core shared by the MCP client
// (internal/mcp) and the hosted web providers (internal/web): messages, ids,
// one-line errors, response parsing (plain JSON, batch and SSE) and the
// streamable-HTTP POST with Mcp-Session-Id.
//
// It speaks no MCP method itself. It sits BELOW both packages so neither has
// to import the other — web importing mcp would close the cycle
// wiring → mcp → web → mcp.
package jsonrpc

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ── messages ─────────────────────────────────────────────────────────────

// Message is the common protocol unit: request/response have an id,
// notifications do not. Unknown fields are ignored on unmarshal — MCP adds
// fields freely and the client must tolerate them.
type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// HasID tells request/response (id present) from notification (absent).
// id:null does not count.
func (m Message) HasID() bool {
	v := bytes.TrimSpace(m.ID)
	return len(v) > 0 && !bytes.Equal(v, []byte("null"))
}

// Error is the JSON-RPC error object (code + message required; data is
// free-form and unpredictable across servers).
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Error formats on ONE line: raw data is never dumped (it may hold secrets).
func (e *Error) Error() string {
	if e == nil {
		return "MCP server error"
	}
	msg := OneLine(e.Message)
	if msg == "" {
		msg = "(no message)"
	}
	if e.Code != 0 {
		return fmt.Sprintf("MCP server returned error %d: %s", e.Code, msg)
	}
	return "MCP server returned error: " + msg
}

// NewNotification builds a JSON-RPC notification (no id; no response expected).
func NewNotification(method string, params any) Message {
	m := Message{JSONRPC: "2.0", Method: method}
	if params != nil {
		m.Params = Bytes(params)
	}
	return m
}

// Request bundles a request with a numeric id and its already-serialized body.
type Request struct {
	ID     int64
	Method string
	Body   []byte
}

// NewRequest serializes a request. An unserializable value leaves Body empty
// and the send fails loudly (params are always internal structs).
func NewRequest(id int64, method string, params any) Request {
	m := Message{JSONRPC: "2.0", ID: Bytes(id), Method: method}
	if params != nil {
		m.Params = Bytes(params)
	}
	b, err := json.Marshal(m)
	if err != nil {
		b = nil
	}
	return Request{ID: id, Method: method, Body: b}
}

// Bytes converts an internal value to JSON; if impossible it returns null
// (never panic on a network path).
func Bytes(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("null")
	}
	return b
}

// DecodeResult unmarshals tolerantly: a missing or null result leaves the
// target at its zero value instead of erroring.
func DecodeResult(raw json.RawMessage, target any) error {
	clean := bytes.TrimSpace(raw)
	if len(clean) == 0 || bytes.Equal(clean, []byte("null")) {
		return nil
	}
	return json.Unmarshal(clean, target)
}

// ── ids ──────────────────────────────────────────────────────────────────

// SameID compares ids tolerating number vs string.
func SameID(a, b json.RawMessage) bool { return IDKey(a) == IDKey(b) }

// IDKey reduces the id to a stable comparable form (n:1, s:abc).
func IDKey(id json.RawMessage) string {
	v := bytes.TrimSpace(id)
	if len(v) == 0 {
		return ""
	}
	var num json.Number
	if json.Unmarshal(v, &num) == nil {
		if i, err := num.Int64(); err == nil {
			return "n:" + fmt.Sprint(i)
		}
		return "s:" + num.String()
	}
	var s string
	if json.Unmarshal(v, &s) == nil {
		return "s:" + s
	}
	return string(v)
}

// ── response parsing ─────────────────────────────────────────────────────

// ErrEmptyResponse flags a missing body in a 2xx response: valid for a
// notification (the server may reply 202/200 without a body), invalid for a
// request, where a JSON-RPC response is mandatory.
var ErrEmptyResponse = errors.New("response without body")

// ResponseFrom finds the message with the matching id: accepts a single
// JSON-RPC message, a batch (array) and SSE (data: lines).
func ResponseFrom(body []byte, contentType string, id json.RawMessage) (Message, error) {
	if strings.Contains(strings.ToLower(contentType), "text/event-stream") {
		return readSSE(bytes.NewReader(body), id, int64(len(body))+1)
	}
	start := bytes.TrimSpace(body)
	if bytes.HasPrefix(start, []byte("data:")) || bytes.HasPrefix(start, []byte("event:")) ||
		bytes.HasPrefix(start, []byte("id:")) || bytes.HasPrefix(start, []byte(":")) {
		// SSE without an informative Content-Type. The ":" covers an SSE
		// COMMENT (":ping"), which comes before data: in real streams —
		// sniffing only by the other three prefixes would treat that stream
		// as JSON and fail.
		return readSSE(bytes.NewReader(body), id, int64(len(body))+1)
	}
	return jsonResponse(body, id)
}

// jsonResponse extracts from a JSON-RPC object or an array (batch).
func jsonResponse(body []byte, id json.RawMessage) (Message, error) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return Message{}, ErrEmptyResponse
	}
	if body[0] == '[' {
		var batch []Message
		if err := json.Unmarshal(body, &batch); err != nil {
			return Message{}, fmt.Errorf("invalid JSON response: %w", err)
		}
		return findByID(batch, id)
	}
	var m Message
	if err := json.Unmarshal(body, &m); err != nil {
		return Message{}, fmt.Errorf("invalid JSON response: %w", err)
	}
	if !m.HasID() {
		return Message{}, fmt.Errorf("server replied with a notification, not a result")
	}
	if !SameID(m.ID, id) {
		return Message{}, fmt.Errorf("response with unexpected id: %s", IDKey(m.ID))
	}
	return m, nil
}

// readSSE reads the data: lines — one JSON-RPC message per event, or a
// batch — and returns AS SOON AS the matching id shows up. A server that keeps
// the stream open after answering (allowed by the spec) therefore never holds
// the call until its timeout. Comments (":ping"), event: and "[DONE]" are
// ignored. At most max bytes are read.
func readSSE(r io.Reader, id json.RawMessage, max int64) (Message, error) {
	if max <= 0 {
		max = MaxBody
	}
	limited := &io.LimitedReader{R: r, N: max}
	sc := bufio.NewScanner(limited)
	sc.Buffer(make([]byte, 0, 64<<10), int(min(max, MaxBody)))
	for sc.Scan() {
		for _, m := range sseMessages(sc.Text()) {
			if SameID(m.ID, id) {
				return m, nil
			}
		}
	}
	if err := sc.Err(); err != nil {
		return Message{}, fmt.Errorf("reading event stream: %w", err)
	}
	if limited.N <= 0 {
		return Message{}, fmt.Errorf("event stream exceeded %d bytes without the response", max)
	}
	return Message{}, fmt.Errorf("response with id %s not found in body", IDKey(id))
}

// sseMessages returns the messages with an id carried by ONE SSE line.
func sseMessages(raw string) []Message {
	line := strings.TrimRight(raw, "\r")
	if !strings.HasPrefix(line, "data:") {
		return nil
	}
	data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if data == "" || data == "[DONE]" {
		return nil
	}
	var found []Message
	if data[0] == '[' {
		var batch []Message
		if json.Unmarshal([]byte(data), &batch) == nil {
			for _, m := range batch {
				if m.HasID() {
					found = append(found, m)
				}
			}
			return found
		}
	}
	var m Message
	if json.Unmarshal([]byte(data), &m) != nil {
		return nil // keep-alive or non-JSON event
	}
	if m.HasID() {
		found = append(found, m)
	}
	return found
}

// findByID looks for the response with the matching id in a list (batch).
func findByID(list []Message, id json.RawMessage) (Message, error) {
	for _, m := range list {
		if SameID(m.ID, id) {
			return m, nil
		}
	}
	return Message{}, fmt.Errorf("response with id %s not found in body", IDKey(id))
}

// ── one-line text ────────────────────────────────────────────────────────

// Summarize reduces the body to the essentials on ONE line (WAF HTML, JSON
// error or plain text) — never dumps the raw body to the terminal.
func Summarize(b []byte, max int) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > max {
		s = s[:max] + "…"
	}
	if s == "" {
		return "(empty body)"
	}
	return s
}

// OneLine collapses spaces and line breaks: messages on a single line.
func OneLine(s string) string { return Summarize([]byte(s), 400) }
