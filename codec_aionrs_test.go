package agenthooks

import (
	"encoding/json"
	"testing"
	"time"
)

func TestDecodeAionrsLifecycle(t *testing.T) {
	now := time.Unix(42, 0)
	preRaw := []byte(`{"event":"PreToolUse","session_id":"conv-1","turn_id":"turn-1","cwd":"/work","tool_use_id":"call-1","tool_name":"tra_capability","tool_input":{"capability":"local"}}`)
	typed, err := decodeAionrs(VariantUnknown, DetectionConfig, now, preRaw)
	if err != nil {
		t.Fatal(err)
	}
	pre, ok := typed.(*ToolPreEvent)
	if !ok {
		t.Fatalf("decoded %T, want *ToolPreEvent", typed)
	}
	if pre.Provider != ProviderAionrs || pre.Kind != KindToolPre || pre.Session.ID != "conv-1" || pre.Session.TurnID != "turn-1" {
		t.Fatalf("unexpected event: %+v", pre.Event)
	}
	if pre.Tool.ID != "call-1" || pre.Tool.Name != "tra_capability" || !json.Valid(pre.Tool.Input) {
		t.Fatalf("unexpected tool: %+v", pre.Tool)
	}

	postRaw := []byte(`{"event":"PostToolUse","session_id":"conv-1","turn_id":"turn-1","tool_use_id":"call-1","tool_name":"tra_capability","tool_input":{},"tool_output":{"ok":false},"failed":true}`)
	typed, err = decodeAionrs(VariantUnknown, DetectionConfig, now, postRaw)
	if err != nil {
		t.Fatal(err)
	}
	post, ok := typed.(*ToolPostEvent)
	if !ok || post.Kind != KindToolError || !post.Failed {
		t.Fatalf("unexpected post event: %#v", typed)
	}

	promptRaw := []byte(`{"event":"PromptSubmitted","session_id":"conv-1","turn_id":"turn-2","prompt":"use TRA"}`)
	typed, err = decodeAionrs(VariantUnknown, DetectionConfig, now, promptRaw)
	if err != nil {
		t.Fatal(err)
	}
	prompt, ok := typed.(*PromptEvent)
	if !ok || prompt.Prompt != "use TRA" {
		t.Fatalf("unexpected prompt event: %#v", typed)
	}

	stopRaw := []byte(`{"event":"Stop","session_id":"conv-1","turn_id":"turn-2","final_message":"done"}`)
	typed, err = decodeAionrs(VariantUnknown, DetectionConfig, now, stopRaw)
	if err != nil {
		t.Fatal(err)
	}
	stop, ok := typed.(*StopEvent)
	if !ok || stop.FinalMessage != "done" {
		t.Fatalf("unexpected stop event: %#v", typed)
	}
}

func TestEncodeAionrsPromptContextAndToolBlock(t *testing.T) {
	prompt := &PromptEvent{Event: Event{Provider: ProviderAionrs, Kind: KindPromptSubmitted}}
	wire, err := encodeAionrs(&prompt.Event, decisionCore{kind: DecisionAcceptPrompt, context: []string{"TRA contract"}})
	if err != nil {
		t.Fatal(err)
	}
	if string(wire.Stdout) != "TRA contract" || wire.ExitCode != 0 {
		t.Fatalf("unexpected prompt response: %#v", wire)
	}

	pre := &ToolPreEvent{Event: Event{Provider: ProviderAionrs, Kind: KindToolPre}}
	wire, err = encodeAionrs(&pre.Event, decisionCore{kind: DecisionDeny, reason: "blocked"})
	if err != nil {
		t.Fatal(err)
	}
	if wire.ExitCode != 1 || string(wire.Stderr) != "blocked" {
		t.Fatalf("unexpected block response: %#v", wire)
	}
}

func TestAionrsCapabilitiesAreHonest(t *testing.T) {
	if !Capabilities(ProviderAionrs, VariantUnknown, KindPromptSubmitted).Has(CapAddContext) {
		t.Fatal("prompt context capability missing")
	}
	if !Capabilities(ProviderAionrs, VariantUnknown, KindToolPre).Has(CapDeny) {
		t.Fatal("tool deny capability missing")
	}
	if Capabilities(ProviderAionrs, VariantUnknown, KindStop).Has(CapContinueAgent) {
		t.Fatal("aionrs stop is observation-only")
	}
}

func TestAionrsPayloadFromEnvironment(t *testing.T) {
	t.Setenv("AIONUI_CONVERSATION_ID", "conv-env")
	t.Setenv("AIONRS_TURN_ID", "turn-env")
	t.Setenv("AIONRS_MSG_ID", "msg-env")
	t.Setenv("AIONRS_PROMPT", "prompt text")
	t.Setenv("AIONRS_TOOL_USE_ID", "call-env")
	t.Setenv("AIONRS_TOOL_ERROR", "true")
	t.Setenv("TOOL_NAME", "tra_capability")
	t.Setenv("TOOL_INPUT", `{"capability":"local"}`)
	t.Setenv("TOOL_OUTPUT", "plain output")

	payload, err := aionrsPayloadFromEnv("PostToolUse")
	if err != nil {
		t.Fatal(err)
	}
	var decoded aionrsIn
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.SessionID != "conv-env" || decoded.TurnID != "turn-env" || decoded.ToolUseID != "call-env" || !decoded.Failed {
		t.Fatalf("unexpected environment projection: %+v", decoded)
	}
	var output string
	if err := json.Unmarshal(decoded.ToolOutput, &output); err != nil || output != "plain output" {
		t.Fatalf("plain output was not JSON wrapped: %q (%v)", decoded.ToolOutput, err)
	}
}
