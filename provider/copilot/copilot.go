// Package copilot provides typed views over native GitHub Copilot CLI hook
// payloads. Unknown fields land in Extra.
//
// Two wire quirks the unified layer normalizes: Copilot omits the event name
// from most payloads (only permissionRequest carries hookName, only
// notification carries hook_event_name), and tool arguments arrive as a
// JSON-ENCODED STRING in toolArgs on pre/postToolUse while permissionRequest
// ships a plain object in toolInput. These views hand you the wire truth; use
// the unified Event.NativeName and ToolCall for the normalized form.
package copilot

import (
	"encoding/json"

	"github.com/speakeasy-api/agenthooks"
	"github.com/speakeasy-api/agenthooks/internal/jsonx"
)

// SessionStartInput is the native sessionStart payload.
type SessionStartInput struct {
	SessionID     string                     `json:"sessionId"`
	Timestamp     int64                      `json:"timestamp"`
	CWD           string                     `json:"cwd"`
	Source        string                     `json:"source"`
	InitialPrompt string                     `json:"initialPrompt"`
	Extra         map[string]json.RawMessage `json:"-"`
}

// SessionEndInput is the native sessionEnd payload.
type SessionEndInput struct {
	SessionID string                     `json:"sessionId"`
	Timestamp int64                      `json:"timestamp"`
	CWD       string                     `json:"cwd"`
	Reason    string                     `json:"reason"`
	Extra     map[string]json.RawMessage `json:"-"`
}

// UserPromptSubmittedInput is the native userPromptSubmitted payload. Command
// hooks cannot rewrite the prompt: Copilot drops their output for this event.
type UserPromptSubmittedInput struct {
	SessionID string                     `json:"sessionId"`
	Timestamp int64                      `json:"timestamp"`
	CWD       string                     `json:"cwd"`
	Prompt    string                     `json:"prompt"`
	Extra     map[string]json.RawMessage `json:"-"`
}

// PreToolUseInput is the native preToolUse payload. ToolArgs is a JSON-encoded
// string, and no tool-call id is supplied.
type PreToolUseInput struct {
	SessionID string                     `json:"sessionId"`
	Timestamp int64                      `json:"timestamp"`
	CWD       string                     `json:"cwd"`
	ToolName  string                     `json:"toolName"`
	ToolArgs  string                     `json:"toolArgs"`
	Extra     map[string]json.RawMessage `json:"-"`
}

// ToolResult is the tool outcome block on postToolUse.
type ToolResult struct {
	ResultType      string `json:"resultType"`
	TextResultForLM string `json:"textResultForLlm"`
}

// PostToolUseInput is the native postToolUse payload.
type PostToolUseInput struct {
	SessionID  string                     `json:"sessionId"`
	Timestamp  int64                      `json:"timestamp"`
	CWD        string                     `json:"cwd"`
	ToolName   string                     `json:"toolName"`
	ToolArgs   string                     `json:"toolArgs"`
	ToolResult ToolResult                 `json:"toolResult"`
	Extra      map[string]json.RawMessage `json:"-"`
}

// PostToolUseFailureInput is the native postToolUseFailure payload: no
// toolResult block, a bare error string instead.
type PostToolUseFailureInput struct {
	SessionID string                     `json:"sessionId"`
	Timestamp int64                      `json:"timestamp"`
	CWD       string                     `json:"cwd"`
	ToolName  string                     `json:"toolName"`
	ToolArgs  string                     `json:"toolArgs"`
	Error     string                     `json:"error"`
	Extra     map[string]json.RawMessage `json:"-"`
}

// PermissionRequestInput is the native permissionRequest payload. It is the
// one event carrying its own name, and its arguments are an object.
type PermissionRequestInput struct {
	HookName              string                     `json:"hookName"`
	SessionID             string                     `json:"sessionId"`
	Timestamp             int64                      `json:"timestamp"`
	CWD                   string                     `json:"cwd"`
	ToolName              string                     `json:"toolName"`
	ToolInput             json.RawMessage            `json:"toolInput"`
	PermissionSuggestions json.RawMessage            `json:"permissionSuggestions"`
	Extra                 map[string]json.RawMessage `json:"-"`
}

// AgentStopInput is the native agentStop payload.
type AgentStopInput struct {
	SessionID      string                     `json:"sessionId"`
	Timestamp      int64                      `json:"timestamp"`
	CWD            string                     `json:"cwd"`
	TranscriptPath string                     `json:"transcriptPath"`
	StopReason     string                     `json:"stopReason"`
	StopHookActive bool                       `json:"stop_hook_active"`
	Extra          map[string]json.RawMessage `json:"-"`
}

// SubagentStopInput is the native subagentStop payload.
type SubagentStopInput struct {
	SessionID        string                     `json:"sessionId"`
	Timestamp        int64                      `json:"timestamp"`
	CWD              string                     `json:"cwd"`
	TranscriptPath   string                     `json:"transcriptPath"`
	AgentID          string                     `json:"agentId"`
	AgentType        string                     `json:"agentType"`
	AgentName        string                     `json:"agentName"`
	AgentDisplayName string                     `json:"agentDisplayName"`
	Response         string                     `json:"response"`
	StopReason       string                     `json:"stopReason"`
	Extra            map[string]json.RawMessage `json:"-"`
}

// NotificationInput is the native notification payload.
type NotificationInput struct {
	SessionID        string                     `json:"sessionId"`
	Timestamp        int64                      `json:"timestamp"`
	CWD              string                     `json:"cwd"`
	Message          string                     `json:"message"`
	Title            string                     `json:"title"`
	HookEventName    string                     `json:"hook_event_name"`
	NotificationType string                     `json:"notification_type"`
	Extra            map[string]json.RawMessage `json:"-"`
}

func view[T any](e *agenthooks.Event, native string) (*T, bool) {
	if e == nil || e.Provider != agenthooks.ProviderCopilot || e.NativeName != native {
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

func SubagentStop(e *agenthooks.Event) (*SubagentStopInput, bool) {
	return view[SubagentStopInput](e, "subagentStop")
}

func Notification(e *agenthooks.Event) (*NotificationInput, bool) {
	return view[NotificationInput](e, "notification")
}
