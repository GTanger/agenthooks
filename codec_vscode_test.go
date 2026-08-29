package agenthooks

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func vscodeArgs() []string { return []string{"agenthooks", "run", "--provider=vscode-copilot"} }

// vscodeDecode runs the payload through the provider switch, not through
// decodeClaude directly: the relabel lives in decodePayload's case, and a
// direct call would pass while the wiring was missing.
func vscodeDecode(t *testing.T, name string) any {
	t.Helper()
	typed, err := decodePayload(ProviderVSCodeCopilot, VariantUnknown, DetectionConfig, testNow, fixture(t, name))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return typed
}

// The Claude corpus IS the VS Code wire shape — same snake_case fields, same
// PascalCase event names — so it doubles as a compatibility assertion that the
// two dialects have not drifted apart. Phase 4 adds the recorded VS Code
// payloads as ground truth; until then these are the only real payloads with
// this shape, and they are recordings rather than docs-derived guesses.
func TestVSCodeDecodesClaudeCorpus(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("agenthookstest", "fixtures", "claude", "*.json"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("claude corpus: %v (%d files)", err, len(paths))
	}
	for _, p := range paths {
		name := filepath.Base(p)
		t.Run(name, func(t *testing.T) {
			payload, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			typed, err := decodePayload(ProviderVSCodeCopilot, VariantUnknown, DetectionConfig, testNow, payload)
			if err != nil {
				t.Fatal(err)
			}
			ev := eventOf(typed)
			if ev.Provider != ProviderVSCodeCopilot {
				t.Errorf("provider = %q; decodeClaude hardcodes claude-code, so the relabel is load-bearing", ev.Provider)
			}
			var wire struct {
				Name string `json:"hook_event_name"`
			}
			if err := json.Unmarshal(payload, &wire); err != nil {
				t.Fatal(err)
			}
			if ev.NativeName != wire.Name {
				t.Errorf("native name = %q, want %q", ev.NativeName, wire.Name)
			}
			want, ok := claudeKinds[wire.Name]
			if !ok {
				want = KindOther
			}
			if ev.Kind != want {
				t.Errorf("kind = %q, want %q", ev.Kind, want)
			}
			if string(ev.Raw) != string(payload) {
				t.Error("Raw must be byte-identical to the payload")
			}
		})
	}
}

// Stop and SubagentStop are the ONE wire difference from Claude Code:
// toolCallingLoop reads the block verdict from inside hookSpecificOutput, so
// encodeClaude's top-level placement would be a continuation that silently
// never happens. The nested hookEventName has to match too — a mismatch there
// makes _toHookResult strip the whole hookSpecificOutput.
func TestVSCodeStopDecisionIsNested(t *testing.T) {
	for _, tc := range []struct{ fixture, native string }{
		{"claude/stop.json", "Stop"},
		{"claude/subagent_stop.json", "SubagentStop"},
	} {
		typed := vscodeDecode(t, tc.fixture)
		base := eventOf(typed)
		wire, err := encodeVSCode(base, decisionCore{kind: DecisionContinue, instruction: "keep going"})
		if err != nil {
			t.Fatal(err)
		}
		var out struct {
			Decision string `json:"decision"`
			Reason   string `json:"reason"`
			HSO      struct {
				Decision      string `json:"decision"`
				Reason        string `json:"reason"`
				HookEventName string `json:"hookEventName"`
			} `json:"hookSpecificOutput"`
		}
		if err := json.Unmarshal(wire.Stdout, &out); err != nil {
			t.Fatalf("%s stdout %q: %v", tc.native, wire.Stdout, err)
		}
		if out.HSO.Decision != "block" || out.HSO.Reason != "keep going" {
			t.Errorf("%s: continuation must ride hookSpecificOutput; body = %s", tc.native, wire.Stdout)
		}
		if out.Decision != "" || out.Reason != "" {
			t.Errorf("%s: top-level decision/reason left behind; body = %s", tc.native, wire.Stdout)
		}
		if out.HSO.HookEventName != tc.native {
			t.Errorf("%s: nested hookEventName = %q; a mismatch strips the whole hookSpecificOutput", tc.native, out.HSO.HookEventName)
		}
	}
}

// The mirror of the test above: PostToolUse and UserPromptSubmit block via
// TOP-LEVEL decision/reason, which is where encodeClaude already puts them.
// Moving those too — the symmetric-looking change — would break both.
func TestVSCodeBlockStaysTopLevel(t *testing.T) {
	for _, tc := range []struct {
		fixture string
		core    decisionCore
	}{
		{"claude/post_tool_use.json", decisionCore{kind: DecisionFlagOutput, reason: "output flagged"}},
		{"claude/user_prompt_submit.json", decisionCore{kind: DecisionBlockPrompt, reason: "output flagged"}},
	} {
		base := eventOf(vscodeDecode(t, tc.fixture))
		wire, err := encodeVSCode(base, tc.core)
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		if err := json.Unmarshal(wire.Stdout, &out); err != nil {
			t.Fatalf("%s stdout %q: %v", tc.fixture, wire.Stdout, err)
		}
		if out["decision"] != "block" || out["reason"] != "output flagged" {
			t.Errorf("%s: block must stay top level; body = %s", tc.fixture, wire.Stdout)
		}
	}
}

// A TOP-LEVEL hookEventName makes _toHookResult discard the ENTIRE result when
// it mismatches, so the library never emits one; the name lives inside
// hookSpecificOutput only, stamped from the VS Code event name.
func TestVSCodeHookEventNameNeverTopLevel(t *testing.T) {
	for _, tc := range []struct {
		fixture string
		core    decisionCore
	}{
		{"claude/pre_tool_use.json", decisionCore{kind: DecisionDeny, reason: "blocked by policy"}},
		{"claude/stop.json", decisionCore{kind: DecisionContinue, instruction: "keep going"}},
		{"claude/session_start.json", decisionCore{kind: DecisionContinueSession, context: []string{"repo is frozen"}}},
	} {
		base := eventOf(vscodeDecode(t, tc.fixture))
		wire, err := encodeVSCode(base, tc.core)
		if err != nil {
			t.Fatal(err)
		}
		var out struct {
			HookEventName any `json:"hookEventName"`
			HSO           struct {
				HookEventName string `json:"hookEventName"`
			} `json:"hookSpecificOutput"`
		}
		if err := json.Unmarshal(wire.Stdout, &out); err != nil {
			t.Fatal(err)
		}
		if out.HookEventName != nil {
			t.Errorf("%s: top-level hookEventName present; a mismatch discards the whole result: %s", tc.fixture, wire.Stdout)
		}
		if out.HSO.HookEventName != base.NativeName {
			t.Errorf("%s: nested hookEventName = %q, want %q", tc.fixture, out.HSO.HookEventName, base.NativeName)
		}
	}
}

// Degradation is enforced generically in applyPolicy against the capability
// row, so it only holds end to end through the runner. The row is narrower
// than Claude Code's in both directions, and each narrowing below is a place a
// handler would otherwise believe it had an effect it does not have.
func TestVSCodeDegradesUnsupportedDecisions(t *testing.T) {
	// tool.post has no CapReplaceOutput: updatedToolOutput is a Claude
	// extension VS Code's output parser does not read.
	post := quietRunner()
	post.OnToolPost(func(ctx context.Context, e *ToolPostEvent) (ToolPostDecision, error) {
		if e.Can(CapReplaceOutput) {
			t.Error("vscode tool.post must not report CapReplaceOutput")
		}
		return ReplaceOutput("scrubbed"), nil
	})
	if out, code := runWith(t, post, vscodeArgs(), fixture(t, "claude/post_tool_use.json")); out != "{}" || code != 0 {
		t.Errorf("replace-output on tool.post = %q (exit %d), want {} at exit 0", out, code)
	}

	// agent.stop takes a continuation and nothing else: no ask, no context.
	// Neither is reachable through StopDecision, so the row is the assertion.
	stop := quietRunner()
	stop.OnStop(func(ctx context.Context, e *StopEvent) (StopDecision, error) {
		if e.Can(CapAddContext) || e.Can(CapAsk) {
			t.Error("vscode agent.stop must report neither CapAddContext nor CapAsk")
		}
		return Finish(), nil
	})
	if out, code := runWith(t, stop, vscodeArgs(), fixture(t, "claude/stop.json")); out != "{}" || code != 0 {
		t.Errorf("finish on agent.stop = %q (exit %d), want {} at exit 0", out, code)
	}

	// subagent.start is NOT observe-only: VS Code honors its additionalContext
	// the same way it honors SessionStart's, and encodeClaude has no case for
	// it, so encodeVSCode has to add one.
	if !Capabilities(ProviderVSCodeCopilot, VariantUnknown, KindSubagentStart).Has(CapAddContext) {
		t.Error("vscode subagent.start must report CapAddContext")
	}
	sub := &Event{Provider: ProviderVSCodeCopilot, NativeName: "SubagentStart", Kind: KindSubagentStart}
	wire, err := encodeVSCode(sub, decisionCore{context: []string{"repo is frozen for release"}})
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		HSO struct {
			AdditionalContext string `json:"additionalContext"`
			HookEventName     string `json:"hookEventName"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(wire.Stdout, &out); err != nil {
		t.Fatal(err)
	}
	if out.HSO.AdditionalContext != "repo is frozen for release" || out.HSO.HookEventName != "SubagentStart" {
		t.Errorf("subagent.start context dropped: %s", wire.Stdout)
	}
}

// Detection is flag-only by construction: VS Code injects no environment
// marker and sends no field Claude Code does not also send, so any shape or
// env branch would misroute real Claude Code sessions into this row.
func TestVSCodeDetection(t *testing.T) {
	inv, err := parseArgs(vscodeArgs())
	if err != nil || inv.provider != ProviderVSCodeCopilot || inv.confidence != DetectionConfig {
		t.Fatalf("--provider=vscode-copilot → %q/%q (%v)", inv.provider, inv.confidence, err)
	}
	if p, ok := detectFromShape(fixture(t, "claude/pre_tool_use.json")); !ok || p != ProviderClaudeCode {
		t.Errorf("shape detection = %q; a VS Code branch here would steal real Claude Code sessions", p)
	}
}
