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
//     reconstruction is exact for every documented event. The two events
//     that cannot be driven from a test harness — preCompact and
//     subagentStart — were read off the CLI's own bundled sources
//     (app.js, `nativeHookProcessor.event("preCompact", …)` and
//     `onSubagentStart`) rather than guessed.
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

	// Copilot carries Claude's shapes under renamed keys: project onto claudeIn
	// and reuse the shared builder. Two normalizations happen here — the two
	// argument shapes collapse to one (a JSON-encoded string in toolArgs on
	// pre/postToolUse, a plain object in toolInput on permissionRequest), and
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
	shaped := claudeIn{
		ToolName:             in.ToolName,
		ToolInput:            args,
		ToolResponse:         output,
		ToolError:            errText,
		Prompt:               in.Prompt,
		Message:              in.Message,
		LastAssistantMessage: in.Response,
		Source:               in.Source,
		Reason:               in.Reason,
		StopHookActive:       in.StopHookActive,
		Trigger:              in.Trigger,
		CustomInstructions:   in.CustomInstructions,
	}
	return buildClaudeShaped(base, &shaped), nil
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
