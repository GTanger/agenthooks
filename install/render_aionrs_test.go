package install

import (
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/speakeasy-api/agenthooks"
)

func TestRenderAionrsCoalescesPostAndPreservesMatchers(t *testing.T) {
	matcher := ToolMatcher{Names: []string{"tra_capability", "mcp__tra__tra_capability"}, MCP: []string{"tra/*"}}
	manifest := Manifest{
		Command: []string{"/opt/TAS Hooks/tas-hooks"},
		Hooks: []HookSpec{
			{Kind: agenthooks.KindPromptSubmitted, Timeout: 10 * time.Second},
			{Kind: agenthooks.KindToolPost, Tools: matcher, Timeout: 10 * time.Second},
			{Kind: agenthooks.KindToolError, Tools: matcher, Timeout: 10 * time.Second},
			{Kind: agenthooks.KindCompactPre, Timeout: 15 * time.Second},
			{Kind: agenthooks.KindStop, Timeout: 15 * time.Second},
		},
		Identity: Identity{Name: "tas-hooks"},
	}

	rendered, err := renderAionrs(manifest, Target{Provider: agenthooks.ProviderAionrs, Scope: ScopeUser})
	if err != nil {
		t.Fatal(err)
	}
	data, err := fs.ReadFile(rendered, "config.toml")
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, table := range []string{"prompt_submitted", "post_tool_use", "compact_pre", "stop"} {
		if !strings.Contains(text, "[[hooks."+table+"]]") {
			t.Fatalf("missing %s table:\n%s", table, text)
		}
	}
	if strings.Count(text, "[[hooks.post_tool_use]]") != 1 {
		t.Fatalf("post/error must share one native hook:\n%s", text)
	}
	if !strings.Contains(text, `tool_match = ["tra_capability", "mcp__tra__tra_capability"]`) {
		t.Fatalf("native matcher missing:\n%s", text)
	}
	if !strings.Contains(text, `agenthooks run --provider=aionrs --event=PostToolUse`) || !strings.Contains(text, `--filter=`) {
		t.Fatalf("launcher/filter missing:\n%s", text)
	}
	if !strings.Contains(text, "timeout_ms = 15000") {
		t.Fatalf("millisecond timeout missing:\n%s", text)
	}
}

func TestRenderAionrsProjectPath(t *testing.T) {
	rendered, err := renderAionrs(
		Manifest{Command: []string{"consumer"}, Hooks: []HookSpec{{Kind: agenthooks.KindStop}}},
		Target{Provider: agenthooks.ProviderAionrs, Scope: ScopeProject},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fs.Stat(rendered, ".aionrs.toml"); err != nil {
		t.Fatalf("project config missing: %v", err)
	}
}
