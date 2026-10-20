// Busy-turn input tests: steer messages, the queued dock and its order.
package cli

import (
	"strings"
	"testing"

	"github.com/ruanbarroso/barroso-harness/internal/runtime"
)

// A steer shows with the pending bar and turns into a normal box when the
// model receives it.
func TestSteerPendingThenDelivered(t *testing.T) {
	colorsOn = true
	defer func() { colorsOn = false }()
	sc, _ := newTestScreen(t, 100, 30, panelState{model: "m"})
	var got []runtime.Steer
	sc.src = &dialogSource{steer: func(s runtime.Steer) { got = append(got, s) }}
	sc.mu.Lock()
	ok := sc.steerTo("also B", nil)
	sc.mu.Unlock()
	if !ok || len(got) != 1 || got[0].Text != "also B" {
		t.Fatalf("steer = %v %v", ok, got)
	}
	if !strings.Contains(sc.frame(), color(fgBorder, bar)) {
		t.Fatal("a pending steer uses the border.base bar")
	}
	sc.markDelivered("also B")
	for _, it := range sc.items {
		if it.pending {
			t.Fatal("delivered steer must stop being pending")
		}
	}
}

// Without a session to steer into, nothing pretends to steer.
func TestSteerUnavailable(t *testing.T) {
	sc := &screen{out: &strings.Builder{}}
	if sc.steerTo("x", nil) {
		t.Fatal("no steer without a session")
	}
}

// The dock shows the count and the first prompt, above the prompt box, and the
// frame keeps every row in place.
func TestQueuedDock(t *testing.T) {
	colorsOn = true
	defer func() { colorsOn = false }()
	sc, _ := newTestScreen(t, 100, 30, panelState{model: "m"})
	plain := sc.frameFor()
	sc.enqueue(queuedPrompt{sent: "first queued"})
	sc.enqueue(queuedPrompt{sent: "second"})
	f := sc.frameFor()
	if f.dockRow != plain.boxTop-dockRows || f.statusRow != f.dockRow-1 || f.boxTop != plain.boxTop {
		t.Fatalf("frame = %+v (plain %+v)", f, plain)
	}
	rows := visualize(sc.frame(), 100, 30)
	if got := rowAt(rows, f.dockRow+1); !strings.Contains(got, "2 queued · first queued") {
		t.Fatalf("dock = %q", got)
	}
	// A click on the dock opens the queued prompts dialog; choosing removes.
	for _, r := range sc.hits.regions {
		if r.id == "dock" {
			r.action()
			break
		}
	}
	if sc.dlg == nil || sc.dlg.title != "Queued prompts" {
		t.Fatalf("dialog = %+v", sc.dlg)
	}
	sc.dialogKey(keyEvent{action: keyEnter})
	if len(sc.queued) != 1 || sc.queued[0].sent != "second" {
		t.Fatalf("queue = %+v", sc.queued)
	}
}

// Queued prompts go out one at a time, in order, only when the REPL waits for
// a line and no turn runs; an interrupt pauses them.
func TestNextQueuedGates(t *testing.T) {
	sc := &screen{out: &strings.Builder{}}
	sc.enqueue(queuedPrompt{sent: "a"})
	sc.enqueue(queuedPrompt{sent: "b"})
	if _, ok := sc.nextQueued(); ok {
		t.Fatal("not before the REPL is ready")
	}
	sc.replReady, sc.busy = true, true
	if _, ok := sc.nextQueued(); ok {
		t.Fatal("not while a turn runs")
	}
	sc.busy, sc.queuePaused = false, true
	if _, ok := sc.nextQueued(); ok {
		t.Fatal("not while paused by an interrupt")
	}
	sc.queuePaused = false
	sc.partial = "select model > "
	if _, ok := sc.nextQueued(); ok {
		t.Fatal("never into a command's question")
	}
	sc.partial = ""
	q, ok := sc.nextQueued()
	if !ok || q.sent != "a" || len(sc.queued) != 1 {
		t.Fatalf("next = %v %v", q, ok)
	}
}

// Steer messages the turn ended without delivering leave the transcript and go
// to the front of the queue, in order.
func TestRequeueUndelivered(t *testing.T) {
	colorsOn = true
	defer func() { colorsOn = false }()
	sc, _ := newTestScreen(t, 100, 30, panelState{model: "m"})
	sc.src = &dialogSource{steer: func(runtime.Steer) {}}
	sc.mu.Lock()
	sc.steerTo("s1", nil)
	sc.steerTo("s2", nil)
	sc.enqueue(queuedPrompt{sent: "later"})
	sc.mu.Unlock()
	sc.requeue([]runtime.Steer{{Text: "s1"}, {Text: "s2"}})
	var order []string
	for _, q := range sc.queued {
		order = append(order, q.sent)
	}
	if strings.Join(order, ",") != "s1,s2,later" {
		t.Fatalf("queue = %v", order)
	}
	if txt := sc.transcriptText(); strings.Contains(txt, "s1") {
		t.Fatalf("undelivered steer stays on screen:\n%s", txt)
	}
}
