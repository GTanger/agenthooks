package agenthooks

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMCPInventoryCompletesBeforeFirstTool(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(os.Getenv("HOME"), ".claude.json"), []byte(`{"mcpServers":{"remote":{"url":"https://mcp.example.com"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	reported := false
	r := New(WithDedupDir(t.TempDir()), WithoutMCPListFallback())
	r.OnMCPInventory(func(_ context.Context, event *MCPInventoryEvent) error {
		if len(event.Servers) != 1 || event.Servers[0].Name != "remote" {
			t.Fatalf("unexpected inventory: %#v", event.Servers)
		}
		if !event.Complete {
			t.Fatal("config-only inventory must be complete when CLI fallback is disabled")
		}
		reported = true
		return nil
	})
	r.OnToolPre(func(_ context.Context, _ *ToolPreEvent) (ToolPreDecision, error) {
		if !reported {
			t.Fatal("inventory handler must complete before the tool handler starts")
		}
		return NoDecision(), nil
	})

	payload := strings.NewReader(fmt.Sprintf(`{"session_id":"session-1","cwd":%q,"hook_event_name":"PreToolUse","tool_name":"mcp__remote__call","tool_input":{}}`, cwd))
	var stdout, stderr bytes.Buffer
	if code := r.Run(t.Context(), []string{"run", "--provider=claude-code", "--variant=cli"}, payload, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code %d: %s", code, stderr.String())
	}
	if !reported {
		t.Fatal("inventory was not reported")
	}
}

func TestMCPInventoryRetriesAfterHandlerFailure(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	r := New(WithDedupDir(t.TempDir()), WithoutMCPListFallback())
	attempts := 0
	r.OnMCPInventory(func(_ context.Context, _ *MCPInventoryEvent) error {
		attempts++
		if attempts == 1 {
			return errors.New("temporary failure")
		}
		return nil
	})
	r.OnToolPre(func(_ context.Context, _ *ToolPreEvent) (ToolPreDecision, error) {
		return NoDecision(), nil
	})

	payload := []byte(`{"session_id":"session-retry","hook_event_name":"PreToolUse","tool_name":"mcp__remote__call","tool_input":{}}`)
	runWith(t, r, claudeArgs(), payload)
	runWith(t, r, claudeArgs(), payload)
	if attempts != 2 {
		t.Fatalf("inventory attempts = %d, want 2", attempts)
	}
}

func TestMCPInventorySessionStartDoesNotWaitForProbe(t *testing.T) {
	isolateHome(t)
	cwd := t.TempDir()
	writeConfig(t, filepath.Join(cwd, ".mcp.json"), `{"mcpServers":{"configured":{"url":"https://configured.example.com/mcp"}}}`)
	stateDir := t.TempDir()
	r := New(WithDedupDir(stateDir))
	inventories := make(chan *MCPInventoryEvent, 2)
	toolStarted := make(chan struct{}, 1)
	r.OnMCPInventory(func(_ context.Context, event *MCPInventoryEvent) error {
		inventories <- event
		return nil
	})
	r.OnToolPre(func(_ context.Context, _ *ToolPreEvent) (ToolPreDecision, error) {
		toolStarted <- struct{}{}
		return NoDecision(), nil
	})

	launch := currentClaudeLaunchContext(cwd)
	cacheDir := filepath.Join(stateDir, "agenthooks-mcplist")
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cachePath := filepath.Join(cacheDir, launch.cacheKey()+".json")
	release, locked, err := tryMCPListLock(cachePath + ".lock")
	if err != nil || !locked {
		t.Fatalf("holding probe lock: locked=%v err=%v", locked, err)
	}
	released := false
	defer func() {
		if !released {
			release()
		}
	}()

	sessionPayload := []byte(fmt.Sprintf(
		`{"session_id":"session-warm","cwd":%q,"hook_event_name":"SessionStart","source":"startup"}`,
		cwd,
	))
	sessionDone := make(chan struct{})
	go func() {
		runWith(t, r, claudeArgs(), sessionPayload)
		close(sessionDone)
	}()
	select {
	case <-sessionDone:
	case <-time.After(500 * time.Millisecond):
		release()
		released = true
		<-sessionDone
		t.Fatal("SessionStart waited for the in-flight MCP probe")
	}
	var first *MCPInventoryEvent
	select {
	case first = <-inventories:
	case <-time.After(time.Second):
		t.Fatal("SessionStart did not emit an MCP inventory")
	}
	if first.Complete || len(first.Servers) != 1 || first.Servers[0].Name != "configured" {
		t.Fatalf("SessionStart inventory = %+v", first)
	}

	toolPayload := []byte(fmt.Sprintf(
		`{"session_id":"session-warm","cwd":%q,"hook_event_name":"PreToolUse","tool_name":"mcp__plugin__run","tool_input":{}}`,
		cwd,
	))
	toolDone := make(chan struct{})
	go func() {
		runWith(t, r, claudeArgs(), toolPayload)
		close(toolDone)
	}()
	select {
	case <-toolDone:
		t.Fatal("first MCP hook did not wait for the in-flight probe")
	case <-time.After(100 * time.Millisecond):
	}
	writeMCPListCache(cachePath, mcpListCache{
		CheckedAt:   time.Now().Unix(),
		Entries:     []mcpConfigEntry{{Name: "plugin", URL: "https://plugin.example.com/mcp"}},
		HasSnapshot: true,
	})
	release()
	released = true
	select {
	case <-toolDone:
	case <-time.After(5 * time.Second):
		t.Fatal("first MCP hook did not resume after the inventory completed")
	}
	var second *MCPInventoryEvent
	select {
	case second = <-inventories:
	case <-time.After(time.Second):
		t.Fatal("first MCP hook did not emit the completed inventory")
	}
	if !second.Complete || len(second.Servers) != 2 {
		t.Fatalf("first MCP hook inventory = %+v", second)
	}
	select {
	case <-toolStarted:
	default:
		t.Fatal("tool handler did not run after the complete inventory handler")
	}
}

func TestMCPInventoryPreservesClaudeConfigAfterProbeFailure(t *testing.T) {
	isolateHome(t)
	cwd := t.TempDir()
	writeConfig(t, filepath.Join(cwd, ".mcp.json"), `{"mcpServers":{"configured":{"url":"https://configured.example.com/mcp"}}}`)
	fake := installFakeClaude(t, "")
	writeConfig(t, fake.exitFile, "1")
	r := mcpTestRunner(t)
	entries, complete := r.effectiveMCPInventory(&Event{
		Provider: ProviderClaudeCode,
		Session:  SessionInfo{CWD: cwd},
	}, true)
	if complete || len(entries) != 1 || entries[0].Name != "configured" {
		t.Fatalf("Claude failed-probe inventory = %+v complete=%v", entries, complete)
	}
}

func TestMCPInventoryPreservesCodexConfigAfterProbeFailure(t *testing.T) {
	home := isolateHome(t)
	cwd := t.TempDir()
	writeConfig(t, filepath.Join(home, ".codex", "config.toml"), `[mcp_servers.configured]
url = "https://configured.example.com/mcp"
`)
	installFakeCodex(t, "not-json")
	launch := parseCodexLaunchArgs([]string{"codex"}, cwd)
	r := mcpTestRunner(t)
	r.codexLaunchContext = &launch
	entries, complete := r.effectiveMCPInventory(&Event{
		Provider: ProviderCodex,
		Session:  SessionInfo{CWD: cwd},
	}, true)
	if complete || len(entries) != 1 || entries[0].Name != "configured" {
		t.Fatalf("Codex failed-probe inventory = %+v complete=%v", entries, complete)
	}
}

func quietRunner(opts ...Option) *Runner {
	opts = append([]Option{WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))}, opts...)
	return New(opts...)
}

func runWith(t *testing.T, r *Runner, args []string, payload []byte) (string, int) {
	t.Helper()
	var out, errb bytes.Buffer
	code := r.Run(context.Background(), args, bytes.NewReader(payload), &out, &errb)
	return out.String(), code
}

func claudeArgs() []string { return []string{"agenthooks", "run", "--provider=claude-code"} }

func TestRunClaudeDeny(t *testing.T) {
	r := quietRunner()
	r.OnToolPre(func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
		if e.Tool.Canonical != ToolShell {
			t.Errorf("expected shell tool, got %+v", e.Tool)
		}
		return Deny("blocked"), nil
	})
	out, code := runWith(t, r, claudeArgs(), fixture(t, "claude/pre_tool_use.json"))
	want := `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"blocked"}}`
	if out != want || code != 0 {
		t.Errorf("got %q (exit %d), want %q (exit 0)", out, code, want)
	}
}

func TestRunFailModes(t *testing.T) {
	boom := func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
		return NoDecision(), errors.New("boom")
	}

	closed := quietRunner(WithPolicy(Policy{Fail: FailClosed}))
	closed.OnToolPre(boom)
	out, code := runWith(t, closed, claudeArgs(), fixture(t, "claude/pre_tool_use.json"))
	if code != 0 || !strings.Contains(out, `"permissionDecision":"deny"`) {
		t.Errorf("fail-closed should deny: %q (exit %d)", out, code)
	}

	open := quietRunner(WithPolicy(Policy{Fail: FailOpen}))
	open.OnToolPre(boom)
	out, code = runWith(t, open, claudeArgs(), fixture(t, "claude/pre_tool_use.json"))
	if code != 0 || out != "{}" {
		t.Errorf("fail-open should be a no-op: %q (exit %d)", out, code)
	}
}

func TestRunPanicRecovery(t *testing.T) {
	r := quietRunner(WithPolicy(Policy{Fail: FailClosed}))
	r.OnToolPre(func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
		panic("handler bug")
	})
	out, code := runWith(t, r, claudeArgs(), fixture(t, "claude/pre_tool_use.json"))
	if code != 0 || !strings.Contains(out, `"deny"`) {
		t.Errorf("panic must not leak garbage; got %q (exit %d)", out, code)
	}
}

func TestRunTimeout(t *testing.T) {
	r := quietRunner(WithPolicy(Policy{Fail: FailClosed, Timeout: 50 * time.Millisecond}))
	r.OnToolPre(func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
		time.Sleep(2 * time.Second) // ignores ctx on purpose
		return Allow(), nil
	})
	start := time.Now()
	out, _ := runWith(t, r, claudeArgs(), fixture(t, "claude/pre_tool_use.json"))
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("deadline not enforced: took %v", elapsed)
	}
	if !strings.Contains(out, `"deny"`) {
		t.Errorf("timeout under fail-closed should deny: %q", out)
	}
}

func TestAskDegradationOnCodex(t *testing.T) {
	ask := func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
		return AskUser("confirm?"), nil
	}
	args := []string{"agenthooks", "run", "--provider=codex"}
	payload := fixture(t, "codex/pre_tool_use.json")

	toDeny := quietRunner(WithPolicy(Policy{Unsupported: Degrade, AskFallback: FallbackDeny}))
	toDeny.OnToolPre(ask)
	out, _ := runWith(t, toDeny, args, payload)
	if !strings.Contains(out, `"permissionDecision":"deny"`) {
		t.Errorf("ask should degrade to deny: %q", out)
	}

	toNone := quietRunner(WithPolicy(Policy{Unsupported: Degrade, AskFallback: FallbackNoDecision}))
	toNone.OnToolPre(ask)
	out, _ = runWith(t, toNone, args, payload)
	if out != "" {
		t.Errorf("ask should degrade to codex empty-stdout no-op: %q", out)
	}

	strict := quietRunner(WithPolicy(Policy{Unsupported: Strict, Fail: FailClosed}))
	strict.OnToolPre(ask)
	out, _ = runWith(t, strict, args, payload)
	if !strings.Contains(out, `"deny"`) {
		t.Errorf("strict unsupported + fail-closed should deny: %q", out)
	}
}

func TestFilterFlag(t *testing.T) {
	deny := func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
		return Deny("no"), nil
	}
	payload := fixture(t, "claude/pre_tool_use.json") // Bash → shell

	miss := quietRunner()
	miss.OnToolPre(deny)
	out, _ := runWith(t, miss, append(claudeArgs(), "--filter=canonical=file.write"), payload)
	if out != "{}" {
		t.Errorf("filtered-out event must no-op without invoking the handler: %q", out)
	}

	hit := quietRunner()
	hit.OnToolPre(deny)
	out, _ = runWith(t, hit, append(claudeArgs(), "--filter=canonical=shell"), payload)
	if !strings.Contains(out, `"deny"`) {
		t.Errorf("matching filter must dispatch: %q", out)
	}
}

func TestOnAnyAndOnOther(t *testing.T) {
	r := quietRunner()
	var anyNative, otherNative string
	r.OnAny(func(ctx context.Context, e *Event) error {
		anyNative = e.NativeName
		return nil
	})
	r.OnOther("Setup", func(ctx context.Context, e *Event) error {
		otherNative = e.NativeName
		return nil
	})
	out, code := runWith(t, r, claudeArgs(), fixture(t, "claude/setup.json"))
	if out != "{}" || code != 0 {
		t.Errorf("unmapped event must no-op: %q (exit %d)", out, code)
	}
	if anyNative != "Setup" || otherNative != "Setup" {
		t.Errorf("observers not called: any=%q other=%q", anyNative, otherNative)
	}
}

func TestContinuationCap(t *testing.T) {
	cont := func(ctx context.Context, e *StopEvent) (StopDecision, error) {
		return ContinueWith("keep going"), nil
	}
	payload := fixture(t, "claude/stop.json") // stop_hook_active → LoopCount 1

	capped := quietRunner(WithPolicy(Policy{ContinuationCap: 1}))
	capped.OnStop(cont)
	out, _ := runWith(t, capped, claudeArgs(), payload)
	if out != "{}" {
		t.Errorf("cap reached: ContinueWith must degrade to Finish: %q", out)
	}

	free := quietRunner()
	free.OnStop(cont)
	out, _ = runWith(t, free, claudeArgs(), payload)
	if out != `{"decision":"block","reason":"keep going"}` {
		t.Errorf("under the cap ContinueWith must block: %q", out)
	}
}

func TestCursorDedup(t *testing.T) {
	calls := 0
	r := quietRunner(WithDedupDir(t.TempDir()))
	r.OnToolPre(func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
		calls++
		if e.NativeName == "beforeMCPExecution" && (e.Tool.MCP == nil || e.Tool.MCP.Server != "srv") {
			t.Errorf("specific MCP server identity = %+v", e.Tool.MCP)
		}
		return Deny("stop"), nil
	})
	args := []string{"agenthooks", "run", "--provider=cursor"}

	first, _ := runWith(t, r, args, fixture(t, "cursor/before_shell_execution.json"))
	sibling := []byte(`{"conversation_id":"conv-cursor-1","generation_id":"gen-5","hook_event_name":"preToolUse","workspace_roots":["/work/repo"],"tool_name":"Shell","tool_input":{"command":"git push origin main"}}`)
	second, _ := runWith(t, r, args, sibling)

	if !strings.Contains(first, `"permission":"deny"`) {
		t.Errorf("first arrival should decide: %q", first)
	}
	if second != "{}" {
		t.Errorf("duplicate sibling should no-op (quirk #2): %q", second)
	}
	if calls != 1 {
		t.Errorf("handler must run once, ran %d times", calls)
	}
}

func TestCursorDedupGenericMCPEchoDoesNotSuppressSpecific(t *testing.T) {
	calls := 0
	r := quietRunner(WithDedupDir(t.TempDir()))
	r.OnToolPre(func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
		calls++
		return Deny("stop"), nil
	})
	args := []string{"agenthooks", "run", "--provider=cursor"}

	// Cursor fires the generic MCP: echo before beforeMCPExecution. The echo
	// carries no server identity (quirk #3), so it must not claim the dedup
	// marker away from the specific sibling.
	generic := []byte(`{"conversation_id":"conv-mcp-1","generation_id":"gen-9","hook_event_name":"preToolUse","tool_name":"MCP:shadow_lookup","tool_input":{"marker":"x"}}`)
	specific := []byte(`{"conversation_id":"conv-mcp-1","generation_id":"gen-9","hook_event_name":"beforeMCPExecution","tool_name":"shadow_lookup","tool_input":"{\"marker\":\"x\"}","mcp_server_name":"srv","command":"node server.mjs"}`)

	if _, code := runWith(t, r, args, generic); code != 0 {
		t.Fatalf("generic echo run failed")
	}
	second, _ := runWith(t, r, args, specific)
	if !strings.Contains(second, `"permission":"deny"`) {
		t.Errorf("specific sibling must still gate after a generic-first echo: %q", second)
	}
	echoAgain, _ := runWith(t, r, args, generic)
	if echoAgain != "{}" {
		t.Errorf("generic echo after the specific processed should no-op: %q", echoAgain)
	}
	if calls != 2 {
		t.Errorf("handler calls = %d, want 2 (echo passes through, specific gates, trailing echo suppressed)", calls)
	}
}

func TestNotifyMode(t *testing.T) {
	r := quietRunner()
	var msg string
	r.OnNotification(func(ctx context.Context, e *NotificationEvent) error {
		msg = e.Message
		return nil
	})
	payload := `{"type":"agent-turn-complete","turn-id":"t-1","thread-id":"th-1","last-assistant-message":"done"}`
	out, code := runWith(t, r, []string{"agenthooks", "notify", "--provider=codex", payload}, nil)
	if code != 0 || out != "" {
		t.Errorf("notify mode should emit nothing on codex: %q (exit %d)", out, code)
	}
	if msg != "done" {
		t.Errorf("notification not delivered: %q", msg)
	}
}

func TestArgvPayloadMode(t *testing.T) {
	r := quietRunner()
	r.OnPromptSubmitted(func(ctx context.Context, e *PromptEvent) (PromptDecision, error) {
		return BlockPrompt("not now"), nil
	})
	payload := `{"conversation_id":"c1","generation_id":"g1","hook_event_name":"beforeSubmitPrompt","prompt":"hi"}`
	out, code := runWith(t, r, []string{"agenthooks", "run", "--provider=cursor", "--argv-payload", payload}, nil)
	if code != 0 || out != `{"continue":false,"user_message":"not now"}` {
		t.Errorf("argv-payload mode broken: %q (exit %d)", out, code)
	}
}

func TestUndetectableProviderNoOps(t *testing.T) {
	for _, v := range []string{
		"CURSOR_VERSION", "CURSOR_TRACE_ID", "CURSOR_AGENT", "CODEX_HOME", "CODEX_SANDBOX",
		"GEMINI_CWD", "GEMINI_CLI", "OPENCODE_SERVER", "OPENCODE", "CLAUDE_PROJECT_DIR", "CLAUDE_PLUGIN_ROOT",
	} {
		t.Setenv(v, "")
	}
	r := quietRunner()
	out, code := runWith(t, r, nil, []byte("not json at all"))
	if code != 0 || out != "{}" {
		t.Errorf("undetectable provider must emit a neutral no-op, got %q (exit %d)", out, code)
	}
}

func TestBadFlagsExit64(t *testing.T) {
	r := quietRunner()
	_, code := runWith(t, r, []string{"agenthooks", "run", "--provider=unknown-agent"}, nil)
	if code != 64 {
		t.Errorf("bad provider flag should exit 64, got %d", code)
	}
}
