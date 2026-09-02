// Package copilot provides typed views over native GitHub Copilot CLI hook
// payloads. Unknown fields land in Extra.
//
// Two wire quirks the unified layer normalizes: Copilot omits the event name
// from most payloads (only permissionRequest carries hookName, only
// notification carries hook_event_name), and toolArgs has changed shape across
// CLI releases — a JSON-ENCODED STRING through 1.0.80, a plain object from
// 1.0.81 — while permissionRequest has always shipped a plain object in
// toolInput. These views hand you the wire truth; use the unified
// Event.NativeName and ToolCall for the normalized form.
package copilot

import (
	"encoding/json"

	"github.com/speakeasy-api/agenthooks"
	"github.com/speakeasy-api/agenthooks/internal/jsonx"
)

// Base carries the fields every Copilot hook payload includes.
type Base struct {
	SessionID string `json:"sessionId"`
	Timestamp int64  `json:"timestamp"`
	CWD       string `json:"cwd"`
}

// SessionStartInput is the native sessionStart payload.
type SessionStartInput struct {
	Base
	Source        string                     `json:"source"`
	InitialPrompt string                     `json:"initialPrompt"`
	Extra         map[string]json.RawMessage `json:"-"`
}

// SessionEndInput is the native sessionEnd payload.
type SessionEndInput struct {
	Base
	Reason string                     `json:"reason"`
	Extra  map[string]json.RawMessage `json:"-"`
}

// UserPromptSubmittedInput is the native userPromptSubmitted payload. Command
// hooks cannot rewrite the prompt: Copilot drops their output for this event.
type UserPromptSubmittedInput struct {
	Base
	Prompt string                     `json:"prompt"`
	Extra  map[string]json.RawMessage `json:"-"`
}

// PreToolUseInput is the native preToolUse payload. No tool-call id is
// supplied.
//
// ToolArgs is raw because the CLI changed its shape without changing its name:
// through 1.0.80 it was a JSON-encoded string, from 1.0.81 it is a plain
// object. Typing it as either one makes the view reject every tool event on
// the other release — silently, since callers key on ok. Handle both, or read
// the normalized object off the unified ToolCall.Input.
type PreToolUseInput struct {
	Base
	ToolName string                     `json:"toolName"`
	ToolArgs json.RawMessage            `json:"toolArgs"`
	Extra    map[string]json.RawMessage `json:"-"`
}

// ToolResult is the tool outcome block on postToolUse.
type ToolResult struct {
	ResultType      string `json:"resultType"`
	TextResultForLM string `json:"textResultForLlm"`
}

// PostToolUseInput is the native postToolUse payload.
type PostToolUseInput struct {
	Base
	ToolName   string                     `json:"toolName"`
	ToolArgs   json.RawMessage            `json:"toolArgs"`
	ToolResult ToolResult                 `json:"toolResult"`
	Extra      map[string]json.RawMessage `json:"-"`
}

// PostToolUseFailureInput is the native postToolUseFailure payload. The
// observed shape is a bare error string; ToolResult is here because the codec
// also treats a toolResult block with resultType "error" as this event, and
// the view has to be able to surface that text too.
type PostToolUseFailureInput struct {
	Base
	ToolName   string                     `json:"toolName"`
	ToolArgs   json.RawMessage            `json:"toolArgs"`
	Error      string                     `json:"error"`
	ToolResult ToolResult                 `json:"toolResult"`
	Extra      map[string]json.RawMessage `json:"-"`
}

// PermissionRequestInput is the native permissionRequest payload. It is the
// one event carrying its own name, and its arguments are an object.
type PermissionRequestInput struct {
	HookName string `json:"hookName"`
	Base
	ToolName              string                     `json:"toolName"`
	ToolInput             json.RawMessage            `json:"toolInput"`
	PermissionSuggestions json.RawMessage            `json:"permissionSuggestions"`
	Extra                 map[string]json.RawMessage `json:"-"`
}

// AgentStopInput is the native agentStop payload.
type AgentStopInput struct {
	Base
	TranscriptPath string                     `json:"transcriptPath"`
	StopReason     string                     `json:"stopReason"`
	StopHookActive bool                       `json:"stop_hook_active"`
	Extra          map[string]json.RawMessage `json:"-"`
}

// SubagentStartInput is the native subagentStart payload.
type SubagentStartInput struct {
	Base
	TranscriptPath   string                     `json:"transcriptPath"`
	AgentName        string                     `json:"agentName"`
	AgentDisplayName string                     `json:"agentDisplayName"`
	AgentDescription string                     `json:"agentDescription"`
	Extra            map[string]json.RawMessage `json:"-"`
}

// SubagentStopInput is the native subagentStop payload.
type SubagentStopInput struct {
	Base
	TranscriptPath   string                     `json:"transcriptPath"`
	AgentID          string                     `json:"agentId"`
	AgentType        string                     `json:"agentType"`
	AgentName        string                     `json:"agentName"`
	AgentDisplayName string                     `json:"agentDisplayName"`
	Response         string                     `json:"response"`
	StopReason       string                     `json:"stopReason"`
	Extra            map[string]json.RawMessage `json:"-"`
}

// PreCompactInput is the native preCompact payload.
type PreCompactInput struct {
	Base
	TranscriptPath     string                     `json:"transcriptPath"`
	Trigger            string                     `json:"trigger"`
	CustomInstructions string                     `json:"customInstructions"`
	Extra              map[string]json.RawMessage `json:"-"`
}

// NotificationInput is the native notification payload.
type NotificationInput struct {
	Base
	Message          string                     `json:"message"`
	Title            string                     `json:"title"`
	HookEventName    string                     `json:"hook_event_name"`
	NotificationType string                     `json:"notification_type"`
	Extra            map[string]json.RawMessage `json:"-"`
}

func view[T any](e *agenthooks.Event, native string) (*T, bool) {
	if e == nil || e.Provider != agenthooks.ProviderCopilotCLI || e.NativeName != native {
		return nil, false
	}
	var v T
	if err := jsonx.Unmarshal(e.Raw, &v); err != nil {
		return nil, false
	}
	return &v, true
}

func SessionStart(e *agenthooks.Event) (*SessionStartInput, bool) {
	return view[SessionStartInput](e, "sessionStart")
}

func SessionEnd(e *agenthooks.Event) (*SessionEndInput, bool) {
	return view[SessionEndInput](e, "sessionEnd")
}

func UserPromptSubmitted(e *agenthooks.Event) (*UserPromptSubmittedInput, bool) {
	return view[UserPromptSubmittedInput](e, "userPromptSubmitted")
}

func PreToolUse(e *agenthooks.Event) (*PreToolUseInput, bool) {
	return view[PreToolUseInput](e, "preToolUse")
}

func PostToolUse(e *agenthooks.Event) (*PostToolUseInput, bool) {
	return view[PostToolUseInput](e, "postToolUse")
}

func PostToolUseFailure(e *agenthooks.Event) (*PostToolUseFailureInput, bool) {
	return view[PostToolUseFailureInput](e, "postToolUseFailure")
}

func PermissionRequest(e *agenthooks.Event) (*PermissionRequestInput, bool) {
	return view[PermissionRequestInput](e, "permissionRequest")
}

func AgentStop(e *agenthooks.Event) (*AgentStopInput, bool) {
	return view[AgentStopInput](e, "agentStop")
}

func SubagentStart(e *agenthooks.Event) (*SubagentStartInput, bool) {
	return view[SubagentStartInput](e, "subagentStart")
}

func SubagentStop(e *agenthooks.Event) (*SubagentStopInput, bool) {
	return view[SubagentStopInput](e, "subagentStop")
}

func PreCompact(e *agenthooks.Event) (*PreCompactInput, bool) {
	return view[PreCompactInput](e, "preCompact")
}

func Notification(e *agenthooks.Event) (*NotificationInput, bool) {
	return view[NotificationInput](e, "notification")
}
