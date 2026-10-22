package tools

import (
	"context"
	"encoding/json"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func runTerm(t *testing.T, ctx context.Context, args map[string]any) string {
	t.Helper()
	b, _ := json.Marshal(args)
	out, err := specTerminalRun().Exec(ctx, t.TempDir(), b)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// sleepCmd is a command that stays silent for secs seconds.
func sleepCmd(secs int) string {
	if runtime.GOOS == "windows" {
		return "ping -n " + itoa(secs+1) + " 127.0.0.1 >nul"
	}
	return "sleep " + itoa(secs)
}

func itoa(n int) string { return strconv.Itoa(n) }

func shrinkJobs(t *testing.T) {
	t.Helper()
	ab, ia := autoBackground, inactivityTimeout
	t.Cleanup(func() { autoBackground, inactivityTimeout = ab, ia; KillJobs() })
}

func TestTerminalRunQuickCommandUnchanged(t *testing.T) {
	out := runTerm(t, context.Background(), map[string]any{"command": "echo hello"})
	if !strings.HasPrefix(out, "exit_code: 0\nstdout+stderr:\n") || !strings.Contains(out, "hello") {
		t.Fatalf("%q", out)
	}
}

// A long command returns control after autoBackground and keeps running;
// terminal_output then waits for it and reports the exit code.
func TestTerminalRunAutoBackground(t *testing.T) {
	shrinkJobs(t)
	autoBackground, inactivityTimeout = 300*time.Millisecond, time.Minute
	start := time.Now()
	out := runTerm(t, context.Background(), map[string]any{"command": sleepCmd(2)})
	if time.Since(start) > 1500*time.Millisecond || !strings.Contains(out, "moved to the background") {
		t.Fatalf("took %v: %s", time.Since(start), out)
	}
	id := regexp.MustCompile(`job-\d+`).FindString(out)
	b, _ := json.Marshal(map[string]any{"job_id": id, "wait_secs": 30})
	res, err := specTerminalOutput().Exec(context.Background(), "", b)
	if err != nil || !strings.Contains(res, "finished") || !strings.Contains(res, "exit_code: 0") {
		t.Fatalf("err=%v\n%s", err, res)
	}
}

// Silence past inactivityTimeout kills the tree and returns exit 124.
func TestTerminalRunInactivityKill(t *testing.T) {
	shrinkJobs(t)
	autoBackground, inactivityTimeout = time.Minute, 500*time.Millisecond
	start := time.Now()
	out := runTerm(t, context.Background(), map[string]any{"command": sleepCmd(20)})
	if time.Since(start) > 10*time.Second || !strings.Contains(out, "no output for") {
		t.Fatalf("took %v: %s", time.Since(start), out)
	}
}

func TestTerminalRunExplicitTimeout(t *testing.T) {
	shrinkJobs(t)
	out := runTerm(t, context.Background(), map[string]any{"command": sleepCmd(20), "timeout_secs": 1})
	if !strings.Contains(out, "timed out after 1s") || !strings.HasPrefix(out, "exit_code: 124") && !strings.HasPrefix(out, "exit_code: 1") {
		t.Fatalf("%s", out)
	}
}

func TestTerminalRunCancelKills(t *testing.T) {
	shrinkJobs(t)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)
	start := time.Now()
	out := runTerm(t, ctx, map[string]any{"command": sleepCmd(20)})
	if time.Since(start) > 10*time.Second || !strings.Contains(out, "cancelled") {
		t.Fatalf("took %v: %s", time.Since(start), out)
	}
}

func TestTerminalRunBackgroundAndKill(t *testing.T) {
	shrinkJobs(t)
	out := runTerm(t, context.Background(), map[string]any{"command": sleepCmd(30), "background": true})
	id := regexp.MustCompile(`job-\d+`).FindString(out)
	if id == "" {
		t.Fatal(out)
	}
	b, _ := json.Marshal(map[string]any{"job_id": id})
	res, err := specTerminalKill().Exec(context.Background(), "", b)
	if err != nil || !strings.Contains(res, "killed by terminal_kill") {
		t.Fatalf("err=%v\n%s", err, res)
	}
}

// Output past the cap keeps head and tail and saves everything to a file.
func TestTerminalRunSpillsLargeOutput(t *testing.T) {
	o := newJobOutput(nil)
	line := strings.Repeat("x", 99) + "\n"
	o.Write([]byte("FIRST\n"))
	for i := 0; i < 3000; i++ {
		o.Write([]byte(line))
	}
	o.Write([]byte("LAST\n"))
	s := o.render(true)
	if !strings.HasPrefix(s, "FIRST") || !strings.HasSuffix(s, "LAST\n") || !strings.Contains(s, "saved to ") || len(s) > outputCap+512 {
		t.Fatalf("len=%d", len(s))
	}
	b, err := os.ReadFile(o.path)
	if err != nil || len(b) != int(o.total) {
		t.Fatalf("file: %v %d/%d", err, len(b), o.total)
	}
	os.Remove(o.path)
}

// Every size between "fits whole" and "spilled" renders without panicking.
func TestJobOutputBoundarySizes(t *testing.T) {
	for _, n := range []int{0, 1, headKeep, headKeep + 1, headKeep + tailKeep, headKeep + tailKeep + 1, outputCap, outputCap + 1} {
		o := newJobOutput(nil)
		o.Write([]byte(strings.Repeat("a", n)))
		s := o.render(true)
		if n <= headKeep+tailKeep && len(s) != n {
			t.Fatalf("n=%d got %d", n, len(s))
		}
		if o.path != "" {
			os.Remove(o.path)
		}
	}
}
