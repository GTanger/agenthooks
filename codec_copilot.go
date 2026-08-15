package agenthooks

import (
	"encoding/json"
	"time"
)

// Copilot dialect: camelCase event names, camelCase payload fields, per-event
// output schemas (permissionDecision, behavior, decision, additionalContext).
//
// Two properties drive everything in this file, both verified against Copilot
// CLI 1.0.80:
//
//  1. The payload does NOT carry its own event name on most events — only
//     permissionRequest ships `hookName` and notification ships a PascalCase
//     `hook_event_name`. The native name is therefore reconstructed from the
//     payload shape (copilotEventName); the shapes are disjoint, so the
//     reconstruction is exact for every documented event.
//  2. `preToolUse` command hooks are fail-closed on ANY non-zero exit other
//     than a timeout: exit 2, a crash, or any other code denies the tool call
//     even when stdout says allow. So this codec NEVER signals through the
//     exit code — every verdict, including a fail-closed handler failure, is
//     encoded as exit 0 plus a stdout decision body. wireResponse.ExitCode is
//     left at its zero value deliberately.
var copilotKinds = map[string]EventKind{
	"sessionStart":        KindSessionStart,
	"sessionEnd":          KindSessionEnd,
	"userPromptSubmitted": KindPromptSubmitted,
	"preToolUse":          KindToolPre,
	"postToolUse":         KindToolPost,
	"postToolUseFailure":  KindToolError,
	"permissionRequest":   KindPermission,
	"agentStop":           KindStop,
	"subagentStart":       KindSubagentStart,
	"subagentStop":        KindSubagentStop,
	"preCompact":          KindCompactPre,
	"notification":        KindNotification,
	// userPromptTransformed and errorOccurred have no unified kind; they
	// arrive as KindOther with the native name intact.
}

// copilotPascalAliases maps the Claude-compatible PascalCase names Copilot
// stamps into `hook_event_name` back onto the native camelCase vocabulary.
// PascalCase is an alias into the same pipeline, not a separate one.
var copilotPascalAliases = map[string]string{
	"SessionStart":       "sessionStart",
	"SessionEnd":         "sessionEnd",
	"UserPromptSubmit":   "userPromptSubmitted",
	"PreToolUse":         "preToolUse",
	"PostToolUse":        "postToolUse",
	"PostToolUseFailure": "postToolUseFailure",
	"PermissionRequest":  "permissionRequest",
	"Notification":       "notification",
	"PreCompact":         "preCompact",
	"Stop":               "agentStop",
	"SubagentStop":       "subagentStop",
}

type copilotToolResult struct {
	ResultType      string          `json:"resultType"`
	TextResultForLM string          `json:"textResultForLlm"`
	Result          json.RawMessage `json:"result"`
}

type copilotIn struct {
	SessionID      string `json:"sessionId"`
	CWD            string `json:"cwd"`
	TranscriptPath string `json:"transcriptPath"`
	Model          string `json:"model"`

	// HookName rides permissionRequest; HookEventName rides the PascalCase
	// compat path (and notification, which stamps it natively).
	HookName      string `json:"hookName"`
	HookEventName string `json:"hook_event_name"`

	Source            string `json:"source"`
	InitialPrompt     string `json:"initialPrompt"`
	Reason            string `json:"reason"`
	Prompt            string `json:"prompt"`
	TransformedPrompt string `json:"transformedPrompt"`

	ToolName string `json:"toolName"`
	// ToolArgs is a JSON-ENCODED STRING on pre/postToolUse; ToolInput is a
	// plain object on permissionRequest. normalizeInput un-stringifies the
	// former, so ToolCall.Input is an object either way.
	ToolArgs   json.RawMessage    `json:"toolArgs"`
	ToolInput  json.RawMessage    `json:"toolInput"`
	ToolResult *copilotToolResult `json:"toolResult"`
	Error      string             `json:"error"`

	StopReason      string `json:"stopReason"`
	StopHookActive  bool   `json:"stop_hook_active"`
	Response        string `json:"response"`
	AgentID         string `json:"agentId"`
	AgentType       string `json:"agentType"`
	AgentName       string `json:"agentName"`
	Message         string `json:"message"`
	NotificationTyp string `json:"notification_type"`
}

// copilotEventName resolves the native event name. Copilot omits it from most
// payloads, so an explicit field wins and the shape decides otherwise. Field
// order below is the discrimination order and must stay in it: sessionStart
// also carries a prompt-ish field, postToolUseFailure also carries toolName.
func copilotEventName(in *copilotIn) string {
	if in.HookName != "" {
		return in.HookName
	}
	if native, ok := copilotPascalAliases[in.HookEventName]; ok {
		return native
	}
	if in.HookEventName != "" {
		return in.HookEventName
	}
	switch {
	case in.TransformedPrompt != "":
		return "userPromptTransformed"
	case in.InitialPrompt != "" || in.Source != "":
		return "sessionStart"
	case in.Prompt != "":
		return "userPromptSubmitted"
	case in.ToolResult != nil:
		if in.ToolResult.ResultType == "error" {
			return "postToolUseFailure"
		}
		return "postToolUse"
	case in.ToolName != "" && in.Error != "":
		return "postToolUseFailure"
	case in.ToolName != "":
		return "preToolUse"
	case in.StopReason != "":
		if in.AgentID != "" || in.AgentType != "" || in.Response != "" {
			return "subagentStop"
		}
		return "agentStop"
	case in.AgentName != "":
		return "subagentStart"
	case in.Message != "" || in.NotificationTyp != "":
		return "notification"
	case in.Reason != "":
		return "sessionEnd"
	}
	return "unknown"
}

func decodeCopilot(v Variant, conf DetectionConfidence, now time.Time, payload []byte) (any, error) {
	var in copilotIn
	if err := json.Unmarshal(payload, &in); err != nil {
		return nil, err
	}
	native := copilotEventName(&in)
	kind, ok := copilotKinds[native]
	if !ok {
		kind = KindOther
	}
	base := Event{
		Provider:            ProviderCopilot,
		Variant:             v,
		NativeName:          native,
		Kind:                kind,
		Time:                now,
		DetectionConfidence: conf,
		Session: SessionInfo{
			ID:             in.SessionID,
			TurnID:         "",
			CWD:            in.CWD,
			WorkspaceRoots: rootsFor(in.CWD),
			TranscriptPath: in.TranscriptPath,
			Model:          in.Model,
			PermissionMode: "",
			UserEmail:      "",
		},
		Agent:      nil,
		Backfilled: false,
		Raw:        json.RawMessage(payload),
		Ext:        nil,
	}
	if in.AgentID != "" || in.AgentType != "" || in.AgentName != "" {
		typ := in.AgentType
		if typ == "" {
			typ = in.AgentName
		}
		base.Agent = &AgentInfo{ID: in.AgentID, Type: typ}
	}

	switch kind {
	case KindToolPre:
		return &ToolPreEvent{Event: base, Tool: copilotToolCall(base.Session, &in)}, nil
	case KindPermission:
		return &PermissionEvent{Event: base, Tool: copilotToolCall(base.Session, &in)}, nil
	case KindToolPost, KindToolError:
		var out json.RawMessage
		errText := in.Error
		if in.ToolResult != nil {
			out = in.ToolResult.Result
			if len(out) == 0 && in.ToolResult.TextResultForLM != "" {
				if b, err := json.Marshal(in.ToolResult.TextResultForLM); err == nil {
					out = b
				}
			}
			if errText == "" && in.ToolResult.ResultType == "error" {
				errText = in.ToolResult.TextResultForLM
			}
		}
		return &ToolPostEvent{
			Event:      base,
			Tool:       copilotToolCall(base.Session, &in),
			Output:     out,
			Failed:     kind == KindToolError,
			Error:      errText,
			DurationMS: nil,
		}, nil
	case KindPromptSubmitted:
		return &PromptEvent{Event: base, Prompt: in.Prompt}, nil
	case KindStop, KindSubagentStop:
		loop := 0
		if in.StopHookActive {
			loop = 1
		}
		return &StopEvent{
			Event:               base,
			PreviouslyContinued: in.StopHookActive,
			LoopCount:           loop,
			FinalMessage:        in.Response,
			Usage:               nil,
		}, nil
	case KindSubagentStart:
		return &SubagentStartEvent{Event: base}, nil
	case KindSessionStart:
		return &SessionStartEvent{Event: base, Source: in.Source}, nil
	case KindSessionEnd:
		return &SessionEndEvent{Event: base, Reason: in.Reason}, nil
	case KindCompactPre:
		return &CompactEvent{Event: base, Trigger: in.Reason, Instructions: ""}, nil
	case KindNotification:
		return &NotificationEvent{Event: base, Message: in.Message}, nil
	}
	ev := base
	return &ev, nil
}

// copilotToolCall normalizes the two argument shapes: a JSON-encoded string in
// toolArgs on pre/postToolUse, a plain object in toolInput on
// permissionRequest. Copilot ships no tool-call id, so every id is synthesized.
func copilotToolCall(s SessionInfo, in *copilotIn) ToolCall {
	raw := in.ToolArgs
	if len(raw) == 0 {
		raw = in.ToolInput
	}
	return makeToolCall(s, in.ToolName, "", raw, raw)
}

// encodeCopilot writes the per-event output schema. It always exits 0: on
// preToolUse any non-zero exit is an unconditional deny regardless of stdout,
// so signalling through the exit code would turn a broken hook (or an expired
// credential) into a total tool-call outage.
func encodeCopilot(base *Event, d decisionCore) (wireResponse, error) {
	out := map[string]any{}
	ctx := joinContext(d.context)

	switch base.Kind {
	case KindToolPre:
		switch d.kind {
		case DecisionAllow:
			out["permissionDecision"] = "allow"
		case DecisionDeny:
			out["permissionDecision"] = "deny"
		case DecisionAsk:
			out["permissionDecision"] = "ask"
		}
		if reason := joinNonEmpty(d.reason, ctx); reason != "" && d.kind != DecisionNoDecision {
			out["permissionDecisionReason"] = reason
		}
		if d.hasUpdatedInput {
			out["modifiedArgs"] = d.updatedInput
		}
	case KindPermission:
		switch d.kind {
		case DecisionAllow:
			out["behavior"] = "allow"
		case DecisionDeny:
			out["behavior"] = "deny"
		}
		if msg := joinNonEmpty(d.reason, ctx); msg != "" && d.kind != DecisionNoDecision {
			out["message"] = msg
		}
	case KindStop, KindSubagentStop:
		if d.kind == DecisionContinue {
			out["decision"] = "block"
			out["reason"] = d.instruction
		}
	case KindSessionStart, KindNotification:
		if ctx != "" {
			out["additionalContext"] = ctx
		}
	}

	b, err := json.Marshal(out)
	if err != nil {
		return wireResponse{Stdout: nil, Stderr: nil, ExitCode: 0}, err
	}
	return wireResponse{Stdout: b, Stderr: nil, ExitCode: 0}, nil
}
