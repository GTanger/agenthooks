// Package openclaw provides typed views over the NDJSON frames the generated
// agenthooks OpenClaw shim plugin proxies from the Gateway's typed api.on
// plugin hooks. Event.Raw for OpenClaw events is the verbatim frame:
// {seq, hook, event, ctx}.
package openclaw

import (
	"encoding/json"

	"github.com/speakeasy-api/agenthooks"
	"github.com/speakeasy-api/agenthooks/internal/jsonx"
)

// Frame is the shim wire request. Event and Ctx mirror the two arguments
// OpenClaw hands every typed hook handler.
type Frame struct {
	Seq   int64                      `json:"seq"`
	Hook  string                     `json:"hook"`
	Event json.RawMessage            `json:"event"`
	Ctx   json.RawMessage            `json:"ctx"`
	Extra map[string]json.RawMessage `json:"-"`
}

// Reply is the shim wire response. Output is returned verbatim as the hook
// handler's return value.
type Reply struct {
	Seq    int64          `json:"seq"`
	Output map[string]any `json:"output,omitempty"`
	Error  string         `json:"error,omitempty"`
}

// HookContext is the per-hook context OpenClaw passes as the handler's second
// argument. Conversation-scope hooks carry WorkspaceDir/ModelID; tool-scope
// hooks carry ToolCallID.
type HookContext struct {
	AgentID      string                     `json:"agentId"`
	SessionKey   string                     `json:"sessionKey"`
	SessionID    string                     `json:"sessionId"`
	RunID        string                     `json:"runId"`
	WorkspaceDir string                     `json:"workspaceDir"`
	ModelID      string                     `json:"modelId"`
	ToolCallID   string                     `json:"toolCallId"`
	ToolName     string                     `json:"toolName"`
	ChannelID    string                     `json:"channelId"`
	Extra        map[string]json.RawMessage `json:"-"`
}

// BeforeToolCallEvent is the event half of before_tool_call.
type BeforeToolCallEvent struct {
	ToolName   string                     `json:"toolName"`
	Params     json.RawMessage            `json:"params"`
	RunID      string                     `json:"runId"`
	ToolCallID string                     `json:"toolCallId"`
	Extra      map[string]json.RawMessage `json:"-"`
}

// AfterToolCallEvent is the event half of after_tool_call. Result carries the
// model-visible tool output; for a call blocked by this very plugin it holds
// the block text instead (quirk #37).
type AfterToolCallEvent struct {
	ToolName   string                     `json:"toolName"`
	Params     json.RawMessage            `json:"params"`
	RunID      string                     `json:"runId"`
	ToolCallID string                     `json:"toolCallId"`
	Result     json.RawMessage            `json:"result"`
	Error      string                     `json:"error"`
	DurationMS *float64                   `json:"durationMs"`
	Extra      map[string]json.RawMessage `json:"-"`
}

// DecodeFrame returns the verbatim frame behind an OpenClaw event.
func DecodeFrame(e *agenthooks.Event) (*Frame, bool) {
	if e == nil || e.Provider != agenthooks.ProviderOpenClaw {
		return nil, false
	}
	var fr Frame
	if err := jsonx.Unmarshal(e.Raw, &fr); err != nil {
		return nil, false
	}
	return &fr, true
}

// Context decodes the hook context of any OpenClaw event.
func Context(e *agenthooks.Event) (*HookContext, bool) {
	fr, ok := DecodeFrame(e)
	if !ok {
		return nil, false
	}
	var cx HookContext
	if err := jsonx.Unmarshal(fr.Ctx, &cx); err != nil {
		return nil, false
	}
	return &cx, true
}

// BeforeToolCall decodes a before_tool_call frame's event half.
func BeforeToolCall(e *agenthooks.Event) (*BeforeToolCallEvent, bool) {
	if e == nil || e.NativeName != "before_tool_call" {
		return nil, false
	}
	fr, ok := DecodeFrame(e)
	if !ok {
		return nil, false
	}
	var in BeforeToolCallEvent
	if err := jsonx.Unmarshal(fr.Event, &in); err != nil {
		return nil, false
	}
	return &in, true
}

// AfterToolCall decodes an after_tool_call frame's event half.
func AfterToolCall(e *agenthooks.Event) (*AfterToolCallEvent, bool) {
	if e == nil || e.NativeName != "after_tool_call" {
		return nil, false
	}
	fr, ok := DecodeFrame(e)
	if !ok {
		return nil, false
	}
	var in AfterToolCallEvent
	if err := jsonx.Unmarshal(fr.Event, &in); err != nil {
		return nil, false
	}
	return &in, true
}
