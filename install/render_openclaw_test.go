package install

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/speakeasy-api/agenthooks"
)

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
	// llm_output for the agent_end splice; the prompt gate is not subscribed.
	for _, want := range []string{"before_tool_call", "after_tool_call", "agent_end", "llm_output"} {
		if !strings.Contains(shim, `"`+want+`"`) {
			t.Errorf("shim must subscribe %q", want)
		}
	}
	if strings.Contains(shim, `"before_agent_run",`) {
		t.Error("shim must not subscribe hooks the manifest omits")
	}
	// Blocking ToolPre spec carries 30s: the shim gate deadline honors it.
	if !strings.Contains(shim, "GATE_TIMEOUT_MS = 30000") {
		t.Error("gate timeout must come from the blocking spec")
	}
	if !strings.Contains(shim, "FAIL_CLOSED = true") {
		t.Error("fail mode must ride the shim (manifest is FailClosed)")
	}
	// Gateway hooks hand plugins the full config incl. auth secrets; the shim
	// must sanitize before forwarding.
	if !strings.Contains(shim, "sanitizeCtx") {
		t.Error("shim must sanitize gateway hook contexts")
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
