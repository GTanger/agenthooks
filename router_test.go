package agenthooks

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func neutralStage(calls *[]string, name string) func(context.Context, *ToolPreEvent) (ToolPreDecision, error) {
	return func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
		*calls = append(*calls, name)
		return NoDecision(), nil
	}
}

func denyStage(calls *[]string, name, reason string) func(context.Context, *ToolPreEvent) (ToolPreDecision, error) {
	return func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
		*calls = append(*calls, name)
		return Deny(reason), nil
	}
}

func TestStackedRegistrationOrderFirstConclusiveWins(t *testing.T) {
	r := quietRunner()
	var calls []string
	r.OnToolPre(neutralStage(&calls, "h1"), neutralStage(&calls, "h2"))
	r.OnToolPre(denyStage(&calls, "h3", "third wins"))
	r.OnToolPre(neutralStage(&calls, "h4")) // after the winner: must not run

	d, err := r.Decide(context.Background(), testToolPreEvent(ProviderClaudeCode, "Bash"))
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if got := strings.Join(calls, ","); got != "h1,h2,h3" {
		t.Errorf("run order = %s, want h1,h2,h3 (h4 short-circuited)", got)
	}
	if d.Kind() != DecisionDeny || d.Reason() != "third wins" {
		t.Errorf("got kind=%v reason=%q", d.Kind(), d.Reason())
	}
}

func TestStackedNeutralEnrichmentPreserved(t *testing.T) {
	// A lone handler returning an enriched neutral must behave exactly as it
	// did before registration stacked: context/system message survive.
	single := quietRunner()
	single.OnToolPre(func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
		return NoDecision().WithContext("hint").WithSystemMessage("note"), nil
	})
	d, err := single.Decide(context.Background(), testToolPreEvent(ProviderClaudeCode, "Bash"))
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if d.Kind() != DecisionNoDecision || d.SystemMessage() != "note" {
		t.Errorf("neutral enrichment lost: kind=%v sys=%q", d.Kind(), d.SystemMessage())
	}
	if got := d.Context(); len(got) != 1 || got[0] != "hint" {
		t.Errorf("Context() = %v, want [hint]", got)
	}

	// Multiple all-neutral stages merge their contexts in order.
	multi := quietRunner()
	multi.OnToolPre(
		func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
			return NoDecision().WithContext("a"), nil
		},
		func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
			return NoDecision().WithContext("b"), nil
		},
	)
	d, err = multi.Decide(context.Background(), testToolPreEvent(ProviderClaudeCode, "Bash"))
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if got := d.Context(); d.Kind() != DecisionNoDecision || len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("merged neutral = kind %v, ctx %v; want no-decision, [a b]", d.Kind(), got)
	}
}

func TestEdgePathRunsStackedHandlers(t *testing.T) {
	// The pipeline slots in where single-handler dispatch was: the wire
	// output for a stacked neutral->deny is identical to a lone deny.
	r := quietRunner()
	var calls []string
	r.OnToolPre(neutralStage(&calls, "n"), denyStage(&calls, "d", "blocked"))
	out, code := runWith(t, r, claudeArgs(), fixture(t, "claude/pre_tool_use.json"))
	want := `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"blocked"}}`
	if out != want || code != 0 {
		t.Errorf("got %q (exit %d), want %q (exit 0)", out, code, want)
	}
	if got := strings.Join(calls, ","); got != "n,d" {
		t.Errorf("run order = %s, want n,d", got)
	}
}

func TestAnyShortCircuits(t *testing.T) {
	var calls []string
	h := Any(
		neutralStage(&calls, "n"),
		denyStage(&calls, "d", "stop"),
		neutralStage(&calls, "never"),
	)
	d, err := h(context.Background(), testToolPreEvent(ProviderClaudeCode, "Bash"))
	if err != nil {
		t.Fatalf("Any: %v", err)
	}
	if got := strings.Join(calls, ","); got != "n,d" {
		t.Errorf("run order = %s, want n,d", got)
	}
	if d.Kind() != DecisionDeny || d.Reason() != "stop" {
		t.Errorf("got kind=%v reason=%q", d.Kind(), d.Reason())
	}
}

func TestAnyErrorAborts(t *testing.T) {
	sentinel := errors.New("boom")
	var calls []string
	h := Any(
		func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
			calls = append(calls, "err")
			return NoDecision(), sentinel
		},
		denyStage(&calls, "never", "x"),
	)
	_, err := h(context.Background(), testToolPreEvent(ProviderClaudeCode, "Bash"))
	if !errors.Is(err, sentinel) {
		t.Errorf("want sentinel error, got %v", err)
	}
	if got := strings.Join(calls, ","); got != "err" {
		t.Errorf("error must abort immediately, ran %s", got)
	}
}

func TestAllMostRestrictiveWinsContextAppends(t *testing.T) {
	var calls []string
	h := All(
		func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
			calls = append(calls, "allow")
			return Allow().WithContext("allow-ctx"), nil
		},
		func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
			calls = append(calls, "deny")
			return Deny("deny wins").WithContext("deny-ctx").WithSystemMessage("sys"), nil
		},
		func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
			calls = append(calls, "ask")
			return AskUser("please").WithContext("ask-ctx"), nil
		},
	)
	d, err := h(context.Background(), testToolPreEvent(ProviderClaudeCode, "Bash"))
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if got := strings.Join(calls, ","); got != "allow,deny,ask" {
		t.Errorf("All must run every handler in order, ran %s", got)
	}
	if d.Kind() != DecisionDeny || d.Reason() != "deny wins" || d.SystemMessage() != "sys" {
		t.Errorf("winner fields not wholesale: kind=%v reason=%q sys=%q", d.Kind(), d.Reason(), d.SystemMessage())
	}
	want := []string{"allow-ctx", "deny-ctx", "ask-ctx"}
	got := d.Context()
	if len(got) != len(want) {
		t.Fatalf("Context() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Context() = %v, want %v", got, want)
		}
	}
}

func TestAllTiePrefersEarliest(t *testing.T) {
	h := All(
		func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) { return Deny("first"), nil },
		func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) { return Deny("second"), nil },
	)
	d, err := h(context.Background(), testToolPreEvent(ProviderClaudeCode, "Bash"))
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if d.Reason() != "first" {
		t.Errorf("tie must go to the earliest, got %q", d.Reason())
	}
}

func TestAllStopAgentSticky(t *testing.T) {
	h := All(
		func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) { return Deny("gate"), nil },
		func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
			return NoDecision().StopAgent("halt everything"), nil
		},
	)
	d, err := h(context.Background(), testToolPreEvent(ProviderClaudeCode, "Bash"))
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	core := d.decCore()
	if d.Kind() != DecisionDeny || !core.stopAgent || core.stopReason != "halt everything" {
		t.Errorf("StopAgent must stick through the merge: kind=%v stop=%v reason=%q", d.Kind(), core.stopAgent, core.stopReason)
	}
}

func TestAllErrorsJoinAndAbort(t *testing.T) {
	e1, e2 := errors.New("first failure"), errors.New("second failure")
	var calls []string
	h := All(
		func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
			calls = append(calls, "f1")
			return NoDecision(), e1
		},
		denyStage(&calls, "ok", "x"),
		func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
			calls = append(calls, "f2")
			return NoDecision(), e2
		},
	)
	d, err := h(context.Background(), testToolPreEvent(ProviderClaudeCode, "Bash"))
	if !errors.Is(err, e1) || !errors.Is(err, e2) {
		t.Errorf("joined error must carry both failures, got %v", err)
	}
	if got := strings.Join(calls, ","); got != "f1,ok,f2" {
		t.Errorf("All must run every handler even after an error, ran %s", got)
	}
	if coreOf(d).kind != DecisionNoDecision {
		t.Errorf("aborted combinator must return the neutral decision, got %v", d.Kind())
	}
}

func TestAllContinueBeatsFinish(t *testing.T) {
	ev := &StopEvent{Event: Event{Provider: ProviderClaudeCode, NativeName: "Stop", Kind: KindStop}}
	h := All(
		func(ctx context.Context, e *StopEvent) (StopDecision, error) { return Finish(), nil },
		func(ctx context.Context, e *StopEvent) (StopDecision, error) { return ContinueWith("more work"), nil },
	)
	d, err := h(context.Background(), ev)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if d.Kind() != DecisionContinue || d.Instruction() != "more work" {
		t.Errorf("continue must outrank finish: kind=%v instruction=%q", d.Kind(), d.Instruction())
	}
}

func TestWhenGuardsByMatcher(t *testing.T) {
	shell := testToolPreEvent(ProviderClaudeCode, "Bash")
	read := testToolPreEvent(ProviderClaudeCode, "Read")
	mcp := testToolPreEvent(ProviderClaudeCode, "mcp__srv__lookup")

	cases := []struct {
		name string
		m    Matcher
		hit  *ToolPreEvent
		miss *ToolPreEvent
	}{
		{"canonical", MatchCanonical(ToolShell), shell, read},
		{"names", MatchTools("bash"), shell, read}, // case-insensitive
		{"mcp", MatchMCP("srv/*"), mcp, shell},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			h := When(tc.m, denyStage(&calls, "h", "gated"))

			d, err := h(context.Background(), tc.hit)
			if err != nil || d.Kind() != DecisionDeny {
				t.Errorf("matching tool must dispatch: %v, %v", d.Kind(), err)
			}
			d, err = h(context.Background(), tc.miss)
			if err != nil || d.Kind() != DecisionNoDecision {
				t.Errorf("non-matching tool must be neutral: %v, %v", d.Kind(), err)
			}
			if len(calls) != 1 {
				t.Errorf("handler ran %d times, want 1", len(calls))
			}
		})
	}
}

func TestWhenNonToolEventIsNeutral(t *testing.T) {
	called := false
	h := When(MatchTools("Bash"), func(ctx context.Context, e *PromptEvent) (PromptDecision, error) {
		called = true
		return BlockPrompt("x"), nil
	})
	pe := &PromptEvent{Event: Event{Provider: ProviderClaudeCode, Kind: KindPromptSubmitted}, Prompt: "hi"}
	d, err := h(context.Background(), pe)
	if err != nil || d.Kind() != DecisionNoDecision || called {
		t.Errorf("events without a tool call never match: kind=%v called=%v err=%v", d.Kind(), called, err)
	}
}

type matchEverything struct{}

func (matchEverything) Matches(ToolCall) bool { return true }

func TestWhenAcceptsCustomMatcher(t *testing.T) {
	var calls []string
	h := When(matchEverything{}, denyStage(&calls, "h", "gated"))
	d, err := h(context.Background(), testToolPreEvent(ProviderClaudeCode, "AnythingAtAll"))
	if err != nil || d.Kind() != DecisionDeny || len(calls) != 1 {
		t.Errorf("custom matcher must plug in: kind=%v calls=%v err=%v", d.Kind(), calls, err)
	}
}

func TestUseMiddlewareOrder(t *testing.T) {
	r := quietRunner()
	var order []string
	mw := func(name string) Interceptor {
		return func(ctx context.Context, typed any, next Next) (Decision, error) {
			order = append(order, name+"-before")
			d, err := next(ctx, typed)
			order = append(order, name+"-after")
			return d, err
		}
	}
	r.Use(mw("outer"))
	r.Use(mw("inner"))
	r.OnToolPre(func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
		order = append(order, "handler")
		return Deny("no"), nil
	})
	d, err := r.Decide(context.Background(), testToolPreEvent(ProviderClaudeCode, "Bash"))
	if err != nil || d.Kind() != DecisionDeny {
		t.Fatalf("got %v, %v", d, err)
	}
	want := "outer-before,inner-before,handler,inner-after,outer-after"
	if got := strings.Join(order, ","); got != want {
		t.Errorf("middleware order = %s, want %s", got, want)
	}
}

func TestUseShortCircuitSkipsHandlers(t *testing.T) {
	r := quietRunner()
	handlerRan := false
	r.Use(func(ctx context.Context, typed any, next Next) (Decision, error) {
		return Deny("gated by middleware"), nil // never calls next
	})
	r.OnToolPre(func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
		handlerRan = true
		return NoDecision(), nil
	})

	d, err := r.Decide(context.Background(), testToolPreEvent(ProviderClaudeCode, "Bash"))
	if err != nil || d.Kind() != DecisionDeny || handlerRan {
		t.Errorf("middleware short-circuit: kind=%v handlerRan=%v err=%v", d.Kind(), handlerRan, err)
	}

	// Same middleware gates the edge path: the wire carries its deny.
	out, code := runWith(t, r, claudeArgs(), fixture(t, "claude/pre_tool_use.json"))
	if code != 0 || !strings.Contains(out, `"permissionDecision":"deny"`) {
		t.Errorf("middleware must gate the edge path too: %q (exit %d)", out, code)
	}
}

func TestUseTransformsProjection(t *testing.T) {
	r := quietRunner()
	var seen string
	r.Use(func(ctx context.Context, typed any, next Next) (Decision, error) {
		if tp, ok := typed.(*ToolPreEvent); ok {
			tp.Tool.Input = json.RawMessage(`{"command":"echo safe"}`)
		}
		return next(ctx, typed)
	})
	r.OnToolPre(func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
		seen = string(e.Tool.Input)
		return NoDecision(), nil
	})

	ev := testToolPreEvent(ProviderClaudeCode, "Bash")
	rawBefore := string(ev.Raw)
	if _, err := r.Decide(context.Background(), ev); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if seen != `{"command":"echo safe"}` {
		t.Errorf("handler must see the transformed projection, saw %s", seen)
	}
	if string(ev.Raw) != rawBefore {
		t.Error("Raw must stay verbatim through middleware transforms")
	}
}

func TestUsePostProcessesDecision(t *testing.T) {
	r := quietRunner()
	r.Use(func(ctx context.Context, typed any, next Next) (Decision, error) {
		d, err := next(ctx, typed)
		if err == nil && d.Kind() == DecisionDeny {
			return AskUser("softened: " + d.Reason()), nil
		}
		return d, err
	})
	r.OnToolPre(func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
		return Deny("hard"), nil
	})
	d, err := r.Decide(context.Background(), testToolPreEvent(ProviderClaudeCode, "Bash"))
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if d.Kind() != DecisionAsk || d.Reason() != "softened: hard" {
		t.Errorf("post-processing lost: kind=%v reason=%q", d.Kind(), d.Reason())
	}
}

func walkToolPreStage(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
	return NoDecision(), nil
}

func walkStopStage(ctx context.Context, e *StopEvent) (StopDecision, error) {
	return Finish(), nil
}

func TestWalkOrderNamesAndPositions(t *testing.T) {
	r := quietRunner()
	r.OnAny(func(ctx context.Context, e *Event) error { return nil })
	r.OnOther("Setup", func(ctx context.Context, e *Event) error { return nil })
	r.Use(func(ctx context.Context, typed any, next Next) (Decision, error) { return next(ctx, typed) })
	r.OnToolPre(walkToolPreStage, walkToolPreStage)
	r.OnStop(walkStopStage)

	var infos []StageInfo
	if err := r.Walk(func(si StageInfo) error {
		infos = append(infos, si)
		return nil
	}); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(infos) != 6 {
		t.Fatalf("visited %d stages, want 6: %+v", len(infos), infos)
	}

	check := func(i int, kind EventKind, typ StageType, pos int) {
		t.Helper()
		if infos[i].Kind != kind || infos[i].Type != typ || infos[i].Pos != pos {
			t.Errorf("stage %d = %+v, want kind=%q type=%s pos=%d", i, infos[i], kind, typ, pos)
		}
	}
	check(0, "", StageObserver, 0)        // OnAny
	check(1, KindOther, StageObserver, 0) // OnOther
	check(2, "", StageMiddleware, 0)      // Use
	check(3, KindToolPre, StageHandler, 0)
	check(4, KindToolPre, StageHandler, 1)
	check(5, KindStop, StageHandler, 0)

	// Names are the reflected function names: named funcs report as
	// themselves, anonymous funcs as their closure names.
	if !strings.Contains(infos[3].Name, "walkToolPreStage") {
		t.Errorf("reflected name for a named func: %q", infos[3].Name)
	}
	if !strings.Contains(infos[4].Name, "walkToolPreStage") {
		t.Errorf("reflected name for a named func: %q", infos[4].Name)
	}
	if !strings.Contains(infos[5].Name, "walkStopStage") {
		t.Errorf("reflected name for a named func: %q", infos[5].Name)
	}
}

func TestWalkStopsOnError(t *testing.T) {
	r := quietRunner()
	r.OnToolPre(walkToolPreStage, walkToolPreStage, walkToolPreStage)
	sentinel := errors.New("stop walking")
	visits := 0
	err := r.Walk(func(si StageInfo) error {
		visits++
		if visits == 2 {
			return sentinel
		}
		return nil
	})
	if !errors.Is(err, sentinel) || visits != 2 {
		t.Errorf("Walk must stop at the first error: err=%v visits=%d", err, visits)
	}
}

func TestCombinatorsCompose(t *testing.T) {
	// Closure property: combinators nest and register like leaves.
	var calls []string
	r := quietRunner()
	r.OnToolPre(
		Any(
			When(MatchCanonical(ToolFileRead), denyStage(&calls, "reads", "no reads")),
			All(
				neutralStage(&calls, "audit"),
				When(MatchCanonical(ToolShell), denyStage(&calls, "shells", "no shells")),
			),
		),
	)
	d, err := r.Decide(context.Background(), testToolPreEvent(ProviderClaudeCode, "Bash"))
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if got := strings.Join(calls, ","); got != "audit,shells" {
		t.Errorf("composition ran %s, want audit,shells", got)
	}
	if d.Kind() != DecisionDeny || d.Reason() != "no shells" {
		t.Errorf("got kind=%v reason=%q", d.Kind(), d.Reason())
	}
}
