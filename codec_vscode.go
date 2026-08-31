package agenthooks

import (
	"encoding/json"
	"time"
)

// VS Code Copilot Chat dialect: snake_case JSON on stdin, PascalCase event
// names, and per-event response placement on stdout. Keep this codec separate
// from other providers: matching wire fields today do not imply shared future
// behavior.

var vscodeKinds = map[string]EventKind{
	"SessionStart":       KindSessionStart,
	"SessionEnd":         KindSessionEnd,
	"UserPromptSubmit":   KindPromptSubmitted,
	"PreToolUse":         KindToolPre,
	"PostToolUse":        KindToolPost,
	"PostToolUseFailure": KindToolError,
	"PermissionRequest":  KindPermission,
	"Stop":               KindStop,
	"SubagentStart":      KindSubagentStart,
	"SubagentStop":       KindSubagentStop,
	"PreCompact":         KindCompactPre,
}

type vscodeIn struct {
	SessionID            string          `json:"session_id"`
	TranscriptPath       string          `json:"transcript_path"`
	CWD                  string          `json:"cwd"`
	HookEventName        string          `json:"hook_event_name"`
	PermissionMode       string          `json:"permission_mode"`
	Model                string          `json:"model"`
	PromptID             string          `json:"prompt_id"`
	ToolName             string          `json:"tool_name"`
	ToolInput            json.RawMessage `json:"tool_input"`
	ToolUseID            string          `json:"tool_use_id"`
	ToolResponse         json.RawMessage `json:"tool_response"`
	ToolError            string          `json:"tool_error"`
	Prompt               string          `json:"prompt"`
	LastAssistantMessage string          `json:"last_assistant_message"`
	DurationMS           *float64        `json:"duration_ms"`
	Source               string          `json:"source"`
	Reason               string          `json:"reason"`
	StopHookActive       bool            `json:"stop_hook_active"`
	Trigger              string          `json:"trigger"`
	CustomInstructions   string          `json:"custom_instructions"`
	AgentID              string          `json:"agent_id"`
	AgentType            string          `json:"agent_type"`
}

func decodeVSCode(v Variant, conf DetectionConfidence, now time.Time, payload []byte) (any, error) {
	var in vscodeIn
	if err := json.Unmarshal(payload, &in); err != nil {
		return nil, err
	}
	kind, ok := vscodeKinds[in.HookEventName]
	if !ok {
		kind = KindOther
	}
	base := Event{
		Provider:            ProviderVSCodeCopilot,
		Variant:             v,
		NativeName:          in.HookEventName,
		Kind:                kind,
		Time:                now,
		DetectionConfidence: conf,
		Session: SessionInfo{
			ID:             in.SessionID,
			TurnID:         in.PromptID,
			CWD:            in.CWD,
			WorkspaceRoots: rootsFor(in.CWD),
			TranscriptPath: in.TranscriptPath,
			Model:          in.Model,
			PermissionMode: in.PermissionMode,
		},
		Raw: json.RawMessage(payload),
	}
	if in.AgentID != "" || in.AgentType != "" {
		base.Agent = &AgentInfo{ID: in.AgentID, Type: in.AgentType}
	}

	switch kind {
	case KindToolPre:
		return &ToolPreEvent{Event: base, Tool: makeToolCall(base.Session, in.ToolName, in.ToolUseID, in.ToolInput, in.ToolInput)}, nil
	case KindPermission:
		return &PermissionEvent{Event: base, Tool: makeToolCall(base.Session, in.ToolName, in.ToolUseID, in.ToolInput, in.ToolInput)}, nil
	case KindToolPost, KindToolError:
		return &ToolPostEvent{
			Event:      base,
			Tool:       makeToolCall(base.Session, in.ToolName, in.ToolUseID, in.ToolInput, in.ToolInput),
			Output:     in.ToolResponse,
			Failed:     kind == KindToolError,
			Error:      in.ToolError,
			DurationMS: in.DurationMS,
		}, nil
	case KindPromptSubmitted:
		return &PromptEvent{Event: base, Prompt: in.Prompt}, nil
	case KindStop, KindSubagentStop:
		loopCount := 0
		if in.StopHookActive {
			loopCount = 1
		}
		return &StopEvent{Event: base, PreviouslyContinued: in.StopHookActive, LoopCount: loopCount, FinalMessage: in.LastAssistantMessage}, nil
	case KindSubagentStart:
		return &SubagentStartEvent{Event: base}, nil
	case KindSessionStart:
		return &SessionStartEvent{Event: base, Source: in.Source}, nil
	case KindSessionEnd:
		return &SessionEndEvent{Event: base, Reason: in.Reason}, nil
	case KindCompactPre:
		return &CompactEvent{Event: base, Trigger: in.Trigger, Instructions: in.CustomInstructions}, nil
	default:
		return &base, nil
	}
}

// encodeVSCode follows VS Code's per-event field placement. Stop verdicts are
// nested; prompt and post-tool verdicts stay top-level.
func encodeVSCode(base *Event, d decisionCore) (wireResponse, error) {
	out := map[string]any{}
	hso := map[string]any{}
	ctx := joinContext(d.context)

	switch base.Kind {
	case KindToolPre, KindPermission:
		switch d.kind {
		case DecisionAllow:
			hso["permissionDecision"] = "allow"
		case DecisionDeny:
			hso["permissionDecision"] = "deny"
		case DecisionAsk:
			hso["permissionDecision"] = "ask"
		}
		if d.reason != "" && d.kind != DecisionNoDecision {
			hso["permissionDecisionReason"] = d.reason
		}
		if d.hasUpdatedInput {
			hso["updatedInput"] = d.updatedInput
		}
		if ctx != "" {
			hso["additionalContext"] = ctx
		}
	case KindPromptSubmitted:
		if d.kind == DecisionBlockPrompt {
			out["decision"] = "block"
			out["reason"] = d.reason
		}
		if ctx != "" {
			hso["additionalContext"] = ctx
		}
	case KindSessionStart, KindSubagentStart:
		if ctx != "" {
			hso["additionalContext"] = ctx
		}
	case KindStop, KindSubagentStop:
		if d.kind == DecisionContinue {
			hso["decision"] = "block"
			hso["reason"] = d.instruction
		}
	case KindToolPost, KindToolError:
		if d.kind == DecisionFlagOutput {
			out["decision"] = "block"
			out["reason"] = d.reason
		}
		if ctx != "" {
			hso["additionalContext"] = ctx
		}
	}

	if d.systemMessage != "" {
		out["systemMessage"] = d.systemMessage
	}
	if d.stopAgent {
		out["continue"] = false
		if d.stopReason != "" {
			out["stopReason"] = d.stopReason
		}
	}
	if len(hso) > 0 {
		hso["hookEventName"] = base.NativeName
		out["hookSpecificOutput"] = hso
	}
	b, err := json.Marshal(out)
	if err != nil {
		return wireResponse{}, err
	}
	return wireResponse{Stdout: b}, nil
}
