package install

import (
	"errors"
	"io/fs"
	"path"

	"github.com/speakeasy-api/agenthooks"
)

// kindToCopilot expands unified kinds to Copilot hook event lists. Copilot
// fires exactly one native event per kind, so there is no double-fire to dedupe.
var kindToCopilot = map[agenthooks.EventKind][]string{
	agenthooks.KindSessionStart:    {"sessionStart"},
	agenthooks.KindSessionEnd:      {"sessionEnd"},
	agenthooks.KindPromptSubmitted: {"userPromptSubmitted"},
	agenthooks.KindToolPre:         {"preToolUse"},
	agenthooks.KindToolPost:        {"postToolUse"},
	agenthooks.KindToolError:       {"postToolUseFailure"},
	agenthooks.KindPermission:      {"permissionRequest"},
	agenthooks.KindStop:            {"agentStop"},
	agenthooks.KindSubagentStart:   {"subagentStart"},
	agenthooks.KindSubagentStop:    {"subagentStop"},
	agenthooks.KindCompactPre:      {"preCompact"},
	agenthooks.KindNotification:    {"notification"},
}

// copilotHookEntry is one command entry. There is deliberately NO matcher
// field: an empty matcher is a validation error that discards this plugin's
// ENTIRE hook config, so the house `"matcher": ""` pattern would silently
// disable every hook. An absent matcher means match-all, which is the
// semantic the other dialects' empty matcher was reaching for.
//
// Only `command` is emitted, never `bash`/`powershell`: Copilot copies
// `command` into both when they are absent, and the rendered argv (an absolute
// binary path plus flags) is valid in either shell.
type copilotHookEntry struct {
	Type       string `json:"type"`
	Command    string `json:"command"`
	TimeoutSec int    `json:"timeoutSec,omitempty"`
}

func renderCopilot(m Manifest, t Target) (fs.FS, error) {
	hooks := map[string][]copilotHookEntry{}
	for _, spec := range m.Hooks {
		events, ok := kindToCopilot[spec.Kind]
		if !ok {
			continue
		}
		cmd := hookCommand(m, agenthooks.ProviderCopilot, spec)
		for _, event := range events {
			hooks[event] = append(hooks[event], copilotHookEntry{
				Type:       "command",
				Command:    cmd,
				TimeoutSec: timeoutSeconds(spec),
			})
		}
	}
	// No failClosed flag: Copilot fixes the posture per event (preToolUse is
	// fail-closed on any non-timeout error, everything else fails open).
	content, err := jsonFile(map[string]any{
		"version": 1,
		"hooks":   hooks,
	})
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{}
	switch t.Scope {
	case ScopePlugin:
		if m.Identity.Name == "" {
			return nil, errors.New("install: plugin scope requires Identity.Name")
		}
		pluginJSON, err := jsonFile(map[string]string{
			"name":        m.Identity.Name,
			"version":     m.Identity.Version,
			"description": m.Identity.Description,
		})
		if err != nil {
			return nil, err
		}
		// plugin.json sits at the package root, and the hook config ships at
		// hooks/hooks.json ONLY — Copilot parses both <root>/hooks.json and
		// <root>/hooks/hooks.json, so emitting both double-registers.
		files["plugin.json"] = pluginJSON
		files[path.Join("hooks", "hooks.json")] = content
	case ScopeProject:
		files[path.Join(".github", "hooks", "agenthooks.json")] = content
	default:
		files[path.Join("hooks", "hooks.json")] = content // Target.Dir is ~/.copilot
	}
	return memFS(files), nil
}
