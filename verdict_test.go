package agenthooks

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// Every decision type satisfies the sealed Decision interface.
var (
	_ Decision = ToolPreDecision{}
	_ Decision = PromptDecision{}
	_ Decision = StopDecision{}
	_ Decision = ToolPostDecision{}
	_ Decision = SessionStartDecision{}
	_ Decision = coreDecision{}
)

func testToolPreEvent(p Provider, tool string) *ToolPreEvent {
	s := SessionInfo{ID: "sess-1", TurnID: "turn-1"}
	return &ToolPreEvent{
		Event: Event{Provider: p, NativeName: "PreToolUse", Kind: KindToolPre, Session: s},
		Tool:  makeToolCall(s, tool, "tu-1", []byte(`{"command":"rm -rf /tmp/x"}`), nil),
	}
}

func TestDecisionKindString(t *testing.T) {
	cases := map[DecisionKind]string{
		DecisionNoDecision:      "no-decision",
		DecisionAllow:           "allow",
		DecisionDeny:            "deny",
		DecisionAsk:             "ask",
		DecisionAcceptPrompt:    "accept-prompt",
		DecisionBlockPrompt:     "block-prompt",
		DecisionFinish:          "finish",
		DecisionContinue:        "continue",
		DecisionObserved:        "observed",
		DecisionFlagOutput:      "flag-output",
		DecisionReplaceOutput:   "replace-output",
		DecisionContinueSession: "continue-session",
	}
	for k, want := range cases {
		if got := k.String(); got != want {
			t.Errorf("DecisionKind(%d).String() = %q, want %q", int(k), got, want)
		}
	}
	if got := DecisionKind(99).String(); got != "decision-kind(99)" {
		t.Errorf("unknown kind stringifies as %q", got)
	}
}

func TestDecisionReadAccessors(t *testing.T) {
	d := Deny("no shells").WithContext("a").WithContext("b").WithSystemMessage("careful")
	if d.Kind() != DecisionDeny || d.Reason() != "no shells" || d.SystemMessage() != "careful" {
		t.Errorf("common accessors: kind=%v reason=%q sys=%q", d.Kind(), d.Reason(), d.SystemMessage())
	}
	if got := d.Context(); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("Context() = %v, want [a b]", got)
	}

	if _, ok := Allow().UpdatedInput(); ok {
		t.Error("UpdatedInput must report absent without WithUpdatedInput")
	}
	if v, ok := Allow().WithUpdatedInput(map[string]any{"command": "true"}).UpdatedInput(); !ok || v == nil {
		t.Errorf("UpdatedInput() = %v, %v; want value, true", v, ok)
	}

	if ContinueWith("more").Instruction() != "more" {
		t.Error("Instruction must return the ContinueWith payload")
	}
	if Finish().Kind() != DecisionFinish || ContinueWith("x").Kind() != DecisionContinue {
		t.Error("stop decision kinds wrong")
	}

	if v, ok := ReplaceOutput("redacted").ReplacedOutput(); !ok || v != "redacted" {
		t.Errorf("ReplacedOutput() = %v, %v; want redacted, true", v, ok)
	}
	if _, ok := Observed().ReplacedOutput(); ok {
		t.Error("ReplacedOutput must report absent on Observed")
	}
	if FlagOutput("stale").Reason() != "stale" {
		t.Error("Reason on FlagOutput")
	}

	if AcceptPrompt().Kind() != DecisionAcceptPrompt || BlockPrompt("x").Kind() != DecisionBlockPrompt {
		t.Error("prompt decision kinds wrong")
	}
	if ContinueSession().Kind() != DecisionContinueSession {
		t.Error("session decision kind wrong")
	}

	var zero ToolPreDecision
	if zero.Kind() != DecisionNoDecision {
		t.Error("zero-value decision must be neutral")
	}
}

// TestDecisionBlocks is the truth table for the Blocks classification over
// every decision constructor: true exactly for the kinds whose intent is
// "the action is prevented" (deny, block-prompt). An ask defers to a human —
// it does not block.
func TestDecisionBlocks(t *testing.T) {
	cases := []struct {
		name     string
		decision Decision
		want     bool
	}{
		// tool.pre / permission.request
		{"NoDecision", NoDecision(), false},
		{"Allow", Allow(), false},
		{"Deny", Deny("no shells"), true},
		{"AskUser", AskUser("confirm?"), false},
		// prompt.submitted
		{"AcceptPrompt", AcceptPrompt(), false},
		{"BlockPrompt", BlockPrompt("secrets"), true},
		// agent.stop / subagent.stop
		{"Finish", Finish(), false},
		{"ContinueWith", ContinueWith("keep going"), false},
		// tool.post / tool.error
		{"Observed", Observed(), false},
		{"FlagOutput", FlagOutput("stale"), false},
		{"ReplaceOutput", ReplaceOutput("redacted"), false},
		// session.start
		{"ContinueSession", ContinueSession(), false},
	}
	for _, tc := range cases {
		if got := tc.decision.Blocks(); got != tc.want {
			t.Errorf("%s.Blocks() = %v, want %v", tc.name, got, tc.want)
		}
	}

	var zero ToolPreDecision
	if zero.Blocks() {
		t.Error("zero-value (neutral) decision must not block")
	}
	if !Deny("x").WithContext("c").WithSystemMessage("s").Blocks() {
		t.Error("modifiers must not change the Blocks classification")
	}
}

func TestContextAccessorDoesNotAlias(t *testing.T) {
	d := NoDecision().WithContext("keep")
	got := d.Context()
	got[0] = "mutated"
	if d.Context()[0] != "keep" {
		t.Error("Context must return a copy, not the internal slice")
	}
}

func TestDecideReturnsWinningDecision(t *testing.T) {
	r := quietRunner()
	r.OnToolPre(func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
		return Deny("blocked").WithContext("why"), nil
	})
	d, err := r.Decide(context.Background(), testToolPreEvent(ProviderClaudeCode, "Bash"))
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if d.Kind() != DecisionDeny || d.Reason() != "blocked" {
		t.Errorf("got kind=%v reason=%q", d.Kind(), d.Reason())
	}
	if _, ok := d.(ToolPreDecision); !ok {
		t.Errorf("Decide must return the concrete decision type, got %T", d)
	}
}

func TestDecideSkipsCapabilityDegradation(t *testing.T) {
	// Codex cannot express ask: Run degrades it per policy, Decide must not.
	r := quietRunner(WithPolicy(Policy{Unsupported: Degrade, AskFallback: FallbackDeny}))
	r.OnToolPre(func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
		return AskUser("confirm?"), nil
	})
	d, err := r.Decide(context.Background(), testToolPreEvent(ProviderCodex, "shell"))
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if d.Kind() != DecisionAsk {
		t.Errorf("Decide must return the raw ask (no applyPolicy), got %v", d.Kind())
	}
}

func TestDecideNeutralIsZeroDecision(t *testing.T) {
	r := quietRunner()
	d, err := r.Decide(context.Background(), testToolPreEvent(ProviderClaudeCode, "Bash"))
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if d == nil || d.Kind() != DecisionNoDecision {
		t.Errorf("exhausted pipeline must be the neutral zero decision, got %v", d)
	}
}

func TestDecideStageErrorReturnsError(t *testing.T) {
	sentinel := errors.New("stage boom")
	r := quietRunner()
	r.OnToolPre(func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
		return NoDecision(), sentinel
	})
	d, err := r.Decide(context.Background(), testToolPreEvent(ProviderClaudeCode, "Bash"))
	if !errors.Is(err, sentinel) {
		t.Errorf("stage error must return as-is, got %v", err)
	}
	if d != nil {
		t.Errorf("decision must be nil on error, got %v", d)
	}
}

func TestDecidePanicBecomesError(t *testing.T) {
	r := quietRunner()
	r.OnToolPre(func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
		panic("handler bug")
	})
	_, err := r.Decide(context.Background(), testToolPreEvent(ProviderClaudeCode, "Bash"))
	if err == nil || !strings.Contains(err.Error(), "panic") {
		t.Errorf("panic must convert to an error, got %v", err)
	}
}

func TestDecideHonorsContextDeadline(t *testing.T) {
	r := quietRunner()
	r.OnToolPre(func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
		time.Sleep(2 * time.Second) // ignores ctx on purpose
		return Allow(), nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := r.Decide(ctx, testToolPreEvent(ProviderClaudeCode, "Bash"))
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("deadline not enforced: took %v", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("want deadline error, got %v", err)
	}
}

func TestDecideRunsObserversAndMiddleware(t *testing.T) {
	r := quietRunner()
	var observed, wrapped bool
	r.OnAny(func(ctx context.Context, e *Event) error {
		observed = true
		return nil
	})
	r.Use(func(ctx context.Context, typed any, next Next) (Decision, error) {
		wrapped = true
		return next(ctx, typed)
	})
	r.OnToolPre(func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
		return Deny("no"), nil
	})
	d, err := r.Decide(context.Background(), testToolPreEvent(ProviderClaudeCode, "Bash"))
	if err != nil || d.Kind() != DecisionDeny {
		t.Fatalf("got %v, %v", d, err)
	}
	if !observed || !wrapped {
		t.Errorf("observers/middleware must run in Decide: observed=%v wrapped=%v", observed, wrapped)
	}
}

func TestDecideRejectsNonEvent(t *testing.T) {
	r := quietRunner()
	if _, err := r.Decide(context.Background(), 42); err == nil {
		t.Error("Decide must reject values that are not agenthooks events")
	}
}
