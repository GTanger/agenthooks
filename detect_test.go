package agenthooks

import "testing"

// Generated configs place consumer-binary flags ahead of the sentinel
// ("mybinary --config=x agenthooks serve --provider=opencode"); the mode
// keyword must still be recognized there.
func TestParseArgsConsumerFlagsBeforeSentinel(t *testing.T) {
	t.Parallel()
	inv, err := parseArgs([]string{"--config=/etc/consumer.json", "agenthooks", "serve", "--provider=opencode"})
	if err != nil {
		t.Fatal(err)
	}
	if inv.mode != "serve" {
		t.Errorf("mode = %q, want %q", inv.mode, "serve")
	}
	if inv.provider != ProviderOpenCode {
		t.Errorf("provider = %q, want %q", inv.provider, ProviderOpenCode)
	}
	if inv.payload != "" {
		t.Errorf("payload = %q, want empty", inv.payload)
	}
}

func TestParseArgsNoSentinel(t *testing.T) {
	t.Parallel()
	inv, err := parseArgs([]string{"--provider=claude-code"})
	if err != nil {
		t.Fatal(err)
	}
	if inv.mode != "run" {
		t.Errorf("mode = %q, want %q", inv.mode, "run")
	}
	if inv.provider != ProviderClaudeCode {
		t.Errorf("provider = %q, want %q", inv.provider, ProviderClaudeCode)
	}
}

// One installed file, two runtimes. --provider=vscode-copilot is the default
// the Copilot CLI overrides with its own env; VS Code sets no marker, so its
// absence is the only signal available and it has to mean "leave the flag
// alone". Getting this backwards sends a whole runtime the wrong response
// schema, silently — both runtimes accept and ignore the other's body.
func TestDemoteVSCodeToCLI(t *testing.T) {
	// No COPILOT_* set: a VS Code session, flag obeyed.
	for _, v := range []string{"COPILOT_CLI", "COPILOT_PLUGIN_ROOT", "COPILOT_PLUGIN_DATA"} {
		t.Setenv(v, "")
	}
	if got := demoteVSCodeToCLI(ProviderVSCodeCopilot); got != ProviderVSCodeCopilot {
		t.Errorf("no COPILOT_* → %q, want %q", got, ProviderVSCodeCopilot)
	}

	t.Setenv("COPILOT_PLUGIN_ROOT", "/tmp/plugin")
	if got := demoteVSCodeToCLI(ProviderVSCodeCopilot); got != ProviderCopilotCLI {
		t.Errorf("COPILOT_PLUGIN_ROOT set → %q, want %q", got, ProviderCopilotCLI)
	}
	// Every other provider is untouched: Claude Code hooks run inside a
	// Copilot CLI session under CLAUDE_* compat vars, and demoting those would
	// hijack a provider that never shared the file.
	for _, p := range []Provider{ProviderClaudeCode, ProviderCopilotCLI, ProviderCursor, ""} {
		if got := demoteVSCodeToCLI(p); got != p {
			t.Errorf("demote(%q) = %q, want unchanged", p, got)
		}
	}
}
