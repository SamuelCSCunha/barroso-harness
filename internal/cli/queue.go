// Busy-turn input, the way OpenCode handles it (spec §10): Enter while a turn
// runs sends a STEER — the message reaches the model at the next tool-round
// boundary without cancelling the turn — and ctrl+x Enter QUEUES a prompt that
// is sent after the turn ends. Steer messages show inline with a gray bar until
// the model receives them; queued prompts sit in a dock above the prompt.
//
// Only the full screen offers this: it is the only path with a live session to
// steer into (-p and ACP have no prompt to type in while a turn runs).
package cli

import (
	"strconv"
	"strings"

	"github.com/ruanbarroso/barroso-harness/internal/runtime"
)

// queuedPrompt is a prompt waiting for the turn to end.
type queuedPrompt struct {
	entry  historyEntry
	sent   string
	images []string
}

// dockRows is the height of the queued dock: padding, the line, padding.
const dockRows = 3

// steerTo sends a message into the running turn and shows it with a pending
// bar. Caller holds t.mu. Reports false when this run has no session to steer.
func (t *screen) steerTo(sent string, images []string) bool {
	if t.src == nil || t.src.steer == nil {
		return false
	}
	t.addItem(&item{kind: "user", text: sent, images: images, pending: true})
	t.src.steer(runtime.Steer{Text: sent, Images: images})
	return true
}

// enqueue puts a prompt on the dock. Caller holds t.mu.
func (t *screen) enqueue(q queuedPrompt) {
	t.queued = append(t.queued, q)
}

// markDelivered turns the oldest pending steer with this text into a normal
// message box: the model has it now.
func (t *screen) markDelivered(text string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, l := range t.lines {
		if it := t.itemOf(l); strings.HasPrefix(l, itemMark) && it != nil && it.pending && it.text == text {
			t.mutate(func() { it.pending = false })
			return
		}
	}
}

// requeue takes back the steer messages the turn ended without delivering: they
// leave the transcript (the REPL draws them again when they are sent) and go to
// the front of the dock, in order.
func (t *screen) requeue(left []runtime.Steer) {
	if len(left) == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	var back []queuedPrompt
	t.mutate(func() {
		for _, s := range left {
			for i, l := range t.lines {
				it := t.itemOf(l)
				if !strings.HasPrefix(l, itemMark) || it == nil || !it.pending || it.text != s.Text {
					continue
				}
				from := i
				if from > 0 && t.lines[from-1] == "" {
					from--
				}
				t.lines = append(t.lines[:from], t.lines[i+1:]...)
				delete(t.items, it.id)
				break
			}
			back = append(back, queuedPrompt{entry: historyEntry{text: s.Text}, sent: s.Text, images: s.Images})
		}
		t.queued = append(back, t.queued...)
	})
}

// nextQueued pops the prompt to send now: only when the REPL is waiting for a
// line (replReady), no turn runs and the queue was not paused by an interrupt.
// Caller holds t.mu.
func (t *screen) nextQueued() (queuedPrompt, bool) {
	if len(t.queued) == 0 || t.busy || !t.replReady || t.partial != "" || t.queuePaused {
		return queuedPrompt{}, false
	}
	q := t.queued[0]
	t.queued = t.queued[1:]
	return q, true
}

// drawDock paints "N queued · <first prompt>" above the prompt box: ┃ in
// border.base on raised.base, as OpenCode's queued dock. A click opens the
// queued prompts dialog. Caller holds t.mu.
func (t *screen) drawDock(sb *strings.Builder, f frame) {
	if len(t.queued) == 0 {
		return
	}
	width := f.boxWidth - f.barCol + 1
	lead := strings.Repeat(" ", f.barCol-1)
	edge := color(fgBorder, bar)
	label := strconv.Itoa(len(t.queued)) + " queued · "
	first := oneLine(t.queued[0].sent, max(10, width-visWidth(label)-6))
	if t.queuePaused {
		label = strconv.Itoa(len(t.queued)) + " queued (paused: enter sends) · "
	}
	mid := edge + "  " + grayf(label) + color(colorText, first)
	top := f.dockRow
	rows := []string{edge, mid, edge}
	for i, r := range rows {
		line := lead + withBg(r, bgRaised, width)
		writeRow(sb, top+i, line)
		t.hits.add(&hitRegion{row: top + i, col: f.barCol, width: width, height: 1, id: "dock",
			plain: line, hover: lead + withBg(r, bgBox, width),
			action: func() { t.openQueueDialog(); t.drawLocked() }})
	}
}

// writeRow paints a full row over the screen background.
func writeRow(sb *strings.Builder, row int, s string) {
	sb.WriteString("\033[" + strconv.Itoa(row) + ";1H" + bgScreen + s + escClearLine)
}

// openQueueDialog lists the queued prompts; choosing one removes it from the
// queue (it has not been sent, so nothing else to undo).
func (t *screen) openQueueDialog() {
	var items []dialogItem
	for i, q := range t.queued {
		items = append(items, dialogItem{title: oneLine(q.sent, 70), desc: "remove from queue", value: strconv.Itoa(i)})
	}
	t.openDialog(&dialog{title: "Queued prompts", placeholder: "Search", size: dialogWide, items: items,
		choose: func(t *screen, it dialogItem) string {
			i, err := strconv.Atoi(it.value)
			if err == nil && i < len(t.queued) {
				t.queued = append(t.queued[:i], t.queued[i+1:]...)
				t.toastf("info", "Removed from the queue")
			}
			return ""
		}})
}

// Entregue is runtime.RendererSteer: a steer message reached the model.
func (r *screenTurn) Entregue(text string) { r.t.markDelivered(text) }
