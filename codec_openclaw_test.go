package agenthooks

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestDecodeOpenClawBeforeToolCall(t *testing.T) {
	typed, err := decodeOpenClawLine(VariantUnknown, DetectionConfig, testNow, fixture(t, "openclaw/before_tool_call.json"))
	if err != nil {
		t.Fatal(err)
	}
	ev, ok := typed.(*ToolPreEvent)
	if !ok {
		t.Fatalf("decoded %T, want *ToolPreEvent", typed)
	}
	if ev.Kind != KindToolPre || ev.NativeName != "before_tool_call" || ev.Provider != ProviderOpenClaw {
		t.Errorf("envelope wrong: %+v", ev.Event)
	}
	if ev.Session.ID != "oclaw-sess-1" || ev.Session.TurnID != "run-oclaw-1" {
		t.Errorf("session wrong: %+v", ev.Session)
	}
	if ev.Tool.Name != "exec" || ev.Tool.Canonical != ToolShell || ev.Tool.ID != "toolu_01ABC" || ev.Tool.Synthesized {
		t.Errorf("tool wrong: %+v", ev.Tool)
	}
	var params struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(ev.Tool.Input, &params); err != nil || params.Command != "ls -la" {
		t.Errorf("input wrong: %s (%v)", ev.Tool.Input, err)
	}
}

func TestDecodeOpenClawAfterToolCall(t *testing.T) {
	typed, err := decodeOpenClawLine(VariantUnknown, DetectionConfig, testNow, fixture(t, "openclaw/after_tool_call.json"))
	if err != nil {
		t.Fatal(err)
	}
	ev, ok := typed.(*ToolPostEvent)
	if !ok {
		t.Fatalf("decoded %T, want *ToolPostEvent", typed)
	}
	if ev.Kind != KindToolPost || ev.Failed || ev.Tool.ID != "toolu_01ABC" {
		t.Errorf("event wrong: kind=%s failed=%v tool=%+v", ev.Kind, ev.Failed, ev.Tool)
	}
	if ev.DurationMS == nil || *ev.DurationMS != 626 {
		t.Errorf("duration wrong: %v", ev.DurationMS)
	}
	if !strings.Contains(string(ev.Output), "total 40") {
		t.Errorf("output missing tool result: %s", ev.Output)
	}
}

func TestDecodeOpenClawPromptAndStop(t *testing.T) {
	typed, err := decodeOpenClawLine(VariantUnknown, DetectionConfig, testNow, fixture(t, "openclaw/before_agent_run.json"))
	if err != nil {
		t.Fatal(err)
	}
	pe, ok := typed.(*PromptEvent)
	if !ok {
		t.Fatalf("decoded %T, want *PromptEvent", typed)
	}
	if pe.Kind != KindPromptSubmitted || !strings.HasPrefix(pe.Prompt, "Use your exec tool") {
		t.Errorf("prompt wrong: %+v", pe)
	}
	if pe.Session.CWD != "/work/agent" || pe.Session.Model != "claude-opus-4-8" {
		t.Errorf("conversation ctx not lifted: %+v", pe.Session)
	}

	typed, err = decodeOpenClawLine(VariantUnknown, DetectionConfig, testNow, fixture(t, "openclaw/agent_end.json"))
	if err != nil {
		t.Fatal(err)
	}
	st, ok := typed.(*StopEvent)
	if !ok {
		t.Fatalf("decoded %T, want *StopEvent", typed)
	}
	if st.Kind != KindStop || st.FinalMessage != "Here is the ls output." {
		t.Errorf("stop wrong: %+v", st)
	}
	switch {
	case st.Usage == nil:
		t.Error("usage missing")
	case st.Usage.OutputTokens == nil || *st.Usage.OutputTokens != 137:
		t.Errorf("output tokens wrong: %+v", st.Usage.OutputTokens)
	case st.Usage.CacheReadTokens == nil || *st.Usage.CacheReadTokens != 68543:
		t.Errorf("cache read tokens wrong: %+v", st.Usage.CacheReadTokens)
	}
}

func TestDecodeOpenClawSessionLifecycle(t *testing.T) {
	typed, err := decodeOpenClawLine(VariantUnknown, DetectionConfig, testNow, fixture(t, "openclaw/session_start.json"))
	if err != nil {
		t.Fatal(err)
	}
	ss, ok := typed.(*SessionStartEvent)
	if !ok {
		t.Fatalf("decoded %T, want *SessionStartEvent", typed)
	}
	if ss.Session.ID != "oclaw-sess-1" || ss.Source != "resume" {
		t.Errorf("session start wrong: %+v", ss)
	}

	typed, err = decodeOpenClawLine(VariantUnknown, DetectionConfig, testNow, fixture(t, "openclaw/session_end.json"))
	if err != nil {
		t.Fatal(err)
	}
	se, ok := typed.(*SessionEndEvent)
	if !ok {
		t.Fatalf("decoded %T, want *SessionEndEvent", typed)
	}
	if se.Reason != "idle" {
		t.Errorf("session end wrong: %+v", se)
	}
}

func TestDecodeOpenClawModelResponse(t *testing.T) {
	typed, err := decodeOpenClawLine(VariantUnknown, DetectionConfig, testNow, fixture(t, "openclaw/llm_output.json"))
	if err != nil {
		t.Fatal(err)
	}
	me, ok := typed.(*ModelEvent)
	if !ok {
		t.Fatalf("decoded %T, want *ModelEvent", typed)
	}
	if me.Kind != KindModelResponse || me.Session.TurnID != "run-oclaw-1" {
		t.Errorf("model event wrong: %+v", me)
	}
}

func TestEncodeOpenClawDeny(t *testing.T) {
	base := &Event{Provider: ProviderOpenClaw, Kind: KindToolPre}
	st := newOpenclawServeState()
	reply, err := encodeOpenClawReply(base, decisionCore{kind: DecisionDeny, reason: "no"}, st, "toolu_01ABC")
	if err != nil {
		t.Fatal(err)
	}
	if reply.Output["block"] != true || reply.Output["blockReason"] != "no" {
		t.Errorf("deny reply wrong: %+v", reply.Output)
	}
	if st.blockedCalls["toolu_01ABC"] != "no" {
		t.Errorf("blocked call not tracked: %+v", st.blockedCalls)
	}
}

func TestEncodeOpenClawAskAndUpdate(t *testing.T) {
	base := &Event{Provider: ProviderOpenClaw, Kind: KindToolPre}
	reply, err := encodeOpenClawReply(base, decisionCore{
		kind: DecisionAsk, reason: "confirm this",
		hasUpdatedInput: true, updatedInput: map[string]any{"command": "ls"},
	}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	ra, ok := reply.Output["requireApproval"].(map[string]any)
	if !ok || ra["timeoutBehavior"] != "deny" || ra["description"] != "confirm this" {
		t.Errorf("ask reply wrong: %+v", reply.Output)
	}
	if params, ok := reply.Output["params"].(map[string]any); !ok || params["command"] != "ls" {
		t.Errorf("params rewrite missing: %+v", reply.Output)
	}
}

func TestEncodeOpenClawBlockPrompt(t *testing.T) {
	base := &Event{Provider: ProviderOpenClaw, Kind: KindPromptSubmitted}
	reply, err := encodeOpenClawReply(base, decisionCore{kind: DecisionBlockPrompt, reason: "policy"}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if reply.Output["outcome"] != "block" || reply.Output["reason"] != "policy" || reply.Output["message"] != "policy" {
		t.Errorf("prompt gate reply wrong: %+v", reply.Output)
	}
}

func TestOpenClawBlockedCallFlipsAfterToolCall(t *testing.T) {
	st := newOpenclawServeState()
	st.blockedCalls["toolu_01ABC"] = "no exec here"
	var fr openclawFrame
	raw := fixture(t, "openclaw/after_tool_call_blocked.json")
	if err := json.Unmarshal(raw, &fr); err != nil {
		t.Fatal(err)
	}
	typed, err := decodeOpenClawFrame(VariantUnknown, DetectionConfig, testNow, &fr, raw, st)
	if err != nil {
		t.Fatal(err)
	}
	ev := typed.(*ToolPostEvent)
	if !ev.Failed || !strings.Contains(ev.Error, "no exec here") {
		t.Errorf("blocked after_tool_call must decode as failed: failed=%v error=%q", ev.Failed, ev.Error)
	}
	if ev.Kind != KindToolError {
		t.Errorf("blocked sibling must carry Kind tool.error, got %s", ev.Kind)
	}
	if len(st.blockedCalls) != 0 {
		t.Errorf("blocked marker should be consumed: %+v", st.blockedCalls)
	}
}

func TestOpenClawGateTimeoutNoticeMarksCallBlocked(t *testing.T) {
	// A shim that fail-closed locally reports the call via a gate_timeout
	// frame; the serve loop must then decode the after_tool_call sibling as
	// blocked even though no deny decision was ever produced here.
	r := quietRunner()
	var kinds []EventKind
	var errs []string
	r.OnToolError(func(ctx context.Context, e *ToolPostEvent) (ToolPostDecision, error) {
		kinds = append(kinds, e.Kind)
		errs = append(errs, e.Error)
		return ToolPostDecision{}, nil
	})
	lines := []string{
		`{"seq":1,"hook":"gate_timeout","event":{"toolCallId":"toolu_01ABC","reason":"agenthooks: hook timed out (fail-closed)"}}`,
		strings.TrimSpace(string(fixture(t, "openclaw/after_tool_call_blocked.json"))),
	}
	var out, errb bytes.Buffer
	code := r.Run(context.Background(), []string{"agenthooks", "serve", "--provider=openclaw"},
		strings.NewReader(strings.Join(lines, "\n")+"\n"), &out, &errb)
	if code != 0 {
		t.Fatalf("serve exit %d, stderr: %s", code, errb.String())
	}
	replies := parseReplies(t, out.String())
	if len(replies) != 2 || replies[0].Seq != 1 {
		t.Fatalf("gate_timeout must be acknowledged: %+v", replies)
	}
	if len(kinds) != 1 || kinds[0] != KindToolError || !strings.Contains(errs[0], "timed out") {
		t.Fatalf("after_tool_call of a shim-blocked call must dispatch as tool.error: kinds=%v errs=%v", kinds, errs)
	}
}

func TestOpenClawServeStateBackfillsCWD(t *testing.T) {
	st := newOpenclawServeState()
	for _, name := range []string{"openclaw/before_agent_run.json", "openclaw/before_tool_call.json"} {
		var fr openclawFrame
		raw := fixture(t, name)
		if err := json.Unmarshal(raw, &fr); err != nil {
			t.Fatal(err)
		}
		typed, err := decodeOpenClawFrame(VariantUnknown, DetectionConfig, testNow, &fr, raw, st)
		if err != nil {
			t.Fatal(err)
		}
		base := eventOf(typed)
		if base.Session.CWD != "/work/agent" || base.Session.Model != "claude-opus-4-8" {
			t.Errorf("%s: cwd/model not carried: %+v", name, base.Session)
		}
	}
}

func TestDetectOpenClawShape(t *testing.T) {
	p, ok := detectFromShape(fixture(t, "openclaw/before_tool_call.json"))
	if !ok || p != ProviderOpenClaw {
		t.Errorf("openclaw frame detected as %q", p)
	}
	p, ok = detectFromShape(fixture(t, "opencode/tool_execute_before.json"))
	if !ok || p != ProviderOpenCode {
		t.Errorf("opencode frame detected as %q", p)
	}
}
