package agenthooks

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
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

func TestServeOpenClawFrameDeadlineBoundsHandler(t *testing.T) {
	// A gate frame carries the shim's per-hook deadline; a handler that
	// outlives it must not have its late decision honored (fail-open default:
	// the timed-out gate resolves to no decision).
	r := quietRunner()
	r.OnToolPre(func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
		select {
		case <-time.After(2 * time.Second):
			return Deny("too late to matter"), nil
		case <-ctx.Done():
			return NoDecision(), ctx.Err()
		}
	})

	var fr map[string]any
	if err := json.Unmarshal(fixture(t, "openclaw/before_tool_call.json"), &fr); err != nil {
		t.Fatal(err)
	}
	fr["timeoutMs"] = 100
	line, err := json.Marshal(fr)
	if err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	start := time.Now()
	code := r.Run(context.Background(), []string{"agenthooks", "serve", "--provider=openclaw"},
		strings.NewReader(string(line)+"\n"), &out, &errb)
	if code != 0 {
		t.Fatalf("serve exit %d, stderr: %s", code, errb.String())
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("frame deadline not honored: dispatch took %s", elapsed)
	}
	replies := parseReplies(t, out.String())
	if len(replies) != 1 || replies[0].Output["block"] == true {
		t.Fatalf("timed-out gate must not carry the late deny: %+v", replies)
	}
}

func TestServeOpenClawObserveBackpressureDoesNotBlockGates(t *testing.T) {
	// More observe frames than any fixed buffer, each with a slow handler:
	// the loop must still acknowledge every frame and answer the trailing
	// gate without waiting for the telemetry backlog to drain.
	r := quietRunner()
	r.OnSessionEnd(func(ctx context.Context, e *SessionEndEvent) error {
		time.Sleep(time.Millisecond)
		return nil
	})
	r.OnToolPre(func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
		return Deny("gated"), nil
	})

	const observeFrames = 300
	sessionEnd := strings.TrimSpace(string(fixture(t, "openclaw/session_end.json")))
	lines := make([]string, 0, observeFrames+1)
	for i := 0; i < observeFrames; i++ {
		lines = append(lines, sessionEnd)
	}
	lines = append(lines, strings.TrimSpace(string(fixture(t, "openclaw/before_tool_call.json"))))

	var out, errb bytes.Buffer
	code := r.Run(context.Background(), []string{"agenthooks", "serve", "--provider=openclaw"},
		strings.NewReader(strings.Join(lines, "\n")+"\n"), &out, &errb)
	if code != 0 {
		t.Fatalf("serve exit %d, stderr: %s", code, errb.String())
	}
	replies := parseReplies(t, out.String())
	if len(replies) != observeFrames+1 {
		t.Fatalf("expected %d replies, got %d", observeFrames+1, len(replies))
	}
	last := replies[len(replies)-1]
	if last.Output["block"] != true || last.Output["blockReason"] != "gated" {
		t.Fatalf("gate behind observe backlog must still decide: %+v", last)
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
