package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/subagent"
)

// managerWithTask builds a real manager with ONE task that fails right away
// (incomplete child config) — no network, no provider, deterministic.
func managerWithTask(t *testing.T) (*subagent.Gerente, string) {
	t.Helper()
	g := subagent.Novo(subagent.Config{})
	g.Vincular(context.Background())
	g.ConfigPai(config.Config{})
	id, err := g.Spawn(context.Background(), "investigate the date parser bug")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	return g, id
}

// A nil manager now only exists on a path WITHOUT wiring (Montar always
// creates one); the message explains the situation instead of asking to edit config.
func TestTasksWithoutManagerExplains(t *testing.T) {
	noColors(t)
	var out bytes.Buffer
	cmdTasks(&out, nil, "")
	if !strings.Contains(out.String(), "sub-agents") {
		t.Fatalf("should explain that sub-agents are unavailable: %q", out.String())
	}
}

func TestTasksEmptySaysSo(t *testing.T) {
	noColors(t)
	var out bytes.Buffer
	cmdTasks(&out, subagent.Novo(subagent.Config{}), "")
	if !strings.Contains(out.String(), "no tasks") {
		t.Fatalf("output: %q", out.String())
	}
}

func TestTasksListsStateAndPrompt(t *testing.T) {
	noColors(t)
	g, id := managerWithTask(t)
	var out bytes.Buffer
	cmdTasks(&out, g, "")
	s := out.String()
	for _, want := range []string{id, subagent.Failed, "investigate the", "1 failed"} {
		if !strings.Contains(s, want) {
			t.Errorf("listing missing %q:\n%s", want, s)
		}
	}
}

func TestTasksDetailWithError(t *testing.T) {
	noColors(t)
	g, id := managerWithTask(t)
	var out bytes.Buffer
	cmdTasks(&out, g, id)
	s := out.String()
	if !strings.Contains(s, "prompt: investigate the date parser bug") {
		t.Errorf("detail missing the prompt:\n%s", s)
	}
	if !strings.Contains(s, "error: ") {
		t.Errorf("detail missing the error:\n%s", s)
	}
}

func TestTasksDetailUnknownID(t *testing.T) {
	noColors(t)
	g, _ := managerWithTask(t)
	var out bytes.Buffer
	cmdTasks(&out, g, "t99")
	if !strings.Contains(out.String(), "does not exist") {
		t.Fatalf("output: %q", out.String())
	}
}

// Canceling an already finished task must not pretend to succeed.
func TestTasksCancelFinishedWarns(t *testing.T) {
	noColors(t)
	g, id := managerWithTask(t)
	var out bytes.Buffer
	cmdTasks(&out, g, "cancel "+id)
	if !strings.Contains(out.String(), "nothing to cancel") {
		t.Fatalf("output: %q", out.String())
	}
	out.Reset()
	cmdTasks(&out, g, "cancel")
	if !strings.Contains(out.String(), "usage: /tasks cancel") {
		t.Fatalf("without an id it should show usage: %q", out.String())
	}
}

func TestReplHelpListsTasks(t *testing.T) {
	noColors(t)
	io, out, _ := bridgeIO(t, "")
	printHelp(io)
	if !strings.Contains(out.String(), "/tasks") {
		t.Fatalf("/help missing /tasks:\n%s", out.String())
	}
}
