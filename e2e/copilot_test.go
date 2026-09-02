package e2e

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/speakeasy-api/agenthooks"
	"github.com/speakeasy-api/agenthooks/install"
	"github.com/speakeasy-api/agenthooks/provider/copilot"
)

// copilotHome is a sandbox COPILOT_HOME (the documented override for the
// config/state directory, default ~/.copilot). Nothing is seeded into it:
// Copilot keeps credentials outside the home directory, so a completely empty
// one still runs authenticated — which also means user-scope hooks
// (hooks/hooks.json under Target.Dir) land here and never in the real
// ~/.copilot.
func copilotHome(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

// runCopilot drives one headless turn. --allow-all-tools is mandatory for
// non-interactive mode (Copilot refuses to run tools otherwise); it does NOT
// suppress hooks — preToolUse still fires and its deny is still enforced,
// which is what TestCopilotDeny relies on.
func runCopilot(t *testing.T, proj, home, prompt string, options ...string) {
	t.Helper()
	bin := requireE2E(t, "copilot")
	args := []string{"-p", prompt, "--allow-all-tools", "--no-color"}
	args = append(args, options...)
	if _, err := runAgent(t, proj, []string{"COPILOT_HOME=" + home}, bin, args...); err != nil {
		// Don't fail here: a crashed turn records no tool.pre, and
		// runToolTurn retries once with a fresh sandbox.
		t.Logf("copilot -p failed (runToolTurn retries if no events landed): %v", err)
	}
}

// TestCopilotEventFields verifies two things a real Copilot binary is the only
// witness for:
//
//  1. Shape-based event-name reconstruction. Copilot omits the event name from
//     every payload except permissionRequest (hookName) and notification
//     (hook_event_name), so copilotEventName rebuilds it from the payload
//     shape. If the reconstruction were wrong the kinds below would arrive
//     mislabelled (or as KindOther), and both requireKinds and the typed views
//     — which key off Event.NativeName — would reject them.
//  2. The typed views in provider/copilot still describe the wire. Every field
//     we type must be populated, and Extra (unknown-field capture) must be
//     empty: a non-empty Extra means real Copilot ships fields the structs
//     don't know about.
func TestCopilotEventFields(t *testing.T) {
	t.Parallel()
	requireE2E(t, "copilot")
	rec, proj := runToolTurn(t, func() (recorder, string) {
		rec := newRecorder(t, "")
		home := copilotHome(t)
		proj := t.TempDir()
		installHooks(t, rec, agenthooks.ProviderCopilotCLI, install.ScopeUser, home)
		runCopilot(t, proj, home, shellMarkerPrompt("copilot-marker.txt"))
		return rec, proj
	})
	evs := rec.events(t)
	requireKinds(t, evs,
		agenthooks.KindSessionStart,
		agenthooks.KindSessionEnd,
		agenthooks.KindPromptSubmitted,
		agenthooks.KindToolPre,
		agenthooks.KindToolPost,
		agenthooks.KindStop,
	)
	if !markerExists(proj, "copilot-marker.txt") {
		t.Error("marker missing: shell command did not run")
	}
	// Unlike Kimi's and Cursor's print modes (quirks #30, #31), Copilot's -p
	// DOES fire userPromptSubmitted — which is why backfill.go has no copilot
	// case. Both halves are asserted: a real, non-backfilled prompt event
	// carrying the real text, and nothing synthesized anywhere in the run. If
	// this ever fails, copilot needs adding to recoverPromptText in
	// backfill.go, not deleting from here.
	realPrompt := false
	for _, e := range evs {
		if e.Typed && e.Kind == string(agenthooks.KindPromptSubmitted) && !e.Backfilled {
			realPrompt = realPrompt || strings.Contains(e.Prompt, "copilot-marker.txt")
		}
	}
	if !realPrompt {
		t.Errorf("copilot -p fired no real userPromptSubmitted — prompt backfill support is missing for copilot (backfill.go covers only kimi and cursor); got:\n%s", summarize(evs))
	}
	requireNoBackfill(t, evs)

	for _, e := range ofKind(evs, agenthooks.KindSessionStart) {
		ev := &agenthooks.Event{Provider: agenthooks.ProviderCopilotCLI, NativeName: e.Native, Raw: e.Raw}
		in, ok := copilot.SessionStart(ev)
		if !ok {
			t.Fatalf("SessionStart view rejected native %q", e.Native)
		}
		// Source and InitialPrompt are what copilotEventName discriminates
		// sessionStart on: an empty pair would make the reconstruction fall
		// through to another event's branch.
		if in.SessionID == "" || in.CWD == "" || in.Timestamp == 0 {
			t.Errorf("SessionStart base fields incomplete: %+v (raw: %s)", in.Base, e.Raw)
		}
		if in.Source == "" || in.InitialPrompt == "" {
			t.Errorf("SessionStart discriminating fields empty — shape reconstruction is guessing: %+v (raw: %s)", in, e.Raw)
		}
		if len(in.Extra) > 0 {
			t.Errorf("SessionStart has unknown fields %v — structs incomplete (raw: %s)", keys(in.Extra), e.Raw)
		}
	}

	for _, e := range ofKind(evs, agenthooks.KindSessionEnd) {
		ev := &agenthooks.Event{Provider: agenthooks.ProviderCopilotCLI, NativeName: e.Native, Raw: e.Raw}
		in, ok := copilot.SessionEnd(ev)
		if !ok {
			t.Fatalf("SessionEnd view rejected native %q", e.Native)
		}
		if in.SessionID == "" || in.Timestamp == 0 || in.Reason == "" {
			t.Errorf("SessionEnd fields incomplete: %+v (raw: %s)", in, e.Raw)
		}
		if len(in.Extra) > 0 {
			t.Errorf("SessionEnd has unknown fields %v (raw: %s)", keys(in.Extra), e.Raw)
		}
	}

	for _, e := range ofKind(evs, agenthooks.KindPromptSubmitted) {
		if e.Backfilled {
			continue // no Raw to check: a backfill fabricates nothing
		}
		ev := &agenthooks.Event{Provider: agenthooks.ProviderCopilotCLI, NativeName: e.Native, Raw: e.Raw}
		in, ok := copilot.UserPromptSubmitted(ev)
		if !ok {
			t.Fatalf("UserPromptSubmitted view rejected native %q", e.Native)
		}
		if in.SessionID == "" || in.Prompt == "" {
			t.Errorf("UserPromptSubmitted fields incomplete: %+v (raw: %s)", in, e.Raw)
		}
		if len(in.Extra) > 0 {
			t.Errorf("UserPromptSubmitted has unknown fields %v (raw: %s)", keys(in.Extra), e.Raw)
		}
	}

	for _, e := range ofKind(evs, agenthooks.KindToolPre) {
		ev := &agenthooks.Event{Provider: agenthooks.ProviderCopilotCLI, NativeName: e.Native, Raw: e.Raw}
		in, ok := copilot.PreToolUse(ev)
		if !ok {
			t.Fatalf("PreToolUse view rejected native %q", e.Native)
		}
		if in.SessionID == "" || in.CWD == "" || in.ToolName == "" {
			t.Errorf("PreToolUse fields incomplete: %+v (raw: %s)", in, e.Raw)
		}
		// ToolArgs is raw because the shape moved under a stable name: a
		// JSON-encoded object string through CLI 1.0.80, a plain object from
		// 1.0.81. Which one the installed CLI sends is logged rather than
		// pinned; that both decode is what the view has to guarantee.
		if !isObjectOrEncodedObject(in.ToolArgs) {
			t.Errorf("PreToolUse toolArgs is neither an object nor a JSON-encoded object string — a third shape appeared: %s (raw: %s)", in.ToolArgs, e.Raw)
		}
		if len(in.Extra) > 0 {
			t.Errorf("PreToolUse has unknown fields %v (raw: %s)", keys(in.Extra), e.Raw)
		}
	}

	for _, e := range ofKind(evs, agenthooks.KindToolPost) {
		ev := &agenthooks.Event{Provider: agenthooks.ProviderCopilotCLI, NativeName: e.Native, Raw: e.Raw}
		in, ok := copilot.PostToolUse(ev)
		if !ok {
			t.Fatalf("PostToolUse view rejected native %q", e.Native)
		}
		// resultType is the field copilotEventName uses to split postToolUse
		// from postToolUseFailure, so an empty one collapses the two events.
		if in.ToolName == "" || in.ToolResult.ResultType == "" {
			t.Errorf("PostToolUse fields incomplete: %+v (raw: %s)", in, e.Raw)
		}
		if len(in.Extra) > 0 {
			t.Errorf("PostToolUse has unknown fields %v (raw: %s)", keys(in.Extra), e.Raw)
		}
	}

	for _, e := range ofKind(evs, agenthooks.KindStop) {
		ev := &agenthooks.Event{Provider: agenthooks.ProviderCopilotCLI, NativeName: e.Native, Raw: e.Raw}
		in, ok := copilot.AgentStop(ev)
		if !ok {
			t.Fatalf("AgentStop view rejected native %q", e.Native)
		}
		// stopReason present with no agent fields is exactly what separates
		// agentStop from subagentStop in copilotEventName.
		if in.StopReason == "" || in.TranscriptPath == "" {
			t.Errorf("AgentStop fields incomplete: %+v (raw: %s)", in, e.Raw)
		}
		if len(in.Extra) > 0 {
			t.Errorf("AgentStop has unknown fields %v (raw: %s)", keys(in.Extra), e.Raw)
		}
	}

	// Normalization: toolArgs lands as an object whichever shape it arrived
	// in, and the bash call classifies as canonical shell.
	for _, e := range typedToolPres(evs) {
		if e.Canonical == string(agenthooks.ToolShell) {
			return
		}
	}
	t.Errorf("no shell tool.pre normalized from copilot; got:\n%s", summarize(evs))
}

// isObjectOrEncodedObject accepts both toolArgs shapes the Copilot CLI has
// shipped: a plain JSON object (1.0.81+) and an object serialized into a JSON
// string (through 1.0.80). Anything else is a third shape nothing normalizes.
func isObjectOrEncodedObject(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return false
	}
	if trimmed[0] == '"' {
		var inner string
		if err := json.Unmarshal(trimmed, &inner); err != nil {
			return false
		}
		trimmed = bytes.TrimSpace([]byte(inner))
	}
	return len(trimmed) > 0 && trimmed[0] == '{' && json.Valid(trimmed)
}

// TestCopilotToolFailure requires a real failed file-view call to produce the
// native postToolUseFailure event rather than treating failure coverage as an
// optional side effect of denial.
func TestCopilotToolFailure(t *testing.T) {
	t.Parallel()
	requireE2E(t, "copilot")
	rec, _ := runToolTurn(t, func() (recorder, string) {
		rec := newRecorder(t, "")
		home := copilotHome(t)
		proj := t.TempDir()
		installHooks(t, rec, agenthooks.ProviderCopilotCLI, install.ScopeUser, home)
		runCopilot(t, proj, home, toolFailurePrompt())
		return rec, proj
	})
	evs := rec.events(t)
	requireKinds(t, evs, agenthooks.KindToolPre, agenthooks.KindToolError)
	viewTools := make(map[string]bool)
	for _, e := range typedToolPres(evs) {
		if e.Canonical == string(agenthooks.ToolFileRead) {
			viewTools[e.Tool] = true
		}
	}
	matchedViewFailure := false
	for _, e := range ofKind(evs, agenthooks.KindToolError) {
		ev := &agenthooks.Event{Provider: agenthooks.ProviderCopilotCLI, NativeName: e.Native, Raw: e.Raw}
		in, ok := copilot.PostToolUseFailure(ev)
		if !ok {
			t.Fatalf("PostToolUseFailure view rejected native %q", e.Native)
		}
		if in.Error == "" && in.ToolResult.TextResultForLM == "" {
			t.Errorf("PostToolUseFailure carries no error text: %+v (raw: %s)", in, e.Raw)
		}
		if viewTools[in.ToolName] {
			matchedViewFailure = true
		}
		if len(in.Extra) > 0 {
			t.Errorf("PostToolUseFailure has unknown fields %v (raw: %s)", keys(in.Extra), e.Raw)
		}
	}
	if !matchedViewFailure {
		t.Errorf("no failed view tool call recorded; got:\n%s", summarize(evs))
	}
}

// TestCopilotModifiedArgs proves Copilot executes modifiedArgs rather than the
// original shell command. This catches a wire-shape regression that unit tests
// can only show was serialized, not honored by the CLI.
func TestCopilotModifiedArgs(t *testing.T) {
	t.Parallel()
	requireE2E(t, "copilot")
	const original = "original-marker.txt"
	const rewritten = "rewritten-marker.txt"
	rec, proj := runToolTurn(t, func() (recorder, string) {
		rec := newRecorderWithConfig(t, recorderConfig{RewriteCommand: "touch " + rewritten})
		home := copilotHome(t)
		proj := t.TempDir()
		installHooks(t, rec, agenthooks.ProviderCopilotCLI, install.ScopeUser, home)
		runCopilot(t, proj, home, oneShotShellMarkerPrompt(original))
		return rec, proj
	})
	evs := rec.events(t)
	requireKinds(t, evs, agenthooks.KindToolPre, agenthooks.KindToolPost)
	rewrittenShell := false
	for _, e := range typedToolPres(evs) {
		if e.Canonical == string(agenthooks.ToolShell) && e.Rewritten {
			rewrittenShell = true
			break
		}
	}
	if !rewrittenShell {
		t.Errorf("no rewritten shell tool.pre recorded; got:\n%s", summarize(evs))
	}
	if markerExists(proj, original) {
		t.Error("original marker exists: Copilot ignored modifiedArgs")
	}
	if !markerExists(proj, rewritten) {
		t.Error("rewritten marker missing: Copilot did not execute modifiedArgs")
	}
}

// TestCopilotPluginScope runs the generated local-plugin layout through
// Copilot itself; renderer tests alone cannot prove --plugin-dir discovers and
// executes the hook file. User scope is exercised by the other tests.
func TestCopilotPluginScope(t *testing.T) {
	t.Parallel()
	requireE2E(t, "copilot")
	const marker = "plugin-scope-marker.txt"
	rec, proj := runToolTurn(t, func() (recorder, string) {
		rec := newRecorder(t, "")
		home := copilotHome(t)
		proj := t.TempDir()
		pluginDir := t.TempDir()
		installHooks(t, rec, agenthooks.ProviderCopilotCLI, install.ScopePlugin, pluginDir)
		runCopilot(t, proj, home, oneShotShellMarkerPrompt(marker), "--plugin-dir", pluginDir)
		return rec, proj
	})
	evs := rec.events(t)
	requireKinds(t, evs, agenthooks.KindToolPre, agenthooks.KindToolPost)
	if !markerExists(proj, marker) {
		t.Error("plugin-scope marker missing: generated hooks were not exercised")
	}
}

// TestCopilotDeny verifies the load-bearing rule of the whole Copilot codec
// (codec_copilot.go): preToolUse hooks are fail-closed on ANY non-zero exit,
// so the codec signals every verdict as exit 0 plus a stdout decision body and
// never through the exit code. The recorder returns Deny, the hook process
// exits 0, and the tool call must still be blocked — if Copilot only honored
// exit-code denials the marker would exist and the codec's whole design would
// be wrong. The deny also has to survive --allow-all-tools, which is passed on
// every run here.
func TestCopilotDeny(t *testing.T) {
	t.Parallel()
	requireE2E(t, "copilot")
	rec, proj := runToolTurn(t, func() (recorder, string) {
		rec := newRecorder(t, string(agenthooks.ToolShell))
		home := copilotHome(t)
		proj := t.TempDir()
		installHooks(t, rec, agenthooks.ProviderCopilotCLI, install.ScopeUser, home)
		runCopilot(t, proj, home, shellMarkerPrompt("denied-marker.txt"))
		return rec, proj
	})
	evs := rec.events(t)
	requireKinds(t, evs, agenthooks.KindToolPre)
	deniedShell := false
	for _, e := range typedToolPres(evs) {
		if e.Canonical == string(agenthooks.ToolShell) && e.Denied {
			deniedShell = true
			break
		}
	}
	if !deniedShell {
		t.Errorf("no denied shell tool.pre recorded; got:\n%s", summarize(evs))
	}
	if markerExists(proj, "denied-marker.txt") {
		t.Error("marker exists: deny decision did not block the shell command on copilot")
	}

	// A blocked call may or may not produce a postToolUseFailure; when it
	// does, the failure view has to hold up too.
	for _, e := range ofKind(evs, agenthooks.KindToolError) {
		ev := &agenthooks.Event{Provider: agenthooks.ProviderCopilotCLI, NativeName: e.Native, Raw: e.Raw}
		in, ok := copilot.PostToolUseFailure(ev)
		if !ok {
			t.Fatalf("PostToolUseFailure view rejected native %q", e.Native)
		}
		if in.Error == "" && in.ToolResult.TextResultForLM == "" {
			t.Errorf("PostToolUseFailure carries no error text: %+v (raw: %s)", in, e.Raw)
		}
		if len(in.Extra) > 0 {
			t.Errorf("PostToolUseFailure has unknown fields %v (raw: %s)", keys(in.Extra), e.Raw)
		}
	}
}

// TestCopilotPascalCaseCompat drives the file rendered for VS Code
// (agenthooks-vscode.json, PascalCase event keys) with the real Copilot CLI.
// VS Code and the CLI glob the same two hook directories, so this file is
// loaded by both whether we like it or not — the design bets that the CLI's
// documented Claude-compat mode makes that harmless. Three things only the
// real binary can witness:
//
//  1. WHICH of VS Code's eight PascalCase names the CLI actually registers.
//     The CLI docs confirm the mode for PreToolUse and never enumerate the
//     rest; UserPromptSubmit is the suspect one, since the CLI's native name
//     is userPromptSubmitted. An unregistered name is silent — the hook
//     simply never fires — so the recorded kinds are the answer. The t.Logf
//     below is the output to read.
//  2. The COPILOT_* demotion fires in a real CLI hook child. Every event must
//     be stamped copilot-cli, not vscode-copilot: the flag says vscode-copilot
//     and only the CLI's own env overrides it.
//  3. The CLI honors a decision returned for a PascalCase registration, in
//     the FLAT schema encodeCopilot emits. A body in VS Code's nested
//     placement would be accepted and ignored, so a passing deny is the only
//     proof the demotion picked the right encoder.
func TestCopilotPascalCaseCompat(t *testing.T) {
	t.Parallel()
	requireE2E(t, "copilot")

	rec, proj := runToolTurn(t, func() (recorder, string) {
		rec := newRecorder(t, "")
		home := copilotHome(t)
		proj := t.TempDir()
		installHooks(t, rec, agenthooks.ProviderVSCodeCopilot, install.ScopeUser, home)
		runCopilot(t, proj, home, shellMarkerPrompt("pascal-marker.txt"))
		return rec, proj
	})
	evs := rec.events(t)
	// sessionEnd and postToolUseFailure are deliberately absent: they have no
	// VS Code counterpart, so kindToVSCode never renders them and the CLI
	// cannot fire what was never registered. That is the documented cost of
	// the PascalCase file versus the camelCase one.
	requireKinds(t, evs,
		agenthooks.KindSessionStart,
		agenthooks.KindPromptSubmitted,
		agenthooks.KindToolPre,
		agenthooks.KindToolPost,
		agenthooks.KindStop,
	)
	registered := map[string]bool{}
	for _, e := range evs {
		if e.Native != "" {
			registered[e.Native] = true
		}
	}
	t.Logf("PascalCase names the Copilot CLI registered: %v", keys(registered))
	if !markerExists(proj, "pascal-marker.txt") {
		t.Error("marker missing: shell command did not run under the PascalCase file")
	}
	for _, e := range evs {
		if e.Provider != string(agenthooks.ProviderCopilotCLI) {
			t.Errorf("event %s stamped provider %q, want %q — the COPILOT_* demotion did not fire, so this session got VS Code's capability row and nested encoder", e.Native, e.Provider, agenthooks.ProviderCopilotCLI)
		}
	}

	// Second turn, denying: a nested body would be silently ignored here.
	denyRec, denyProj := runToolTurn(t, func() (recorder, string) {
		rec := newRecorder(t, string(agenthooks.ToolShell))
		home := copilotHome(t)
		proj := t.TempDir()
		installHooks(t, rec, agenthooks.ProviderVSCodeCopilot, install.ScopeUser, home)
		runCopilot(t, proj, home, shellMarkerPrompt("pascal-denied-marker.txt"))
		return rec, proj
	})
	denyEvs := denyRec.events(t)
	requireKinds(t, denyEvs, agenthooks.KindToolPre)
	deniedShell := false
	for _, e := range typedToolPres(denyEvs) {
		if e.Canonical == string(agenthooks.ToolShell) && e.Denied {
			deniedShell = true
			break
		}
	}
	if !deniedShell {
		t.Errorf("no denied shell tool.pre recorded; got:\n%s", summarize(denyEvs))
	}
	if markerExists(denyProj, "pascal-denied-marker.txt") {
		t.Error("marker exists: the flat deny was not honored for a PascalCase registration")
	}
}

// TestCopilotStopContinuation drives the one capability the Copilot row claims
// on agent.stop — CapContinueAgent — through a real turn. It was previously
// read off the shipped binary's executeStopHook and nothing else, so a wire
// change (or a wrong schema) would have been invisible: the CLI ignores an
// unrecognized stop body silently and simply finishes.
//
// The proof is the second marker. The recorder returns ContinueWith once, and
// only the CLI feeding that instruction back into the model can create it.
// The second stop's guard fields are asserted too: Copilot reports
// stop_hook_active, so PreviouslyContinued/LoopCount must be populated on the
// continued turn — that is what keeps Policy.ContinuationCap load-bearing
// rather than decorative here.
func TestCopilotStopContinuation(t *testing.T) {
	t.Parallel()
	requireE2E(t, "copilot")
	const first = "stop-first-marker.txt"
	const continued = "stop-continued-marker.txt"
	rec, proj := runToolTurn(t, func() (recorder, string) {
		rec := newRecorderWithConfig(t, recorderConfig{ContinueInstruction: oneShotShellMarkerPrompt(continued)})
		home := copilotHome(t)
		proj := t.TempDir()
		installHooks(t, rec, agenthooks.ProviderCopilotCLI, install.ScopeUser, home)
		runCopilot(t, proj, home, oneShotShellMarkerPrompt(first))
		return rec, proj
	})
	evs := rec.events(t)
	requireKinds(t, evs, agenthooks.KindStop)
	if !markerExists(proj, first) {
		t.Error("first marker missing: the turn never ran its own shell command")
	}
	if !markerExists(proj, continued) {
		t.Errorf("continuation marker missing: the CLI did not act on the ContinueWith instruction, so CapContinueAgent is not honored on agentStop; got:\n%s", summarize(evs))
	}
	var stops []event
	for _, e := range evs {
		if e.Typed && e.Kind == string(agenthooks.KindStop) {
			stops = append(stops, e)
		}
	}
	if len(stops) < 2 {
		t.Fatalf("want at least two agent.stop deliveries (the original turn and the continued one), got %d:\n%s", len(stops), summarize(evs))
	}
	if !stops[0].Continued {
		t.Errorf("first stop did not return a continuation: %+v", stops[0])
	}
	last := stops[len(stops)-1]
	if !last.PrevContinued || last.LoopCount < 1 {
		t.Errorf("continued stop reports no native guard (PreviouslyContinued=%v LoopCount=%d) — Copilot stopped sending stop_hook_active, so the library-side continuation cap is now the only loop bound", last.PrevContinued, last.LoopCount)
	}
}

// subagentPrompt delegates to the CLI's `task` tool, which is what fires
// subagentStart/subagentStop. The nested agent is told to answer in one word:
// the delegation is the point, not the work.
func subagentPrompt() string {
	return "Use the task tool exactly once to delegate this to a subagent: reply with the single word ok. " +
		"Do not use any other tool."
}

// TestCopilotSubagentEvents measures subagentStart and subagentStop, which the
// codec previously mapped from the CLI's bundled sources alone (subagentStart
// in particular has no explicit event name on the wire, so copilotEventName
// reconstructs it from `agentName` — an inference until this test).
//
// Both runtimes are driven, because one file's registrations do not predict
// the other's dialect. The camelCase CLI file gets camelCase payloads for
// both events; the PascalCase (VS Code) file gets Claude-shaped SubagentStop
// but a NATIVE camelCase subagentStart — the CLI's Claude-compat translation
// does not cover it. That mix is asserted below: it is exactly the kind of
// half-translation that would silently drop an event to KindOther.
func TestCopilotSubagentEvents(t *testing.T) {
	t.Parallel()
	requireE2E(t, "copilot")
	for _, tc := range []struct {
		name              string
		provider          agenthooks.Provider
		startName, opName string
	}{
		{"camelCase", agenthooks.ProviderCopilotCLI, "subagentStart", "subagentStop"},
		{"pascalCase", agenthooks.ProviderVSCodeCopilot, "subagentStart", "SubagentStop"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec, _ := runToolTurn(t, func() (recorder, string) {
				rec := newRecorder(t, "")
				home := copilotHome(t)
				proj := t.TempDir()
				installHooks(t, rec, tc.provider, install.ScopeUser, home)
				runCopilot(t, proj, home, subagentPrompt())
				return rec, proj
			})
			evs := rec.events(t)
			requireKinds(t, evs, agenthooks.KindSubagentStart, agenthooks.KindSubagentStop)
			for _, e := range ofKind(evs, agenthooks.KindSubagentStart) {
				if e.Native != tc.startName {
					t.Errorf("subagent.start native = %q, want %q — the CLI changed which dialect it sends for this registration", e.Native, tc.startName)
				}
			}
			for _, e := range ofKind(evs, agenthooks.KindSubagentStop) {
				if e.Native != tc.opName {
					t.Errorf("subagent.stop native = %q, want %q — the CLI changed which dialect it sends for this registration", e.Native, tc.opName)
				}
			}
			if tc.provider != agenthooks.ProviderCopilotCLI {
				return // views below are the camelCase wire only
			}
			for _, e := range ofKind(evs, agenthooks.KindSubagentStart) {
				ev := &agenthooks.Event{Provider: agenthooks.ProviderCopilotCLI, NativeName: e.Native, Raw: e.Raw}
				in, ok := copilot.SubagentStart(ev)
				if !ok {
					t.Fatalf("SubagentStart view rejected native %q", e.Native)
				}
				// AgentName is the sole discriminator copilotEventName has for
				// this event; empty means the reconstruction was luck.
				if in.SessionID == "" || in.AgentName == "" {
					t.Errorf("SubagentStart fields incomplete: %+v (raw: %s)", in, e.Raw)
				}
				if len(in.Extra) > 0 {
					t.Errorf("SubagentStart has unknown fields %v (raw: %s)", keys(in.Extra), e.Raw)
				}
			}
			for _, e := range ofKind(evs, agenthooks.KindSubagentStop) {
				ev := &agenthooks.Event{Provider: agenthooks.ProviderCopilotCLI, NativeName: e.Native, Raw: e.Raw}
				in, ok := copilot.SubagentStop(ev)
				if !ok {
					t.Fatalf("SubagentStop view rejected native %q", e.Native)
				}
				// AgentID/AgentType are what separate subagentStop from
				// agentStop: both carry stopReason and nothing else does.
				if in.AgentID == "" || in.AgentType == "" || in.StopReason == "" {
					t.Errorf("SubagentStop fields incomplete: %+v (raw: %s)", in, e.Raw)
				}
				if len(in.Extra) > 0 {
					t.Errorf("SubagentStop has unknown fields %v (raw: %s)", keys(in.Extra), e.Raw)
				}
			}
		})
	}
}

// TestCopilotPreCompact measures preCompact in both runtimes. `/compact` is
// accepted as a headless prompt and the hook fires BEFORE the CLI decides
// there is nothing to compact, so a fresh session drives the event for free —
// the turn ends in "Nothing to compact" and spends no model tokens.
//
// Unlike the subagent events, both runtimes translate this one: the PascalCase
// registration yields a Claude-shaped PreCompact payload. Note what this test
// does NOT establish — capMatrix[vscode-copilot][KindCompactPre] is a row about
// what VS Code honors on a decision, and the CLI running the PascalCase file
// gets the copilot row instead (demotion, quirk #46). That row stays inferred
// until a real VS Code session drives it.
func TestCopilotPreCompact(t *testing.T) {
	t.Parallel()
	requireE2E(t, "copilot")
	for _, tc := range []struct {
		name     string
		provider agenthooks.Provider
		native   string
	}{
		{"camelCase", agenthooks.ProviderCopilotCLI, "preCompact"},
		{"pascalCase", agenthooks.ProviderVSCodeCopilot, "PreCompact"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := newRecorder(t, "")
			home := copilotHome(t)
			installHooks(t, rec, tc.provider, install.ScopeUser, home)
			runCopilot(t, t.TempDir(), home, "/compact")
			evs := rec.events(t)
			requireKinds(t, evs, agenthooks.KindCompactPre)
			for _, e := range ofKind(evs, agenthooks.KindCompactPre) {
				if e.Native != tc.native {
					t.Errorf("compact.pre native = %q, want %q", e.Native, tc.native)
				}
				if e.Provider != string(agenthooks.ProviderCopilotCLI) {
					t.Errorf("compact.pre stamped provider %q, want %q", e.Provider, agenthooks.ProviderCopilotCLI)
				}
				if tc.provider != agenthooks.ProviderCopilotCLI {
					continue // camelCase view only
				}
				ev := &agenthooks.Event{Provider: agenthooks.ProviderCopilotCLI, NativeName: e.Native, Raw: e.Raw}
				in, ok := copilot.PreCompact(ev)
				if !ok {
					t.Fatalf("PreCompact view rejected native %q", e.Native)
				}
				// Trigger is the only field copilotEventName can discriminate
				// preCompact on, and an explicit /compact must report "manual".
				if in.Trigger != "manual" {
					t.Errorf("PreCompact trigger = %q, want \"manual\" (raw: %s)", in.Trigger, e.Raw)
				}
				if len(in.Extra) > 0 {
					t.Errorf("PreCompact has unknown fields %v (raw: %s)", keys(in.Extra), e.Raw)
				}
			}
		})
	}
}
