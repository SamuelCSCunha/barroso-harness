package tools

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestKillsProtected(t *testing.T) {
	prot := []int{4242, 31}
	for cmd, want := range map[string]int{
		"kill 31":                       31,
		"kill -9 4242":                  4242,
		"echo x; kill -TERM 100 31":     31,
		"for p in 1; do kill -31; done": 31,
		"sleep 1 && kill -s KILL 4242":  4242,
	} {
		if got, ok := killsProtected(cmd, prot); !ok || got != want {
			t.Errorf("%q: got %d,%v want %d", cmd, got, ok, want)
		}
	}
	for _, cmd := range []string{"kill 100", "echo kill 31x", "skill 31", "ps | grep 31", "kill -9 1"} {
		if got, ok := killsProtected(cmd, prot); ok {
			t.Errorf("%q: unexpected hit %d", cmd, got)
		}
	}
}

func TestTerminalRunRefusesKillingItself(t *testing.T) {
	raw, _ := json.Marshal(map[string]string{"command": "kill -9 " + strconv.Itoa(os.Getpid())})
	out, err := specTerminalRun().Exec(context.Background(), ".", raw)
	if err != nil || !strings.HasPrefix(out, "refused:") {
		t.Fatalf("expected refusal, got %q, %v", out, err)
	}
}

func TestProtectedPIDsStartsWithSelf(t *testing.T) {
	p := ProtectedPIDs()
	if len(p) < 2 || p[0] != os.Getpid() || p[1] != os.Getppid() {
		t.Fatalf("got %v", p)
	}
}
