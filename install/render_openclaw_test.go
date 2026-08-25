package install

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/speakeasy-api/agenthooks"
)

// shimConst extracts a `const NAME = <json>` literal from the generated shim.
func shimConst(t *testing.T, shim, name string) string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^const ` + name + ` = (.+)$`).FindStringSubmatch(shim)
	if m == nil {
		t.Fatalf("shim missing const %s", name)
	}
	return m[1]
}

func TestRenderOpenClawPlugin(t *testing.T) {
	fsys, err := Render(testManifest(), Target{Provider: agenthooks.ProviderOpenClaw, Scope: ScopeProject})
	if err != nil {
		t.Fatal(err)
	}

	var manifest struct {
		ID         string `json:"id"`
		Version    string `json:"version"`
		Activation struct {
			OnStartup bool `json:"onStartup"`
		} `json:"activation"`
	}
	if err := json.Unmarshal(readRendered(t, fsys, "openclaw.plugin.json"), &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.ID != "myhooks" || manifest.Version != "1.0.0" || !manifest.Activation.OnStartup {
		t.Errorf("plugin manifest wrong: %+v", manifest)
	}

	shim := string(readRendered(t, fsys, "index.js"))
	if !strings.Contains(shim, `"/usr/local/bin/myhooks"`) {
		t.Errorf("shim must bake in the command:\n%s", shim[:200])
	}
	if !strings.Contains(shim, `"agenthooks", "serve", "--provider=openclaw"`) {
		t.Error("shim must spawn serve mode")
	}

	// testManifest subscribes ToolPre, Stop, ToolPost — Stop pulls in
	// llm_output for the agent_end splice, gateway lifecycle is always
	// observed — and the prompt gate must NOT be subscribed. Parse the actual
	// HOOKS array so a renderer regression cannot hide in the script body.
	var hooks []string
	if err := json.Unmarshal([]byte(shimConst(t, shim, "HOOKS")), &hooks); err != nil {
		t.Fatal(err)
	}
	sort.Strings(hooks)
	want := []string{"after_tool_call", "agent_end", "before_tool_call", "gateway_start", "gateway_stop", "llm_output"}
	if strings.Join(hooks, ",") != strings.Join(want, ",") {
		t.Errorf("HOOKS = %v, want %v", hooks, want)
	}

	// The blocking ToolPre spec carries 30s; the per-gate deadline map must
	// honor it, and the serve invocation must carry the max gate deadline.
	var gates map[string]int64
	if err := json.Unmarshal([]byte(shimConst(t, shim, "GATE_TIMEOUT_MS")), &gates); err != nil {
		t.Fatal(err)
	}
	if len(gates) != 1 || gates["before_tool_call"] != 30000 {
		t.Errorf("gate timeouts wrong: %v", gates)
	}
	if !strings.Contains(shim, `"--timeout=30s"`) {
		t.Error("serve args must carry the max gate deadline")
	}
	if !strings.Contains(shim, "FAIL_CLOSED = true") {
		t.Error("fail mode must ride the shim (manifest is FailClosed)")
	}
	// An unreachable consumer must resolve gates as timed out (fail mode
	// applies), never as a silent allow, and fail-closed tool gates must
	// report the local block to the daemon.
	if !strings.Contains(shim, "return Promise.resolve({ timedOut: true })") {
		t.Error("unavailable child must resolve gates as timed out")
	}
	if !strings.Contains(shim, `call("gate_timeout", { toolCallId: event.toolCallId, reason }, null)`) {
		t.Error("fail-closed tool gate must report the local block to the daemon")
	}
	// A daemon-reported error fails closed with its own reason so decisions
	// and gate_timeout telemetry do not misdiagnose errors as timeouts.
	if !strings.Contains(shim, `"agenthooks: hook failed (fail-closed): " + reply.error`) {
		t.Error("a daemon-reported error must fail closed with an error-specific reason")
	}
	if !strings.Contains(shim, `"agenthooks: hook timed out (fail-closed)"`) {
		t.Error("a shim timeout must fail closed with the timeout reason")
	}
	if !strings.Contains(shim, "if (cached !== undefined) llmByRun.delete(sessionKey)") {
		t.Error("agent_end must consume exactly the llm_output cache key that served the splice")
	}

	// Gateway hooks hand plugins the full config incl. auth secrets; the shim
	// must forward only the allowlisted fields, and route every forwarded ctx
	// through the sanitizer.
	if !strings.Contains(shim, "return { port: ctx?.port, workspaceDir: ctx?.workspaceDir }") {
		t.Error("gateway ctx must be reduced to the allowlisted fields")
	}
	if strings.Contains(strings.ReplaceAll(shim, "sanitizeCtx(hook, ctx)", ""), "call(hook, event, ctx") {
		t.Error("every forwarded hook ctx must pass through sanitizeCtx")
	}
	// Package installs reject TypeScript entries; the shim must stay plain JS.
	if strings.Contains(shim, ": ChildProcess") || strings.Contains(shim, "COMMAND: string[]") {
		t.Error("shim must be plain JavaScript, not TypeScript")
	}

	var pkg struct {
		OpenClaw struct {
			Extensions []string `json:"extensions"`
		} `json:"openclaw"`
	}
	if err := json.Unmarshal(readRendered(t, fsys, "package.json"), &pkg); err != nil {
		t.Fatal(err)
	}
	if len(pkg.OpenClaw.Extensions) != 1 || pkg.OpenClaw.Extensions[0] != "./index.js" {
		t.Errorf("package.json must key plugin detection via openclaw.extensions: %+v", pkg.OpenClaw)
	}
}

func TestRenderOpenClawToolErrorSubscribesAfterToolCall(t *testing.T) {
	m := Manifest{
		Command:  []string{"/usr/local/bin/myhooks"},
		Hooks:    []HookSpec{{Kind: agenthooks.KindToolError}},
		Identity: Identity{Name: "errhooks", Version: "1.0.0"},
	}
	fsys, err := Render(m, Target{Provider: agenthooks.ProviderOpenClaw, Scope: ScopeProject})
	if err != nil {
		t.Fatal(err)
	}
	shim := string(readRendered(t, fsys, "index.js"))
	var hooks []string
	if err := json.Unmarshal([]byte(shimConst(t, shim, "HOOKS")), &hooks); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, h := range hooks {
		if h == "after_tool_call" {
			found = true
		}
	}
	if !found {
		t.Errorf("KindToolError must subscribe after_tool_call: %v", hooks)
	}
}
