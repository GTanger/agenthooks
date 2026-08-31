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
//     reconstruction is exact for every documented event. preCompact
//     (`trigger`) and subagentStart (`agentName`) were read off the CLI's own
//     bundled sources first (app.js, `nativeHookProcessor.event("preCompact",
//     …)` and `onSubagentStart`); both are now driven live too — the `task`
//     tool fires the subagent pair and `/compact` is accepted as a headless
//     prompt (e2e TestCopilotSubagentEvents, TestCopilotPreCompact).
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
	// Events with no unified kind (userPromptTransformed, errorOccurred)
	// arrive as KindOther with the raw payload intact.
}

// copilotPascalAliases maps the Claude-compatible PascalCase names Copilot
// stamps into `hook_event_name` back onto the native camelCase vocabulary.
// PascalCase is an alias into the same pipeline, not a separate one. Only
// notification is observed shipping one (Copilot CLI 1.0.80); add entries as
// more turn up on the wire.
var copilotPascalAliases = map[string]string{
	"Notification": "notification",
}

var copilotCompatKinds = map[string]EventKind{
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
	"Notification":       KindNotification,
}

type copilotToolResult struct {
	ResultType      string `json:"resultType"`
	TextResultForLM string `json:"textResultForLlm"`
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

	Source        string `json:"source"`
	InitialPrompt string `json:"initialPrompt"`
	Reason        string `json:"reason"`
	Prompt        string `json:"prompt"`

	// Trigger and CustomInstructions ride preCompact only. Copilot spells
	// customInstructions in camelCase where Claude spells it snake_case;
	// Trigger carries Claude's own vocabulary ("auto" | "manual").
	Trigger            string `json:"trigger"`
	CustomInstructions string `json:"customInstructions"`

	ToolName string `json:"toolName"`
	// ToolArgs on pre/postToolUse is version-dependent: a JSON-ENCODED STRING
	// through CLI 1.0.80, a plain object from 1.0.81. ToolInput is a plain
	// object on permissionRequest. Raw either way, because normalizeInput
	// un-stringifies when needed and ToolCall.Input is an object in all three
	// cases.
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

type copilotCompatIn struct {
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
	Message              string          `json:"message"`
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
	// preCompact is the only event carrying `trigger`, and it carries no
	// `reason`. This case must still precede sessionEnd: `reason` is the
	// weakest discriminator in the switch, so anything reaching it that is
	// not really a session end gets silently mislabelled KindSessionEnd.
	case in.Trigger != "":
		return "preCompact"
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
	// PascalCase compatibility fallthrough. A --provider=copilot registration
	// can receive this snake_case wire shape from two directions: the CLI running the
	// PascalCase compat file this library installs for VS Code, and VS Code
	// discovering a camelCase CLI file (both runtimes glob both hook
	// directories). copilotEventName has no camelCase shape to reconstruct from
	// there, so without this the event lands on KindOther with the tool fields
	// lost. The discriminator is an explicit event name with no camelCase
	// sessionId — every genuine Copilot payload keys the session on sessionId,
	// including the one native event (notification) that also ships
	// hook_event_name. The label stays ProviderCopilot so encodeCopilot still
	// answers in the CLI's flat schema.
	if in.HookEventName != "" && in.SessionID == "" {
		return decodeCopilotCompat(v, conf, now, payload)
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
			CWD:            in.CWD,
			WorkspaceRoots: rootsFor(in.CWD),
			TranscriptPath: in.TranscriptPath,
			Model:          in.Model,
		},
		Raw: json.RawMessage(payload),
	}
	if in.AgentID != "" || in.AgentType != "" || in.AgentName != "" {
		typ := in.AgentType
		if typ == "" {
			typ = in.AgentName
		}
		base.Agent = &AgentInfo{ID: in.AgentID, Type: typ}
	}

	// Two normalizations happen here: the argument shapes collapse to one
	// (toolArgs on pre/postToolUse, either a
	// JSON-encoded string or a plain object depending on the CLI release, and
	// a plain object in toolInput on permissionRequest), and
	// the toolResult block flattens to output + error text. Copilot ships no
	// tool-call id (so every id is synthesized) and no duration.
	args := in.ToolArgs
	if len(args) == 0 {
		args = in.ToolInput
	}
	var output json.RawMessage
	errText := in.Error
	if in.ToolResult != nil {
		if in.ToolResult.TextResultForLM != "" {
			if b, err := json.Marshal(in.ToolResult.TextResultForLM); err == nil {
				output = b
			}
		}
		if errText == "" && in.ToolResult.ResultType == "error" {
			errText = in.ToolResult.TextResultForLM
		}
	}
	switch kind {
	case KindToolPre:
		return &ToolPreEvent{Event: base, Tool: makeToolCall(base.Session, in.ToolName, "", args, args)}, nil
	case KindPermission:
		return &PermissionEvent{Event: base, Tool: makeToolCall(base.Session, in.ToolName, "", args, args)}, nil
	case KindToolPost, KindToolError:
		return &ToolPostEvent{
			Event:  base,
			Tool:   makeToolCall(base.Session, in.ToolName, "", args, args),
			Output: output,
			Failed: kind == KindToolError,
			Error:  errText,
		}, nil
	case KindPromptSubmitted:
		return &PromptEvent{Event: base, Prompt: in.Prompt}, nil
	case KindStop, KindSubagentStop:
		loopCount := 0
		if in.StopHookActive {
			loopCount = 1
		}
		return &StopEvent{Event: base, PreviouslyContinued: in.StopHookActive, LoopCount: loopCount, FinalMessage: in.Response}, nil
	case KindSubagentStart:
		return &SubagentStartEvent{Event: base}, nil
	case KindSessionStart:
		return &SessionStartEvent{Event: base, Source: in.Source}, nil
	case KindSessionEnd:
		return &SessionEndEvent{Event: base, Reason: in.Reason}, nil
	case KindNotification:
		return &NotificationEvent{Event: base, Message: in.Message}, nil
	case KindCompactPre:
		return &CompactEvent{Event: base, Trigger: in.Trigger, Instructions: in.CustomInstructions}, nil
	default:
		return &base, nil
	}
}

func decodeCopilotCompat(v Variant, conf DetectionConfidence, now time.Time, payload []byte) (any, error) {
	var in copilotCompatIn
	if err := json.Unmarshal(payload, &in); err != nil {
		return nil, err
	}
	kind, ok := copilotCompatKinds[in.HookEventName]
	if !ok {
		kind = KindOther
	}
	base := Event{
		Provider: ProviderCopilot, Variant: v, NativeName: in.HookEventName,
		Kind: kind, Time: now, DetectionConfidence: conf,
		Session: SessionInfo{
			ID: in.SessionID, TurnID: in.PromptID, CWD: in.CWD,
			WorkspaceRoots: rootsFor(in.CWD), TranscriptPath: in.TranscriptPath,
			Model: in.Model, PermissionMode: in.PermissionMode,
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
		return &ToolPostEvent{Event: base, Tool: makeToolCall(base.Session, in.ToolName, in.ToolUseID, in.ToolInput, in.ToolInput), Output: in.ToolResponse, Failed: kind == KindToolError, Error: in.ToolError, DurationMS: in.DurationMS}, nil
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
	case KindNotification:
		return &NotificationEvent{Event: base, Message: in.Message}, nil
	case KindCompactPre:
		return &CompactEvent{Event: base, Trigger: in.Trigger, Instructions: in.CustomInstructions}, nil
	default:
		return &base, nil
	}
}

// encodeCopilot writes the per-event output schema. It always exits 0: on
// preToolUse any non-zero exit is an unconditional deny regardless of stdout,
// so signalling through the exit code would turn a broken hook (or an expired
// credential) into a total tool-call outage.
func encodeCopilot(base *Event, d decisionCore) (wireResponse, error) {
	out := map[string]any{}

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
		if d.reason != "" && d.kind != DecisionNoDecision {
			out["permissionDecisionReason"] = d.reason
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
		if d.reason != "" && d.kind != DecisionNoDecision {
			out["message"] = d.reason
		}
	case KindStop, KindSubagentStop:
		if d.kind == DecisionContinue {
			out["decision"] = "block"
			out["reason"] = d.instruction
		}
	case KindSessionStart:
		// sessionStart is the ONLY kind with CapAddContext for Copilot, so it
		// is the only place d.context can survive applyPolicy. The decision
		// reasons above are deliberately reason-only: folding context into
		// them would claim a capability the matrix does not grant.
		if ctx := joinContext(d.context); ctx != "" {
			out["additionalContext"] = ctx
		}
	}

	b, err := json.Marshal(out)
	if err != nil {
		return wireResponse{}, err
	}
	return wireResponse{Stdout: b}, nil
}
