package agenthooks

import "encoding/json"

// VS Code Copilot Chat dialect: byte-for-byte the Claude Code wire shape on
// stdin (snake_case fields, PascalCase event names) and almost the same on
// stdout, so both edges delegate — decode via decodeClaudeAs, encode via
// encodeClaude plus the two placement fixups below.
//
// Where VS Code reads each field was verified against the extension source
// rather than the docs (chatHookService.ts, toolCallingLoop.ts,
// hookResultProcessor.ts); the reference contradicts itself on Stop nesting.

// encodeVSCode writes encodeClaude's body with the two keys VS Code reads from
// a different place than Claude Code does:
//
//   - Stop / SubagentStop: the block verdict rides decision/reason NESTED in
//     hookSpecificOutput (toolCallingLoop reads it there). PostToolUse and
//     UserPromptSubmit keep the top-level placement encodeClaude already uses.
//   - SubagentStart: additionalContext is honored, which Claude Code does not
//     do, so encodeClaude has no case for it.
//
// The nested hookEventName must match the invoking hook type: _toHookResult
// strips the whole hookSpecificOutput when it mismatches (and discards the
// entire result when a TOP-LEVEL hookEventName mismatches, which is why one is
// never emitted). base.NativeName is the VS Code name, so it is correct today.
func encodeVSCode(base *Event, d decisionCore) (wireResponse, error) {
	wire, err := encodeClaude(base, d)
	if err != nil {
		return wireResponse{}, err
	}
	switch base.Kind {
	case KindSubagentStart, KindStop, KindSubagentStop:
	default:
		return wire, nil
	}

	var out map[string]any
	if err := json.Unmarshal(wire.Stdout, &out); err != nil {
		return wireResponse{}, err
	}
	hso, _ := out["hookSpecificOutput"].(map[string]any)
	if hso == nil {
		hso = map[string]any{}
	}
	switch base.Kind {
	case KindSubagentStart:
		if ctx := joinContext(d.context); ctx != "" {
			hso["additionalContext"] = ctx
		}
	case KindStop, KindSubagentStop:
		if v, ok := out["decision"]; ok {
			hso["decision"] = v
			delete(out, "decision")
			if reason, ok := out["reason"]; ok {
				hso["reason"] = reason
				delete(out, "reason")
			}
		}
	}
	if len(hso) == 0 {
		return wire, nil
	}
	hso["hookEventName"] = base.NativeName
	out["hookSpecificOutput"] = hso

	b, err := json.Marshal(out)
	if err != nil {
		return wireResponse{}, err
	}
	return wireResponse{Stdout: b}, nil
}
