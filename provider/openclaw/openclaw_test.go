package openclaw

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/speakeasy-api/agenthooks"
)

func rawEvent(t *testing.T, name, native string) *agenthooks.Event {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "agenthookstest", "fixtures", "openclaw", name))
	if err != nil {
		t.Fatal(err)
	}
	return &agenthooks.Event{Provider: agenthooks.ProviderOpenClaw, NativeName: native, Raw: raw}
}

func TestTypedViews(t *testing.T) {
	if in, ok := BeforeToolCall(rawEvent(t, "before_tool_call.json", "before_tool_call")); !ok || in.ToolName != "exec" || in.ToolCallID != "toolu_01ABC" {
		t.Errorf("BeforeToolCall wrong: %+v ok=%v", in, ok)
	}
	if in, ok := AfterToolCall(rawEvent(t, "after_tool_call.json", "after_tool_call")); !ok || in.ToolName != "exec" || in.DurationMS == nil {
		t.Errorf("AfterToolCall wrong: %+v ok=%v", in, ok)
	}
	if in, ok := BeforeAgentRun(rawEvent(t, "before_agent_run.json", "before_agent_run")); !ok || in.Prompt == "" || in.SystemPrompt == "" {
		t.Errorf("BeforeAgentRun wrong: %+v ok=%v", in, ok)
	}
	in, ok := AgentEnd(rawEvent(t, "agent_end.json", "agent_end"))
	if !ok {
		t.Fatal("AgentEnd failed to decode")
	}
	if !in.Success || in.FinalMessage == "" {
		t.Errorf("AgentEnd wrong: %+v", in)
	}
	if in.Usage == nil || in.Usage.Output == nil || *in.Usage.Output != 137 {
		t.Errorf("AgentEnd usage wrong: %+v", in.Usage)
	}
	if in, ok := SessionStart(rawEvent(t, "session_start.json", "session_start")); !ok || in.SessionID != "oclaw-sess-1" || in.ResumedFrom == "" {
		t.Errorf("SessionStart wrong: %+v ok=%v", in, ok)
	}
	if in, ok := SessionEnd(rawEvent(t, "session_end.json", "session_end")); !ok || in.Reason != "idle" || in.NextSessionID == "" {
		t.Errorf("SessionEnd wrong: %+v ok=%v", in, ok)
	}
	out, ok := LlmOutput(rawEvent(t, "llm_output.json", "llm_output"))
	if !ok || out.HarnessID != "openclaw" || len(out.AssistantTexts) != 1 {
		t.Errorf("LlmOutput wrong: %+v ok=%v", out, ok)
	}
	ctx, ok := Context(rawEvent(t, "before_agent_run.json", "before_agent_run"))
	if !ok || ctx.WorkspaceDir != "/work/agent" || ctx.ModelID != "claude-opus-4-8" {
		t.Fatalf("Context wrong: %+v ok=%v", ctx, ok)
	}
	// Unknown-field capture: the fixture carries context fields the struct
	// does not declare; they must survive in Extra, not vanish.
	if _, found := ctx.Extra["modelProviderId"]; !found {
		t.Errorf("Context must capture unknown fields in Extra: %+v", ctx.Extra)
	}
	// A view must reject frames from a different hook.
	if _, ok := BeforeToolCall(rawEvent(t, "after_tool_call.json", "after_tool_call")); ok {
		t.Error("BeforeToolCall must reject other hooks")
	}
}
