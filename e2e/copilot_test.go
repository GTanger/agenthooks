package e2e

import (
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
		installHooks(t, rec, agenthooks.ProviderCopilot, install.ScopeUser, home)
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
		ev := &agenthooks.Event{Provider: agenthooks.ProviderCopilot, NativeName: e.Native, Raw: e.Raw}
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
		ev := &agenthooks.Event{Provider: agenthooks.ProviderCopilot, NativeName: e.Native, Raw: e.Raw}
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
		ev := &agenthooks.Event{Provider: agenthooks.ProviderCopilot, NativeName: e.Native, Raw: e.Raw}
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
		ev := &agenthooks.Event{Provider: agenthooks.ProviderCopilot, NativeName: e.Native, Raw: e.Raw}
		in, ok := copilot.PreToolUse(ev)
		if !ok {
			t.Fatalf("PreToolUse view rejected native %q", e.Native)
		}
		if in.SessionID == "" || in.CWD == "" || in.ToolName == "" {
			t.Errorf("PreToolUse fields incomplete: %+v (raw: %s)", in, e.Raw)
		}
		// ToolArgs is typed as a string on purpose: Copilot double-encodes
		// the arguments here (a JSON object serialized into a JSON string),
		// unlike permissionRequest's plain toolInput object.
		if !strings.HasPrefix(in.ToolArgs, "{") {
			t.Errorf("PreToolUse toolArgs is not a JSON-encoded object string — the double-encoding quirk changed: %q (raw: %s)", in.ToolArgs, e.Raw)
		}
		if len(in.Extra) > 0 {
			t.Errorf("PreToolUse has unknown fields %v (raw: %s)", keys(in.Extra), e.Raw)
		}
	}

	for _, e := range ofKind(evs, agenthooks.KindToolPost) {
		ev := &agenthooks.Event{Provider: agenthooks.ProviderCopilot, NativeName: e.Native, Raw: e.Raw}
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
		ev := &agenthooks.Event{Provider: agenthooks.ProviderCopilot, NativeName: e.Native, Raw: e.Raw}
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

	// Normalization: toolArgs un-stringifies and the bash call classifies as
	// canonical shell.
	for _, e := range typedToolPres(evs) {
		if e.Canonical == string(agenthooks.ToolShell) {
			return
		}
	}
	t.Errorf("no shell tool.pre normalized from copilot; got:\n%s", summarize(evs))
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
		installHooks(t, rec, agenthooks.ProviderCopilot, install.ScopeUser, home)
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
		ev := &agenthooks.Event{Provider: agenthooks.ProviderCopilot, NativeName: e.Native, Raw: e.Raw}
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
		installHooks(t, rec, agenthooks.ProviderCopilot, install.ScopeUser, home)
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
		installHooks(t, rec, agenthooks.ProviderCopilot, install.ScopePlugin, pluginDir)
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
		installHooks(t, rec, agenthooks.ProviderCopilot, install.ScopeUser, home)
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
		ev := &agenthooks.Event{Provider: agenthooks.ProviderCopilot, NativeName: e.Native, Raw: e.Raw}
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
