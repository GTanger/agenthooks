package agenthooks

import (
	"encoding/json"
	"testing"
)

// Copilot omits the event name from every payload but permissionRequest and
// notification, so the shape reconstruction is the load-bearing part of the
// decoder: get it wrong and events are mislabelled in telemetry or, worse,
// gated with the wrong capability set.
func TestCopilotEventNamesFromShape(t *testing.T) {
	cases := map[string]struct {
		fixture string
		kind    EventKind
	}{
		"sessionStart":        {"copilot/session_start.json", KindSessionStart},
		"sessionEnd":          {"copilot/session_end.json", KindSessionEnd},
		"userPromptSubmitted": {"copilot/user_prompt_submitted.json", KindPromptSubmitted},
		"preToolUse":          {"copilot/pre_tool_use.json", KindToolPre},
		"postToolUse":         {"copilot/post_tool_use.json", KindToolPost},
		"postToolUseFailure":  {"copilot/post_tool_use_failure.json", KindToolError},
		"permissionRequest":   {"copilot/permission_request.json", KindPermission},
		"agentStop":           {"copilot/agent_stop.json", KindStop},
		"subagentStop":        {"copilot/subagent_stop.json", KindSubagentStop},
		"notification":        {"copilot/notification.json", KindNotification},
	}
	for want, tc := range cases {
		typed, err := decodeCopilot(VariantUnknown, DetectionConfig, testNow, fixture(t, tc.fixture))
		if err != nil {
			t.Fatalf("%s: %v", want, err)
		}
		ev := eventOf(typed)
		if ev.NativeName != want || ev.Kind != tc.kind {
			t.Errorf("%s decoded as native=%q kind=%q, want kind=%q", want, ev.NativeName, ev.Kind, tc.kind)
		}
		if ev.Session.ID != "sess-copilot-1" {
			t.Errorf("%s session id = %q", want, ev.Session.ID)
		}
	}
}

// toolArgs is a JSON-encoded string on pre/postToolUse and a plain object on
// permissionRequest; both must normalize to an object.
func TestCopilotToolArgsNormalize(t *testing.T) {
	typed, err := decodeCopilot(VariantUnknown, DetectionConfig, testNow, fixture(t, "copilot/pre_tool_use.json"))
	if err != nil {
		t.Fatal(err)
	}
	pre, ok := typed.(*ToolPreEvent)
	if !ok {
		t.Fatalf("decoded %T, want *ToolPreEvent", typed)
	}
	var args struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(pre.Tool.Input, &args); err != nil {
		t.Fatalf("tool input is not an object: %s", pre.Tool.Input)
	}
	if args.Command != "echo hello-from-gram" {
		t.Errorf("command = %q", args.Command)
	}
	if pre.Tool.Canonical != ToolShell || !pre.Tool.Synthesized {
		t.Errorf("tool = %+v; copilot ships no call id, so it must be synthesized", pre.Tool)
	}

	typed, err = decodeCopilot(VariantUnknown, DetectionConfig, testNow, fixture(t, "copilot/permission_request.json"))
	if err != nil {
		t.Fatal(err)
	}
	perm, ok := typed.(*PermissionEvent)
	if !ok {
		t.Fatalf("decoded %T, want *PermissionEvent", typed)
	}
	if err := json.Unmarshal(perm.Tool.Input, &args); err != nil || args.Command != "echo hello-from-gram" {
		t.Errorf("permission tool input = %s (%v)", perm.Tool.Input, err)
	}
}

// The rule the whole codec is built around: Copilot denies a tool call on ANY
// non-zero exit from a preToolUse command hook, so a deny must ride stdout
// with exit 0 — and so must a fail-closed handler failure.
func TestCopilotDenyExitsZero(t *testing.T) {
	typed, err := decodeCopilot(VariantUnknown, DetectionConfig, testNow, fixture(t, "copilot/pre_tool_use.json"))
	if err != nil {
		t.Fatal(err)
	}
	base := eventOf(typed)

	wire, err := encodeCopilot(base, decisionCore{kind: DecisionDeny, reason: "blocked by policy"})
	if err != nil {
		t.Fatal(err)
	}
	if wire.ExitCode != 0 {
		t.Fatalf("deny exit code = %d, want 0 (non-zero denies unconditionally and loses the reason)", wire.ExitCode)
	}
	var out map[string]any
	if err := json.Unmarshal(wire.Stdout, &out); err != nil {
		t.Fatalf("deny stdout %q: %v", wire.Stdout, err)
	}
	if out["permissionDecision"] != "deny" || out["permissionDecisionReason"] != "blocked by policy" {
		t.Errorf("deny body = %s", wire.Stdout)
	}

	// failCore under FailClosed is the credential-ratchet path; it must also
	// leave the exit code alone.
	fail := failCore(Policy{Fail: FailClosed}, base)
	if fail.kind != DecisionDeny {
		t.Fatalf("fail-closed core = %s, want deny", fail.kind)
	}
	wire, err = encodeCopilot(base, fail)
	if err != nil || wire.ExitCode != 0 {
		t.Fatalf("fail-closed wire = %+v (%v), want exit 0", wire, err)
	}
}

// permissionRequest speaks behavior/message, not permissionDecision, and the
// prompt event can express nothing at all (Copilot drops command-hook output
// for userPromptSubmitted).
func TestCopilotPerEventOutputSchemas(t *testing.T) {
	perm, err := decodeCopilot(VariantUnknown, DetectionConfig, testNow, fixture(t, "copilot/permission_request.json"))
	if err != nil {
		t.Fatal(err)
	}
	wire, err := encodeCopilot(eventOf(perm), decisionCore{kind: DecisionDeny, reason: "nope"})
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(wire.Stdout, &out); err != nil {
		t.Fatal(err)
	}
	if out["behavior"] != "deny" || out["message"] != "nope" {
		t.Errorf("permissionRequest body = %s", wire.Stdout)
	}

	if Capabilities(ProviderCopilot, VariantUnknown, KindPromptSubmitted).Has(CapDeny) {
		t.Error("prompt.submitted must not claim deny: copilot drops command-hook output for it")
	}

	stop, err := decodeCopilot(VariantUnknown, DetectionConfig, testNow, fixture(t, "copilot/agent_stop.json"))
	if err != nil {
		t.Fatal(err)
	}
	wire, err = encodeCopilot(eventOf(stop), decisionCore{kind: DecisionContinue, instruction: "keep going"})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(wire.Stdout, &out); err != nil {
		t.Fatal(err)
	}
	if out["decision"] != "block" || out["reason"] != "keep going" {
		t.Errorf("agentStop body = %s", wire.Stdout)
	}
}

func TestCopilotDetection(t *testing.T) {
	inv, err := parseArgs([]string{"agenthooks", "run", "--provider=copilot"})
	if err != nil || inv.provider != ProviderCopilot {
		t.Fatalf("--provider=copilot → %q (%v)", inv.provider, err)
	}
	t.Setenv("COPILOT_CLI", "1")
	t.Setenv("CLAUDE_PLUGIN_ROOT", "/tmp/plugin")
	if p, ok := detectFromEnv(); !ok || p != ProviderCopilot {
		t.Errorf("env detection = %q; copilot cross-sets CLAUDE_PLUGIN_ROOT and must win", p)
	}
	if p, ok := detectFromShape(fixture(t, "copilot/pre_tool_use.json")); !ok || p != ProviderCopilot {
		t.Errorf("shape detection = %q", p)
	}
}
