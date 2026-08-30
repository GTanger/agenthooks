package install

import (
	"encoding/json"
	"testing"

	"github.com/speakeasy-api/agenthooks"
)

func TestRenderVSCodePowerShellCommand(t *testing.T) {
	m := Manifest{
		Command: []string{`C:\Program Files\Agent Hooks\hook.exe`, `arg's`},
		Hooks:   []HookSpec{{Kind: agenthooks.KindStop}},
	}
	fsys, err := renderVSCode(m, Target{Scope: ScopeProject})
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Hooks map[string][]struct {
			Windows string `json:"windows"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(readRendered(t, fsys, ".github/hooks/agenthooks-vscode.json"), &cfg); err != nil {
		t.Fatal(err)
	}
	const want = `& 'C:\Program Files\Agent Hooks\hook.exe' 'arg''s' 'agenthooks' 'run' '--provider=vscode-copilot'`
	if got := cfg.Hooks["Stop"][0].Windows; got != want {
		t.Errorf("windows command = %q, want %q", got, want)
	}
}
