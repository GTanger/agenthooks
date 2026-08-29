package install

import (
	"errors"
	"io/fs"
	"path"

	"github.com/speakeasy-api/agenthooks"
)

// kindToVSCode maps unified kinds to the eight PascalCase events Copilot Chat
// fires in VS Code (HOOKS_BY_TARGET[Target.VSCode] upstream). The six Copilot
// CLI events with no VS Code counterpart — sessionEnd, userPromptTransformed,
// postToolUseFailure, errorOccurred, permissionRequest, notification — are
// absent, and are reachable only through the camelCase CLI file that
// render_copilot.go writes.
var kindToVSCode = map[agenthooks.EventKind]string{
	agenthooks.KindSessionStart:    "SessionStart",
	agenthooks.KindPromptSubmitted: "UserPromptSubmit",
	agenthooks.KindToolPre:         "PreToolUse",
	agenthooks.KindToolPost:        "PostToolUse",
	agenthooks.KindStop:            "Stop",
	agenthooks.KindSubagentStart:   "SubagentStart",
	agenthooks.KindSubagentStop:    "SubagentStop",
	agenthooks.KindCompactPre:      "PreCompact",
}

// vscodeHookEntry is one command entry, with three deliberate omissions and
// one deliberate duplication:
//
//   - No matcher: VS Code parses matcher values and then ignores them, so a
//     key here would read as enforcement that does not exist. --filter on the
//     rendered argv is the only true enforcement (hookCommand emits it for
//     every non-empty matcher, because CompileMatcher has no VS Code dialect).
//   - No version: no VS Code example carries one, and an unknown key is a
//     schema-validation risk for zero benefit. That is also what keeps this
//     file distinguishable from render_copilot.go's CLI document.
//   - No bash/powershell: VS Code's platform split is windows/linux/osx, and
//     the rendered argv (absolute path plus flags) is valid in every shell.
//   - Both timeout spellings, same value: the VS Code reference table says
//     `timeout` while a usage example on the same doc set says `timeoutSec`,
//     and nothing reconciles them. Reading the wrong one silently falls back
//     to the 30s default; two JSON keys cost nothing.
type vscodeHookEntry struct {
	Type       string `json:"type"`
	Command    string `json:"command"`
	Timeout    int    `json:"timeout,omitempty"`
	TimeoutSec int    `json:"timeoutSec,omitempty"`
}

func renderVSCode(m Manifest, t Target) (fs.FS, error) {
	if t.Scope == ScopePlugin {
		return nil, errors.New("install: vscode-copilot has no plugin scope; install at user or project scope")
	}
	hooks := map[string][]vscodeHookEntry{}
	for _, spec := range m.Hooks {
		event, ok := kindToVSCode[spec.Kind]
		if !ok {
			continue
		}
		secs := timeoutSeconds(spec)
		hooks[event] = append(hooks[event], vscodeHookEntry{
			Type:       "command",
			Command:    hookCommand(m, agenthooks.ProviderVSCodeCopilot, spec),
			Timeout:    secs,
			TimeoutSec: secs,
		})
	}
	content, err := jsonFile(map[string]any{"hooks": hooks})
	if err != nil {
		return nil, err
	}
	// Both directories are globbed by VS Code AND by the Copilot CLI, so the
	// basename is what keeps this file distinct from the CLI's. It is neither
	// settings.json nor hooks.json, so isMergeableJSON says no and the file is
	// whole-file owned — the same posture render_copilot.go's project file has.
	files := map[string][]byte{}
	if t.Scope == ScopeProject {
		files[path.Join(".github", "hooks", "agenthooks-vscode.json")] = content
	} else {
		files[path.Join("hooks", "agenthooks-vscode.json")] = content // Target.Dir is ~/.copilot
	}
	return memFS(files), nil
}
