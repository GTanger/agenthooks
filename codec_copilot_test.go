package agenthooks

import (
	"context"
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
		"subagentStart":       {"copilot/subagent_start.json", KindSubagentStart},
		"subagentStop":        {"copilot/subagent_stop.json", KindSubagentStop},
		"preCompact":          {"copilot/pre_compact.json", KindCompactPre},
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

// preToolUse is the only event Copilot lets a hook steer, and it steers it
// through one string field. A typo in the value ("allowed", "Ask") is not a
// parse error on Copilot's side — it is an unrecognized decision, i.e. a
// silent fall-through to the normal permission flow. Pin the exact strings.
func TestCopilotToolPreDecisionValues(t *testing.T) {
	typed, err := decodeCopilot(VariantUnknown, DetectionConfig, testNow, fixture(t, "copilot/pre_tool_use.json"))
	if err != nil {
		t.Fatal(err)
	}
	base := eventOf(typed)

	for _, tc := range []struct {
		name string
		core decisionCore
		want string
	}{
		{"allow", decisionCore{kind: DecisionAllow, reason: "on the allowlist"}, "allow"},
		{"ask", decisionCore{kind: DecisionAsk, reason: "confirm?"}, "ask"},
	} {
		wire, err := encodeCopilot(base, tc.core)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if wire.ExitCode != 0 {
			t.Fatalf("%s exit code = %d, want 0 (any non-zero preToolUse exit denies unconditionally)", tc.name, wire.ExitCode)
		}
		var out map[string]any
		if err := json.Unmarshal(wire.Stdout, &out); err != nil {
			t.Fatalf("%s stdout %q: %v", tc.name, wire.Stdout, err)
		}
		if out["permissionDecision"] != tc.want || out["permissionDecisionReason"] != tc.core.reason {
			t.Errorf("%s body = %s", tc.name, wire.Stdout)
		}
	}
}

// The highest-risk field in the codec: Copilot ignores an unknown key without
// complaint, so a rename of modifiedArgs — or wrapping the args in an extra
// envelope, or re-stringifying them the way toolArgs arrives on input — runs
// the ORIGINAL command while the handler believes it sanitized it. Assert the
// key name and that the value is the args object itself.
func TestCopilotToolPreModifiedArgs(t *testing.T) {
	typed, err := decodeCopilot(VariantUnknown, DetectionConfig, testNow, fixture(t, "copilot/pre_tool_use.json"))
	if err != nil {
		t.Fatal(err)
	}
	wire, err := encodeCopilot(eventOf(typed), decisionCore{
		kind:            DecisionAllow,
		hasUpdatedInput: true,
		updatedInput:    map[string]any{"command": "echo sanitized"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if wire.ExitCode != 0 {
		t.Fatalf("modified-args exit code = %d, want 0", wire.ExitCode)
	}
	var out struct {
		Decision string `json:"permissionDecision"`
		Modified *struct {
			Command string `json:"command"`
		} `json:"modifiedArgs"`
	}
	if err := json.Unmarshal(wire.Stdout, &out); err != nil {
		t.Fatalf("stdout %q: %v", wire.Stdout, err)
	}
	if out.Modified == nil {
		t.Fatalf("no modifiedArgs key in %s; an input rewrite under any other name is a silent no-op", wire.Stdout)
	}
	if out.Modified.Command != "echo sanitized" {
		t.Errorf("modifiedArgs.command = %q, want the rewritten args as a plain object; body = %s", out.Modified.Command, wire.Stdout)
	}
	if out.Decision != "allow" {
		t.Errorf("permissionDecision = %q; the rewrite must not displace the verdict", out.Decision)
	}
}

// permissionRequest answers on behavior/message, and allow here is the field
// that suppresses a user prompt — emitting it under preToolUse's
// permissionDecision name would leave the user staring at a prompt the
// handler already answered.
func TestCopilotPermissionAllow(t *testing.T) {
	typed, err := decodeCopilot(VariantUnknown, DetectionConfig, testNow, fixture(t, "copilot/permission_request.json"))
	if err != nil {
		t.Fatal(err)
	}
	wire, err := encodeCopilot(eventOf(typed), decisionCore{kind: DecisionAllow, reason: "pre-approved"})
	if err != nil {
		t.Fatal(err)
	}
	if wire.ExitCode != 0 {
		t.Fatalf("permission allow exit code = %d, want 0", wire.ExitCode)
	}
	var out map[string]any
	if err := json.Unmarshal(wire.Stdout, &out); err != nil {
		t.Fatal(err)
	}
	if out["behavior"] != "allow" || out["message"] != "pre-approved" || out["permissionDecision"] != nil {
		t.Errorf("permission allow body = %s", wire.Stdout)
	}
}

// subagentStop shares agentStop's schema; if it ever forked to its own case,
// a subagent continuation would stop silently while the parent's kept working
// — the kind of asymmetry that only shows up under a delegating agent.
func TestCopilotSubagentStopContinue(t *testing.T) {
	typed, err := decodeCopilot(VariantUnknown, DetectionConfig, testNow, fixture(t, "copilot/subagent_stop.json"))
	if err != nil {
		t.Fatal(err)
	}
	wire, err := encodeCopilot(eventOf(typed), decisionCore{kind: DecisionContinue, instruction: "finish the migration"})
	if err != nil {
		t.Fatal(err)
	}
	if wire.ExitCode != 0 {
		t.Fatalf("subagent continue exit code = %d, want 0", wire.ExitCode)
	}
	var out map[string]any
	if err := json.Unmarshal(wire.Stdout, &out); err != nil {
		t.Fatal(err)
	}
	if out["decision"] != "block" || out["reason"] != "finish the migration" {
		t.Errorf("subagentStop body = %s", wire.Stdout)
	}
}

// sessionStart is the only place Copilot accepts injected context, and it
// arrives under additionalContext — not the reason/message names every other
// copilot event uses. Wrong key means the context silently never reaches the
// model, which reads as "the agent ignored my instructions".
func TestCopilotSessionStartAdditionalContext(t *testing.T) {
	typed, err := decodeCopilot(VariantUnknown, DetectionConfig, testNow, fixture(t, "copilot/session_start.json"))
	if err != nil {
		t.Fatal(err)
	}
	wire, err := encodeCopilot(eventOf(typed), decisionCore{
		kind:    DecisionContinueSession,
		context: []string{"repo is frozen for release"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if wire.ExitCode != 0 {
		t.Fatalf("session start exit code = %d, want 0", wire.ExitCode)
	}
	var out map[string]any
	if err := json.Unmarshal(wire.Stdout, &out); err != nil {
		t.Fatal(err)
	}
	if out["additionalContext"] != "repo is frozen for release" {
		t.Errorf("sessionStart body = %s", wire.Stdout)
	}
}

// Degradation is enforced generically in applyPolicy, not in this codec — so
// the guarantee only holds end to end through the runner. It matters here
// because encodeCopilot has no case for these: an ask on permissionRequest or
// a replace-output on postToolUse that slipped past the policy layer would
// serialize to {}, i.e. no opinion, which on permissionRequest silently means
// "whatever the user clicks" rather than the block the handler asked for.
// Every path must still exit 0: a non-zero exit propagates to preToolUse
// semantics as an unconditional deny.
func TestCopilotDegradesUnsupportedDecisions(t *testing.T) {
	copilotArgs := []string{"agenthooks", "run", "--provider=copilot"}

	// permissionRequest declares deny+allow but no ask. FallbackDeny must
	// harden to a real behavior:deny, not fall through to the empty body.
	deny := quietRunner(WithPolicy(Policy{AskFallback: FallbackDeny}))
	deny.OnPermission(func(ctx context.Context, e *PermissionEvent) (ToolPreDecision, error) {
		if e.Can(CapAsk) {
			t.Error("copilot permissionRequest must not report CapAsk")
		}
		return AskUser("confirm?"), nil
	})
	out, code := runWith(t, deny, copilotArgs, fixture(t, "copilot/permission_request.json"))
	if out != `{"behavior":"deny","message":"confirm?"}` || code != 0 {
		t.Errorf("ask on permissionRequest = %q (exit %d), want a hardened deny at exit 0", out, code)
	}

	// The default fallback is honest instead: no opinion, empty body.
	noop := quietRunner()
	noop.OnPermission(func(ctx context.Context, e *PermissionEvent) (ToolPreDecision, error) {
		return AskUser("confirm?"), nil
	})
	out, code = runWith(t, noop, copilotArgs, fixture(t, "copilot/permission_request.json"))
	if out != "{}" || code != 0 {
		t.Errorf("ask under FallbackNoDecision = %q (exit %d), want {} at exit 0", out, code)
	}

	// postToolUse has no capset at all on Copilot: a rewritten tool output
	// must degrade to observation, never to a body that looks like a verdict.
	post := quietRunner()
	post.OnToolPost(func(ctx context.Context, e *ToolPostEvent) (ToolPostDecision, error) {
		if e.Can(CapReplaceOutput) {
			t.Error("copilot tool.post must not report CapReplaceOutput")
		}
		return ReplaceOutput("scrubbed"), nil
	})
	out, code = runWith(t, post, copilotArgs, fixture(t, "copilot/post_tool_use.json"))
	if out != "{}" || code != 0 {
		t.Errorf("replace-output on postToolUse = %q (exit %d), want {} at exit 0", out, code)
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
