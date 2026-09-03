package install

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/speakeasy-api/agenthooks"
)

var kindToAionrs = map[agenthooks.EventKind]struct {
	table string
	event string
}{
	agenthooks.KindPromptSubmitted: {table: "prompt_submitted", event: "PromptSubmitted"},
	agenthooks.KindToolPre:         {table: "pre_tool_use", event: "PreToolUse"},
	agenthooks.KindToolPost:        {table: "post_tool_use", event: "PostToolUse"},
	agenthooks.KindToolError:       {table: "post_tool_use", event: "PostToolUse"},
	agenthooks.KindCompactPre:      {table: "compact_pre", event: "PreCompact"},
	agenthooks.KindStop:            {table: "stop", event: "Stop"},
}

var aionrsEventOrder = []string{"PromptSubmitted", "PreToolUse", "PostToolUse", "PreCompact", "Stop"}

// renderAionrs emits the native aionrs TOML hook tables. aionrs exposes hook
// payloads as environment variables rather than stdin, so the provider codec
// projects the named native event before entering the normal handler pipeline.
// Tool post and error share one native event and are deliberately coalesced.
func renderAionrs(m Manifest, t Target) (fs.FS, error) {
	if t.Scope == ScopePlugin {
		return nil, errors.New("install: aionrs has no plugin hook scope; use ScopeUser or ScopeProject")
	}

	type groupedSpec struct {
		table string
		specs []HookSpec
	}
	byEvent := map[string]groupedSpec{}
	for _, spec := range m.Hooks {
		mapping, ok := kindToAionrs[spec.Kind]
		if !ok {
			continue
		}
		group := byEvent[mapping.event]
		group.table = mapping.table
		group.specs = append(group.specs, spec)
		byEvent[mapping.event] = group
	}

	name := m.Identity.Name
	if name == "" {
		name = "agenthooks"
	}
	var body strings.Builder
	body.WriteString(tomlBeginMarker + "\n")
	for _, event := range aionrsEventOrder {
		group, ok := byEvent[event]
		if !ok || len(group.specs) == 0 {
			continue
		}
		combined := combineMoltisSpecs(group.specs)
		body.WriteString("\n[[hooks." + group.table + "]]\n")
		fmt.Fprintf(&body, "name = %s\n", tomlString(name+":"+event))
		if len(combined.Tools.Names) > 0 {
			body.WriteString("tool_match = [")
			for index, tool := range combined.Tools.Names {
				if index > 0 {
					body.WriteString(", ")
				}
				body.WriteString(tomlString(tool))
			}
			body.WriteString("]\n")
		}
		fmt.Fprintf(&body, "command = %s\n", tomlString(aionrsHookCommand(m, event, combined)))
		fmt.Fprintf(&body, "timeout_ms = %d\n", aionrsTimeoutMillis(combined))
	}
	body.WriteString("\n" + tomlEndMarker + "\n")

	path := "config.toml"
	if t.Scope == ScopeProject {
		path = ".aionrs.toml"
	}
	return memFS(map[string][]byte{path: []byte(body.String())}), nil
}

func aionrsHookCommand(m Manifest, event string, spec HookSpec) string {
	parts := make([]string, 0, len(m.Command)+6)
	for _, command := range m.Command {
		parts = append(parts, shellQuote(command))
	}
	parts = append(parts, "agenthooks", "run", "--provider="+string(agenthooks.ProviderAionrs), "--event="+event)
	if spec.Timeout > 0 {
		parts = append(parts, "--timeout="+spec.Timeout.String())
	}
	if !spec.Tools.IsEmpty() {
		parts = append(parts, "--filter="+shellQuoteBody(spec.Tools.Encode()))
	}
	return strings.Join(parts, " ")
}

func aionrsTimeoutMillis(spec HookSpec) int64 {
	if spec.Timeout <= 0 {
		return 30_000
	}
	millis := spec.Timeout.Milliseconds()
	if millis < 1 {
		return 1
	}
	return millis
}
