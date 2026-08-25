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

// BeforeAgentRunEvent is the event half of before_agent_run (the blockable
// prompt gate). Requires allowConversationAccess (quirk #35).
type BeforeAgentRunEvent struct {
	Prompt        string                     `json:"prompt"`
	Messages      json.RawMessage            `json:"messages"`
	SystemPrompt  string                     `json:"systemPrompt"`
	AccountID     string                     `json:"accountId"`
	ChannelID     string                     `json:"channelId"`
	SenderID      string                     `json:"senderId"`
	SenderIsOwner *bool                      `json:"senderIsOwner"`
	Extra         map[string]json.RawMessage `json:"-"`
}

// AgentEndUsage is the token/cost block the shim splices into agent_end from
// its cached llm_output (agent_end itself carries no usage natively).
type AgentEndUsage struct {
	Input      *int                       `json:"input"`
	Output     *int                       `json:"output"`
	CacheRead  *int                       `json:"cacheRead"`
	CacheWrite *int                       `json:"cacheWrite"`
	Total      *int                       `json:"total"`
	Extra      map[string]json.RawMessage `json:"-"`
}

// AgentEndEvent is the event half of agent_end. FinalMessage and Usage are
// shim-spliced from the turn's llm_output.
type AgentEndEvent struct {
	RunID        string                     `json:"runId"`
	Messages     json.RawMessage            `json:"messages"`
	Success      bool                       `json:"success"`
	Error        string                     `json:"error"`
	DurationMS   *int                       `json:"durationMs"`
	FinalMessage string                     `json:"finalMessage"`
	Usage        *AgentEndUsage             `json:"usage"`
	Extra        map[string]json.RawMessage `json:"-"`
}

// SessionStartEvent is the event half of session_start.
type SessionStartEvent struct {
	SessionID   string                     `json:"sessionId"`
	SessionKey  string                     `json:"sessionKey"`
	ResumedFrom string                     `json:"resumedFrom"`
	Extra       map[string]json.RawMessage `json:"-"`
}

// SessionEndEvent is the event half of session_end.
type SessionEndEvent struct {
	SessionID          string                     `json:"sessionId"`
	SessionKey         string                     `json:"sessionKey"`
	MessageCount       int                        `json:"messageCount"`
	DurationMS         *int                       `json:"durationMs"`
	Reason             string                     `json:"reason"`
	SessionFile        string                     `json:"sessionFile"`
	TranscriptArchived *bool                      `json:"transcriptArchived"`
	NextSessionID      string                     `json:"nextSessionId"`
	NextSessionKey     string                     `json:"nextSessionKey"`
	Extra              map[string]json.RawMessage `json:"-"`
}

// LlmInputEvent is the event half of llm_input.
type LlmInputEvent struct {
	RunID           string                     `json:"runId"`
	SessionID       string                     `json:"sessionId"`
	Provider        string                     `json:"provider"`
	Model           string                     `json:"model"`
	SystemPrompt    string                     `json:"systemPrompt"`
	Prompt          string                     `json:"prompt"`
	HistoryMessages json.RawMessage            `json:"historyMessages"`
	ImagesCount     int                        `json:"imagesCount"`
	Tools           json.RawMessage            `json:"tools"`
	Extra           map[string]json.RawMessage `json:"-"`
}

// LlmOutputEvent is the event half of llm_output. HarnessID distinguishes the
// embedded runtime from CLI-delegated harnesses (quirk #34).
type LlmOutputEvent struct {
	RunID          string                     `json:"runId"`
	SessionID      string                     `json:"sessionId"`
	Provider       string                     `json:"provider"`
	Model          string                     `json:"model"`
	ResolvedRef    string                     `json:"resolvedRef"`
	HarnessID      string                     `json:"harnessId"`
	Prompt         string                     `json:"prompt"`
	AssistantTexts []string                   `json:"assistantTexts"`
	LastAssistant  json.RawMessage            `json:"lastAssistant"`
	Usage          *AgentEndUsage             `json:"usage"`
	Extra          map[string]json.RawMessage `json:"-"`
}

// SubagentSpawnedEvent is the event half of subagent_spawned.
type SubagentSpawnedEvent struct {
	ChildSessionKey  string                     `json:"childSessionKey"`
	AgentID          string                     `json:"agentId"`
	Label            string                     `json:"label"`
	Mode             string                     `json:"mode"`
	RunID            string                     `json:"runId"`
	ResolvedModel    string                     `json:"resolvedModel"`
	ResolvedProvider string                     `json:"resolvedProvider"`
	Extra            map[string]json.RawMessage `json:"-"`
}

// SubagentEndedEvent is the event half of subagent_ended.
type SubagentEndedEvent struct {
	TargetSessionKey string                     `json:"targetSessionKey"`
	TargetKind       string                     `json:"targetKind"`
	Reason           string                     `json:"reason"`
	RunID            string                     `json:"runId"`
	Outcome          string                     `json:"outcome"`
	Error            string                     `json:"error"`
	Extra            map[string]json.RawMessage `json:"-"`
}

// CompactionEvent is the event half of before_compaction / after_compaction.
type CompactionEvent struct {
	MessageCount    int                        `json:"messageCount"`
	CompactingCount *int                       `json:"compactingCount"`
	CompactedCount  *int                       `json:"compactedCount"`
	TokenCount      *int                       `json:"tokenCount"`
	SessionFile     string                     `json:"sessionFile"`
	Extra           map[string]json.RawMessage `json:"-"`
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
	return decodeEvent[BeforeToolCallEvent](e)
}

// AfterToolCall decodes an after_tool_call frame's event half.
func AfterToolCall(e *agenthooks.Event) (*AfterToolCallEvent, bool) {
	if e == nil || e.NativeName != "after_tool_call" {
		return nil, false
	}
	return decodeEvent[AfterToolCallEvent](e)
}

// BeforeAgentRun decodes a before_agent_run frame's event half.
func BeforeAgentRun(e *agenthooks.Event) (*BeforeAgentRunEvent, bool) {
	if e == nil || e.NativeName != "before_agent_run" {
		return nil, false
	}
	return decodeEvent[BeforeAgentRunEvent](e)
}

// AgentEnd decodes an agent_end frame's event half.
func AgentEnd(e *agenthooks.Event) (*AgentEndEvent, bool) {
	if e == nil || e.NativeName != "agent_end" {
		return nil, false
	}
	return decodeEvent[AgentEndEvent](e)
}

// SessionStart decodes a session_start frame's event half.
func SessionStart(e *agenthooks.Event) (*SessionStartEvent, bool) {
	if e == nil || e.NativeName != "session_start" {
		return nil, false
	}
	return decodeEvent[SessionStartEvent](e)
}

// SessionEnd decodes a session_end frame's event half.
func SessionEnd(e *agenthooks.Event) (*SessionEndEvent, bool) {
	if e == nil || e.NativeName != "session_end" {
		return nil, false
	}
	return decodeEvent[SessionEndEvent](e)
}

// LlmInput decodes an llm_input frame's event half.
func LlmInput(e *agenthooks.Event) (*LlmInputEvent, bool) {
	if e == nil || e.NativeName != "llm_input" {
		return nil, false
	}
	return decodeEvent[LlmInputEvent](e)
}

// LlmOutput decodes an llm_output frame's event half.
func LlmOutput(e *agenthooks.Event) (*LlmOutputEvent, bool) {
	if e == nil || e.NativeName != "llm_output" {
		return nil, false
	}
	return decodeEvent[LlmOutputEvent](e)
}

// SubagentSpawned decodes a subagent_spawned frame's event half.
func SubagentSpawned(e *agenthooks.Event) (*SubagentSpawnedEvent, bool) {
	if e == nil || e.NativeName != "subagent_spawned" {
		return nil, false
	}
	return decodeEvent[SubagentSpawnedEvent](e)
}

// SubagentEnded decodes a subagent_ended frame's event half.
func SubagentEnded(e *agenthooks.Event) (*SubagentEndedEvent, bool) {
	if e == nil || e.NativeName != "subagent_ended" {
		return nil, false
	}
	return decodeEvent[SubagentEndedEvent](e)
}

// Compaction decodes a before_compaction or after_compaction frame's event
// half.
func Compaction(e *agenthooks.Event) (*CompactionEvent, bool) {
	if e == nil || (e.NativeName != "before_compaction" && e.NativeName != "after_compaction") {
		return nil, false
	}
	return decodeEvent[CompactionEvent](e)
}

// decodeEvent decodes the frame's event half into T with unknown-field
// capture.
func decodeEvent[T any](e *agenthooks.Event) (*T, bool) {
	fr, ok := DecodeFrame(e)
	if !ok {
		return nil, false
	}
	var in T
	if err := jsonx.Unmarshal(fr.Event, &in); err != nil {
		return nil, false
	}
	return &in, true
}
