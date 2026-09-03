package install

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"

	"github.com/speakeasy-api/agenthooks"
)

var kindToCodex = map[agenthooks.EventKind]string{
	agenthooks.KindSessionStart:    "SessionStart",
	agenthooks.KindSessionEnd:      "SessionEnd",
	agenthooks.KindPromptSubmitted: "UserPromptSubmit",
	agenthooks.KindToolPre:         "PreToolUse",
	agenthooks.KindToolPost:        "PostToolUse",
	agenthooks.KindPermission:      "PermissionRequest",
	agenthooks.KindStop:            "Stop",
	agenthooks.KindSubagentStart:   "SubagentStart",
	agenthooks.KindSubagentStop:    "SubagentStop",
	agenthooks.KindCompactPre:      "PreCompact",
	agenthooks.KindCompactPost:     "PostCompact",
}

func renderCodex(m Manifest, t Target) (fs.FS, error) {
	hooks := map[string][]claudeMatcherEntry{} // Codex uses the Claude hooks.json shape
	type trustEntry struct {
		key  string
		hash string
	}
	var trust []trustEntry
	// Codex keys hook state by "<source>:<event_label>:<group>:<handler>"
	// where source is the absolute path of the hooks.json it discovered. Codex
	// 0.152.1 on Linux retains the lexical CODEX_HOME symlink in that key,
	// whereas other builds/platforms canonicalize it. Seed both identities when
	// they differ so an archived/symlinked CODEX_HOME stays non-interactive.
	dir := t.Dir
	if absolute, err := filepath.Abs(dir); err == nil {
		dir = absolute
	}
	sources := []string{filepath.Join(dir, "hooks.json")}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil && resolved != dir {
		sources = append(sources, filepath.Join(resolved, "hooks.json"))
	}
	for _, spec := range m.Hooks {
		event, ok := kindToCodex[spec.Kind]
		if !ok {
			continue
		}
		matcher, _ := agenthooks.CompileMatcher(agenthooks.ProviderCodex, spec.Tools)
		command := hookCommand(m, agenthooks.ProviderCodex, spec)
		// Codex parses-but-skips async:true (quirk #10): telemetry hooks get
		// --async, which makes the runner re-exec itself as a detached worker
		// and return immediately — no shell involved.
		if !spec.Blocking {
			command += " --async"
		}
		secs := timeoutSeconds(spec)
		if event == "SessionEnd" && secs > 3 {
			secs = 3
		}
		for _, source := range sources {
			trust = append(trust, trustEntry{
				key:  fmt.Sprintf("%s:%s:%d:0", source, codexEventLabel[event], len(hooks[event])),
				hash: DefinitionHash(event, matcher, command, secs),
			})
		}
		hooks[event] = append(hooks[event], claudeMatcherEntry{
			Matcher: matcher,
			Hooks: []claudeHookCmd{{
				Type:    "command",
				Command: command,
				Timeout: secs,
			}},
		})
	}
	hooksJSON, err := jsonFile(map[string]any{"hooks": hooks})
	if err != nil {
		return nil, err
	}

	// Codex hooks require user trust of the definition hash (quirk #9):
	// installs pre-seed [hooks.state] tables so generated hooks run without
	// an interactive trust prompt. Rendered into config.toml inside the
	// managed marker region (merged like Kimi's TOML configs).
	sort.Slice(trust, func(i, j int) bool { return trust[i].key < trust[j].key })
	var toml strings.Builder
	toml.WriteString(tomlBeginMarker + "\n")
	for _, e := range trust {
		fmt.Fprintf(&toml, "\n[hooks.state.%s]\n", tomlString(e.key))
		fmt.Fprintf(&toml, "trusted_hash = %s\n", tomlString(e.hash))
	}
	toml.WriteString("\n" + tomlEndMarker + "\n")

	return memFS(map[string][]byte{
		"hooks.json":  hooksJSON,
		"config.toml": []byte(toml.String()),
	}), nil
}
