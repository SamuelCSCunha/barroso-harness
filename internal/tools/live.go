package tools

import (
	"bytes"
	"context"
	"sync"
)

// A tool that produces output over time (shell) streams it to a sink
// carried by the context, so the screen can show it while the call runs. The
// call's result is unchanged: the sink only sees a copy.
type outputKey struct{}

// WithOutput returns ctx carrying sink, called with each chunk of live output.
// The sink may be called from any goroutine.
func WithOutput(ctx context.Context, sink func(chunk string)) context.Context {
	return context.WithValue(ctx, outputKey{}, sink)
}

func outputSink(ctx context.Context) func(string) {
	sink, _ := ctx.Value(outputKey{}).(func(string))
	return sink
}

// liveBuffer collects a command's merged stdout+stderr and copies each write
// to the sink. It is used as both Stdout and Stderr (a pointer, so exec
// serializes the writes).
type liveBuffer struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	sink func(string)
}

func (b *liveBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n, err := b.buf.Write(p)
	if b.sink != nil && n > 0 {
		b.sink(string(p[:n]))
	}
	return n, err
}

func (b *liveBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Bytes()
}

// OutputSinkForTest exposes the sink a tool would stream to (tests of other
// packages use it to stand in for shell).
func OutputSinkForTest(ctx context.Context) func(string) { return outputSink(ctx) }
