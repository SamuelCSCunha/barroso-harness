package runtime

// Prompt hooks: commands that run before each user prompt and add context to
// it, so a recall the model must remember to do (and often does not) happens
// on every turn instead. Same contract as Claude Code's UserPromptSubmit, so
// one script serves both harnesses.

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"time"

	"github.com/ruanbarroso/barroso-harness/internal/config"
)

const (
	promptHookTimeout = 10 * time.Second
	// promptHookMax caps what one hook may add to a prompt.
	promptHookMax = 32 << 10
)

// promptContext runs the configured hooks and returns what they add to the
// prompt ("" when none is set or none answered). Only persistent sessions run
// them: subagents and one-shot sessions get their context from the caller.
func (s *Session) promptContext(ctx context.Context, cwd, prompt string) string {
	if len(s.config.PromptHooks) == 0 || !s.Persistente() {
		return ""
	}
	in, err := json.Marshal(map[string]string{
		"hook_event_name": "UserPromptSubmit",
		"prompt":          prompt,
		"cwd":             cwd,
		"session_id":      s.sessionID,
	})
	if err != nil {
		return ""
	}
	var parts []string
	for _, h := range s.config.PromptHooks {
		if out := runPromptHook(ctx, h, cwd, in); out != "" {
			parts = append(parts, out)
		}
	}
	return strings.Join(parts, "\n\n")
}

// withHookContext is the user message the model sees: the prompt, then the
// hooks' context after a blank line.
func withHookContext(prompt, hookCtx string) string {
	if hookCtx == "" {
		return prompt
	}
	return prompt + "\n\n" + hookCtx
}

// runPromptHook runs one hook. Any failure - missing command, non-zero exit,
// timeout - yields "": a hook adds context, it never blocks the turn.
func runPromptHook(ctx context.Context, h config.PromptHook, cwd string, in []byte) string {
	if strings.TrimSpace(h.Command) == "" {
		return ""
	}
	limit := promptHookTimeout
	if h.Timeout > 0 {
		limit = time.Duration(h.Timeout) * time.Second
	}
	hctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	cmd := exec.CommandContext(hctx, h.Command, h.Args...)
	cmd.Dir = cwd
	cmd.Stdin = bytes.NewReader(in)
	hideWindow(cmd)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return hookContext(out)
}

// hookContext reads a hook's stdout: the Claude Code JSON shape when it
// parses, plain text otherwise.
func hookContext(out []byte) string {
	t := strings.TrimSpace(string(out))
	if t == "" {
		return ""
	}
	var j struct {
		HookSpecificOutput struct {
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if strings.HasPrefix(t, "{") && json.Unmarshal([]byte(t), &j) == nil {
		t = strings.TrimSpace(j.HookSpecificOutput.AdditionalContext)
	}
	if len(t) > promptHookMax {
		t = t[:promptHookMax]
	}
	return t
}
