package agenthooks

// The event router composes handlers the way net/http composes handlers and
// middleware: the composable unit is the handler func type itself.
// Combinators take handlers and return handlers (the closure property), so
// leaves and compositions register identically, and variadic registration is
// just a top-level Any. Middleware keeps its own shape (Interceptor) because
// it is the one stage that receives the rest of the pipeline as next.

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"sort"
)

// decision constrains the generic combinators to the five decision types.
// All five share the same underlying struct, which is what lets a single
// implementation read and rebuild decision cores through decisionShape
// conversions — call sites never write a type parameter.
type decision interface {
	ToolPreDecision | PromptDecision | StopDecision | ToolPostDecision | SessionStartDecision
}

// decisionShape is the shared underlying struct of every decision type.
type decisionShape struct{ core decisionCore }

func coreOf[D decision](d D) decisionCore { return decisionShape(d).core }

func fromCore[D decision](c decisionCore) D { return D(decisionShape{core: c}) }

// severity ranks decision kinds for All's most-restrictive-wins merge. Only
// kinds from the same decision family ever meet in one merge: deny > ask >
// allow > neutral, and within the other families continue > finish,
// replace-output > flag-output > observed, block-prompt > accept-prompt.
func (k DecisionKind) severity() int {
	switch k {
	case DecisionDeny, DecisionBlockPrompt, DecisionContinue:
		return 5
	case DecisionAsk:
		return 4
	case DecisionReplaceOutput:
		return 3
	case DecisionFlagOutput:
		return 2
	case DecisionAllow, DecisionAcceptPrompt, DecisionFinish, DecisionObserved, DecisionContinueSession:
		return 1
	}
	return 0
}

// mergeCores implements the All merge: the most restrictive kind wins (ties
// go to the earliest), context appends from every decision in order, the
// winner's other fields are taken wholesale, and StopAgent is sticky — if
// any decision stopped the agent, the merge does.
func mergeCores(cores []decisionCore) decisionCore {
	win := 0
	for i, c := range cores {
		if c.kind.severity() > cores[win].kind.severity() {
			win = i
		}
	}
	merged := cores[win]
	var ctxs []string
	for _, c := range cores {
		ctxs = append(ctxs, c.context...)
	}
	merged.context = ctxs
	if !merged.stopAgent {
		for _, c := range cores {
			if c.stopAgent {
				merged.stopAgent = true
				merged.stopReason = c.stopReason
				break
			}
		}
	}
	return merged
}

// runStages runs handlers in order with stacked-registration (Any)
// semantics: the first conclusive decision (Kind != DecisionNoDecision) wins
// and later handlers do not run; a handler error aborts immediately.
// Neutral decisions fall through, but their enrichments (context,
// system message, StopAgent) are not discarded: when every handler stays
// neutral the neutrals merge, so a single registered handler behaves exactly
// as it did before registrations stacked.
func runStages[E any, D decision](ctx context.Context, ev *E, hs []func(context.Context, *E) (D, error)) (D, error) {
	var zero D
	var neutrals []decisionCore
	for _, h := range hs {
		d, err := h(ctx, ev)
		if err != nil {
			return zero, err
		}
		c := coreOf(d)
		if c.kind != DecisionNoDecision {
			return d, nil
		}
		neutrals = append(neutrals, c)
	}
	if len(neutrals) == 0 {
		return zero, nil
	}
	return fromCore[D](mergeCores(neutrals)), nil
}

// observeStages runs observe-only handlers in order. Every handler runs;
// errors are joined. With at least one handler registered the outcome is
// Observed, preserving the single-handler contract.
func observeStages[E any](ctx context.Context, ev *E, hs []func(context.Context, *E) error) (coreDecision, error) {
	if len(hs) == 0 {
		return coreDecision{}, nil
	}
	var errs []error
	for _, h := range hs {
		if err := h(ctx, ev); err != nil {
			errs = append(errs, err)
		}
	}
	return coreDecision{decisionCore{kind: DecisionObserved}}, errors.Join(errs...)
}

// coreDecision adapts a bare decisionCore to the Decision view, for events
// whose handlers observe rather than decide.
type coreDecision struct{ core decisionCore }

func (d coreDecision) Kind() DecisionKind         { return d.core.kind }
func (d coreDecision) Reason() string             { return d.core.reason }
func (d coreDecision) SystemMessage() string      { return d.core.systemMessage }
func (d coreDecision) Context() []string          { return d.core.contextCopy() }
func (d coreDecision) Blocks() bool               { return d.core.blocks() }
func (d coreDecision) StopsAgent() (string, bool) { return d.core.stopsAgent() }
func (d coreDecision) decCore() decisionCore      { return d.core }

// Any composes handlers into one handler with the same semantics as stacked
// registration — one rule everywhere: handlers run in order, the first
// conclusive decision wins and short-circuits the rest, so order is
// priority. A handler error aborts immediately.
func Any[E any, D decision](hs ...func(context.Context, *E) (D, error)) func(context.Context, *E) (D, error) {
	return func(ctx context.Context, e *E) (D, error) {
		return runStages(ctx, e, hs)
	}
}

// All composes handlers into one handler that runs every handler — no
// short-circuit, so all findings and side effects are recorded — and then
// merges: the most restrictive kind wins (deny > ask > allow > neutral,
// ties to the earliest), Context appends from all decisions in order, the
// winner's other fields are taken wholesale, and StopAgent is sticky.
// Errors: every handler still runs, the errors are joined, and any error
// aborts the combinator.
func All[E any, D decision](hs ...func(context.Context, *E) (D, error)) func(context.Context, *E) (D, error) {
	return func(ctx context.Context, e *E) (D, error) {
		var zero D
		cores := make([]decisionCore, 0, len(hs))
		var errs []error
		for _, h := range hs {
			d, err := h(ctx, e)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			cores = append(cores, coreOf(d))
		}
		if err := errors.Join(errs...); err != nil {
			return zero, err
		}
		if len(cores) == 0 {
			return zero, nil
		}
		return fromCore[D](mergeCores(cores)), nil
	}
}

// Matcher guards handlers by tool identity (see When). ToolMatcher satisfies
// it; custom implementations (e.g. CEL-backed) plug in without library
// dependencies.
type Matcher interface {
	Matches(ToolCall) bool
}

// MatchTools matches exact native tool names (case-insensitive). With no
// arguments it matches every tool (ToolMatcher's empty-matcher semantics).
func MatchTools(names ...string) ToolMatcher { return ToolMatcher{Names: names} }

// MatchMCP matches MCP tools by "server/tool" glob: "server" alone means
// "server/*", and "*" matches any MCP tool.
func MatchMCP(globs ...string) ToolMatcher { return ToolMatcher{MCP: globs} }

// MatchCanonical matches canonical tool classes.
func MatchCanonical(classes ...CanonicalTool) ToolMatcher { return ToolMatcher{Canonical: classes} }

// When guards a handler: h runs only when the event carries a tool call the
// matcher matches; otherwise the stage is neutral. Events without a tool
// call (prompts, stops, ...) never match.
func When[E any, D decision](m Matcher, h func(context.Context, *E) (D, error)) func(context.Context, *E) (D, error) {
	return func(ctx context.Context, e *E) (D, error) {
		if tool := toolOf(e); tool == nil || !m.Matches(*tool) {
			var zero D
			return zero, nil
		}
		return h(ctx, e)
	}
}

// Next continues the pipeline from an interceptor: it runs the remaining
// interceptors and the typed handlers for the event and returns the winning
// decision.
type Next func(ctx context.Context, typed any) (Decision, error)

// Interceptor is router middleware. It receives the typed event (e.g.
// *ToolPreEvent) and the rest of the pipeline as next. It may transform the
// normalized projection in place (Tool.Input, prompt text — Raw and RawInput
// stay verbatim per the fidelity model), short-circuit by returning without
// calling next, or post-process next's decision. Interceptors call next at
// most once.
type Interceptor func(ctx context.Context, typed any, next Next) (Decision, error)

// Use installs middleware around the typed-handler pipeline, outermost
// first: the first interceptor installed sees the event first and the
// decision last. Middleware wraps typed handlers only — OnAny/OnOther
// observers run before it and never gate.
func (r *Runner) Use(i Interceptor) { r.interceptors = append(r.interceptors, i) }

// runPipeline runs the middleware chain around the typed-handler pipeline.
func (r *Runner) runPipeline(ctx context.Context, typed any) (Decision, error) {
	var next Next = r.invoke
	for i := len(r.interceptors) - 1; i >= 0; i-- {
		next = wrapInterceptor(r.interceptors[i], next)
	}
	return next(ctx, typed)
}

func wrapInterceptor(i Interceptor, next Next) Next {
	return func(ctx context.Context, typed any) (Decision, error) {
		// Interceptors call next at most once. Enforce it per invocation so a
		// second call errors instead of silently duplicating downstream
		// handler side effects.
		called := false
		guarded := func(ctx context.Context, typed any) (Decision, error) {
			if called {
				return nil, errors.New("agenthooks: interceptor called next more than once")
			}
			called = true
			return next(ctx, typed)
		}
		return i(ctx, typed, guarded)
	}
}

// coreOfDecision unwraps a Decision for the codecs, tolerating a nil
// decision from a misbehaving interceptor.
func coreOfDecision(d Decision) decisionCore {
	if d == nil {
		return decisionCore{}
	}
	return d.decCore()
}

// Decide runs the router pipeline for a typed event — OnAny/OnOther
// observers, middleware, then the registered handlers — and returns the
// winning decision. It is the server-side entry point: no wire encoding, no
// capability degradation (applyPolicy is an edge/wire concern — degrading an
// ask before the caller's boundary can render it would collapse it), and no
// MCP transport resolution. A stage error (panics included, converted to
// errors) returns as an error with a nil decision; the caller owns failure
// semantics. A neutral outcome is the zero decision:
// Kind() == DecisionNoDecision.
//
// The context deadline is honored even against handlers that ignore ctx; no
// default deadline is applied.
func (r *Runner) Decide(ctx context.Context, typed any) (Decision, error) {
	if isNilPtr(typed) || eventOf(typed) == nil {
		return nil, fmt.Errorf("agenthooks: Decide: %T is not an agenthooks event", typed)
	}
	ctx = withLogger(ctx, r.logger)
	d, err := r.decideGuarded(ctx, typed)
	if err != nil {
		return nil, err
	}
	// A misbehaving interceptor can return a nil Decision; normalize it to
	// the neutral zero decision so callers can inspect the result without a
	// nil-interface panic — the same normalization coreOfDecision applies on
	// the edge path.
	if d == nil {
		return coreDecision{}, nil
	}
	return d, nil
}

// isNilPtr reports whether typed is a typed-nil pointer (e.g. a nil
// (*ToolPreEvent)(nil)). eventOf dereferences recognized event pointers, so
// this guard keeps the exported Decide from panicking on a typed-nil event.
func isNilPtr(typed any) bool {
	if typed == nil {
		return false
	}
	v := reflect.ValueOf(typed)
	return v.Kind() == reflect.Ptr && v.IsNil()
}

// StageType classifies a stage visited by Walk.
type StageType string

const (
	StageMiddleware StageType = "middleware" // installed with Use
	StageObserver   StageType = "observer"   // OnAny / OnOther
	StageHandler    StageType = "handler"    // typed On<Kind> registration
)

// StageInfo describes one registered top-level stage.
type StageInfo struct {
	// Kind is the event kind the stage is registered for. It is empty for
	// middleware and OnAny observers (they see every event) and KindOther
	// for OnOther observers.
	Kind EventKind
	Type StageType
	// Native is the native event name an OnOther observer is registered for.
	// It is empty for every other stage (OnOther stages all report
	// Kind == KindOther, so the native name is the only way to tell them
	// apart in run-order output).
	Native string
	// Name is the reflected function name — useful for named funcs and
	// method values; anonymous closures report their compiler-assigned
	// closure names.
	Name string
	// Pos is the zero-based registration position within the stage's group.
	Pos int
}

// Walk visits every registered top-level stage in dispatch order: OnAny
// observers, OnOther observers (grouped by native name, sorted), middleware
// (outermost first), then typed handlers grouped by event kind in
// registration order. It stops at the first error and returns it.
//
// Combinator internals are opaque: a composition (Any/All/When) registered
// as one handler visits as one stage — bare funcs carry no metadata, and
// stage names are always the reflected function names. Register named funcs
// or method values where readable Walk output matters.
func (r *Runner) Walk(fn func(StageInfo) error) error {
	for i, h := range r.anyHandlers {
		if err := fn(StageInfo{Type: StageObserver, Name: stageName(h), Pos: i}); err != nil {
			return err
		}
	}
	names := make([]string, 0, len(r.otherByName))
	for name := range r.otherByName {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for i, h := range r.otherByName[name] {
			if err := fn(StageInfo{Kind: KindOther, Native: name, Type: StageObserver, Name: stageName(h), Pos: i}); err != nil {
				return err
			}
		}
	}
	for i, ic := range r.interceptors {
		if err := fn(StageInfo{Type: StageMiddleware, Name: stageName(ic), Pos: i}); err != nil {
			return err
		}
	}
	if err := walkStages(fn, KindSessionStart, r.hSessionStart); err != nil {
		return err
	}
	if err := walkStages(fn, KindSessionEnd, r.hSessionEnd); err != nil {
		return err
	}
	if err := walkStages(fn, KindMCPInventory, r.hMCPInventory); err != nil {
		return err
	}
	if err := walkStages(fn, KindPromptSubmitted, r.hPrompt); err != nil {
		return err
	}
	if err := walkStages(fn, KindToolPre, r.hToolPre); err != nil {
		return err
	}
	if err := walkStages(fn, KindToolPost, r.hToolPost); err != nil {
		return err
	}
	if err := walkStages(fn, KindToolError, r.hToolError); err != nil {
		return err
	}
	if err := walkStages(fn, KindPermission, r.hPermission); err != nil {
		return err
	}
	if err := walkStages(fn, KindStop, r.hStop); err != nil {
		return err
	}
	if err := walkStages(fn, KindSubagentStart, r.hSubagentStart); err != nil {
		return err
	}
	if err := walkStages(fn, KindSubagentStop, r.hSubagentStop); err != nil {
		return err
	}
	if err := walkStages(fn, KindCompactPre, r.hCompactPre); err != nil {
		return err
	}
	if err := walkStages(fn, KindCompactPost, r.hCompactPost); err != nil {
		return err
	}
	if err := walkStages(fn, KindNotification, r.hNotification); err != nil {
		return err
	}
	if err := walkStages(fn, KindFileEdited, r.hFileEdited); err != nil {
		return err
	}
	if err := walkStages(fn, KindModelRequest, r.hModelRequest); err != nil {
		return err
	}
	return walkStages(fn, KindModelResponse, r.hModelResponse)
}

func walkStages[H any](fn func(StageInfo) error, kind EventKind, hs []H) error {
	for i, h := range hs {
		if err := fn(StageInfo{Kind: kind, Type: StageHandler, Name: stageName(h), Pos: i}); err != nil {
			return err
		}
	}
	return nil
}

// stageName resolves a stage's display name: the reflected function name.
// Named funcs and method values read best; an anonymous closure (including
// combinator compositions) reports its compiler-assigned closure name.
func stageName(h any) string {
	v := reflect.ValueOf(h)
	if v.Kind() != reflect.Func {
		return ""
	}
	if f := runtime.FuncForPC(v.Pointer()); f != nil {
		return f.Name()
	}
	return ""
}
