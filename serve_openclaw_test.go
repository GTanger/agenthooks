package agenthooks

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestServeOpenClawLoop(t *testing.T) {
	r := quietRunner()
	r.OnToolPre(func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
		if e.Tool.Canonical == ToolShell {
			return Deny("no shell in this session"), nil
		}
		return NoDecision(), nil
	})

	lines := []string{
		strings.TrimSpace(string(fixture(t, "openclaw/session_start.json"))),    // seq 1
		strings.TrimSpace(string(fixture(t, "openclaw/before_agent_run.json"))), // seq 2
		strings.TrimSpace(string(fixture(t, "openclaw/before_tool_call.json"))), // seq 3, exec -> deny
		strings.TrimSpace(string(fixture(t, "openclaw/after_tool_call.json"))),  // seq 4, blocked sibling
		strings.TrimSpace(string(fixture(t, "openclaw/session_end.json"))),      // seq 9
	}
	var out, errb bytes.Buffer
	code := r.Run(context.Background(), []string{"agenthooks", "serve", "--provider=openclaw"},
		strings.NewReader(strings.Join(lines, "\n")+"\n"), &out, &errb)
	if code != 0 {
		t.Fatalf("serve exit %d, stderr: %s", code, errb.String())
	}

	replies := parseReplies(t, out.String())
	if len(replies) != 5 {
		t.Fatalf("expected 5 replies, got %d: %s", len(replies), out.String())
	}
	if replies[0].Seq != 1 || replies[0].Error != "" || len(replies[0].Output) != 0 {
		t.Errorf("session_start no-op wrong: %+v", replies[0])
	}
	deny := replies[2]
	if deny.Seq != 3 || deny.Output["block"] != true || deny.Output["blockReason"] != "no shell in this session" {
		t.Errorf("deny must ride output.block (hook return value): %+v", deny)
	}
	if replies[3].Seq != 4 || len(replies[3].Output) != 0 {
		t.Errorf("after_tool_call is observe-only: %+v", replies[3])
	}
}

func TestServeOpenClawBlockedCallReachesToolError(t *testing.T) {
	r := quietRunner()
	r.OnToolPre(func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
		return Deny("blocked by test"), nil
	})
	var failed []string
	r.OnToolError(func(ctx context.Context, e *ToolPostEvent) (ToolPostDecision, error) {
		failed = append(failed, e.Error)
		return ToolPostDecision{}, nil
	})

	lines := []string{
		strings.TrimSpace(string(fixture(t, "openclaw/before_tool_call.json"))),
		strings.TrimSpace(string(fixture(t, "openclaw/after_tool_call.json"))),
	}
	var out, errb bytes.Buffer
	code := r.Run(context.Background(), []string{"agenthooks", "serve", "--provider=openclaw"},
		strings.NewReader(strings.Join(lines, "\n")+"\n"), &out, &errb)
	if code != 0 {
		t.Fatalf("serve exit %d, stderr: %s", code, errb.String())
	}
	if len(failed) != 1 || !strings.Contains(failed[0], "blocked by test") {
		t.Fatalf("after_tool_call of a denied call must dispatch as tool.error: %v", failed)
	}
}

func TestServeOpenClawPromptGate(t *testing.T) {
	r := quietRunner()
	r.OnPromptSubmitted(func(ctx context.Context, e *PromptEvent) (PromptDecision, error) {
		return BlockPrompt("prompt policy"), nil
	})
	var out, errb bytes.Buffer
	line := strings.TrimSpace(string(fixture(t, "openclaw/before_agent_run.json")))
	code := r.Run(context.Background(), []string{"agenthooks", "serve", "--provider=openclaw"},
		strings.NewReader(line+"\n"), &out, &errb)
	if code != 0 {
		t.Fatalf("serve exit %d, stderr: %s", code, errb.String())
	}
	replies := parseReplies(t, out.String())
	if len(replies) != 1 || replies[0].Output["outcome"] != "block" || replies[0].Output["reason"] != "prompt policy" {
		t.Errorf("prompt gate reply wrong: %+v", replies)
	}
}
