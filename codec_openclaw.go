package agenthooks

import (
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// OpenClaw's Gateway loads plugins in-process (api.on typed hooks); like
// OpenCode there is no spawned-process hook protocol. The generated shim
// plugin (install/render_openclaw.go) proxies hook firings to this binary
// over NDJSON stdio: request {seq, hook, event, ctx} -> response
// {seq, output?, error?}. Unlike OpenCode's mutable-output merge, the reply
// output is returned verbatim as the hook handler's return value — OpenClaw
// collects returns ({block, blockReason, requireApproval, params} on
// before_tool_call; a {outcome} gate decision on before_agent_run).
//
// Payload shapes verified against OpenClaw 2026.6.34 (see quirks #34–#37).

type openclawFrame struct {
	Seq   int64           `json:"seq"`
	Hook  string          `json:"hook"`
	Event json.RawMessage `json:"event"`
	Ctx   json.RawMessage `json:"ctx"`
}

type openclawReply struct {
	Seq    int64          `json:"seq"`
	Output map[string]any `json:"output,omitempty"`
	Error  string         `json:"error,omitempty"`
}

// openclawServeState carries the per-connection context the frames don't:
// conversation-scope hooks report workspaceDir/model, tool-scope hooks don't,
// and a deny decision must flip the corresponding after_tool_call (which
// OpenClaw still fires, carrying the block text as the result — quirk #37)
// from success to blocked.
type openclawServeState struct {
	cwdBySession   map[string]string
	modelBySession map[string]string
	blockedCalls   map[string]string // toolCallId -> block reason
}

func newOpenclawServeState() *openclawServeState {
	return &openclawServeState{
		cwdBySession:   map[string]string{},
		modelBySession: map[string]string{},
		blockedCalls:   map[string]string{},
	}
}

func openclawKind(hook string) EventKind {
	switch hook {
	case "before_tool_call":
		return KindToolPre
	case "after_tool_call":
		return KindToolPost
	case "before_agent_run":
		return KindPromptSubmitted
	case "session_start":
		return KindSessionStart
	case "session_end":
		return KindSessionEnd
	case "agent_end":
		return KindStop
	case "subagent_spawned":
		return KindSubagentStart
	case "subagent_ended":
		return KindSubagentStop
	case "llm_input":
		return KindModelRequest
	case "llm_output":
		return KindModelResponse
	case "before_compaction":
		return KindCompactPre
	case "after_compaction":
		return KindCompactPost
	}
	return KindOther
}

// openclawCtx is the shared hook-context shape. Conversation-scope hooks
// carry workspaceDir/modelId; tool-scope hooks carry toolCallId; every
// agent-scope hook carries runId/sessionKey/sessionId.
type openclawCtx struct {
	AgentID      string `json:"agentId"`
	SessionKey   string `json:"sessionKey"`
	SessionID    string `json:"sessionId"`
	RunID        string `json:"runId"`
	WorkspaceDir string `json:"workspaceDir"`
	ModelID      string `json:"modelId"`
	ToolCallID   string `json:"toolCallId"`
	ToolName     string `json:"toolName"`
}

// decodeOpenClawLine decodes one NDJSON frame without serve-loop state (the
// stateless run-mode path and shape-detection fallback).
func decodeOpenClawLine(v Variant, conf DetectionConfidence, now time.Time, line []byte) (any, error) {
	var fr openclawFrame
	if err := json.Unmarshal(line, &fr); err != nil {
		return nil, err
	}
	return decodeOpenClawFrame(v, conf, now, &fr, line, nil)
}

func decodeOpenClawFrame(v Variant, conf DetectionConfidence, now time.Time, fr *openclawFrame, raw []byte, st *openclawServeState) (any, error) {
	if fr.Hook == "" {
		return nil, errors.New("agenthooks: openclaw frame missing hook name")
	}
	var cx openclawCtx
	_ = json.Unmarshal(fr.Ctx, &cx) // ctx shape varies per hook; best-effort probe

	sessionID := cx.SessionID
	if sessionID == "" {
		sessionID = cx.SessionKey
	}
	if st != nil {
		if cx.WorkspaceDir != "" {
			st.cwdBySession[sessionID] = cx.WorkspaceDir
		}
		if cx.ModelID != "" {
			st.modelBySession[sessionID] = cx.ModelID
		}
		if cx.WorkspaceDir == "" {
			cx.WorkspaceDir = st.cwdBySession[sessionID]
		}
		if cx.ModelID == "" {
			cx.ModelID = st.modelBySession[sessionID]
		}
	}

	kind := openclawKind(fr.Hook)
	base := Event{
		Provider:            ProviderOpenClaw,
		Variant:             v,
		NativeName:          fr.Hook,
		Kind:                kind,
		Time:                now,
		DetectionConfidence: conf,
		Session: SessionInfo{
			ID:             sessionID,
			TurnID:         cx.RunID,
			CWD:            cx.WorkspaceDir,
			WorkspaceRoots: rootsFor(cx.WorkspaceDir),
			Model:          cx.ModelID,
		},
		Raw: json.RawMessage(raw),
	}

	switch kind {
	case KindToolPre:
		var in struct {
			ToolName   string          `json:"toolName"`
			Params     json.RawMessage `json:"params"`
			RunID      string          `json:"runId"`
			ToolCallID string          `json:"toolCallId"`
		}
		_ = json.Unmarshal(fr.Event, &in)
		if base.Session.TurnID == "" {
			base.Session.TurnID = in.RunID
		}
		return &ToolPreEvent{Event: base, Tool: makeToolCall(base.Session, in.ToolName, in.ToolCallID, in.Params, in.Params)}, nil
	case KindToolPost:
		var in struct {
			ToolName   string          `json:"toolName"`
			Params     json.RawMessage `json:"params"`
			RunID      string          `json:"runId"`
			ToolCallID string          `json:"toolCallId"`
			Result     json.RawMessage `json:"result"`
			Error      string          `json:"error"`
			DurationMS *float64        `json:"durationMs"`
		}
		_ = json.Unmarshal(fr.Event, &in)
		if base.Session.TurnID == "" {
			base.Session.TurnID = in.RunID
		}
		errMsg := in.Error
		if errMsg == "" && st != nil {
			if reason, ok := st.blockedCalls[in.ToolCallID]; ok {
				// OpenClaw fires after_tool_call for a call this very serve
				// session denied, with the block text as the result (quirk
				// #37); report it as a failure, not a completion.
				errMsg = "blocked: " + reason
				delete(st.blockedCalls, in.ToolCallID)
			}
		}
		if errMsg != "" {
			base.Kind = KindToolError
		}
		return &ToolPostEvent{
			Event:      base,
			Tool:       makeToolCall(base.Session, in.ToolName, in.ToolCallID, in.Params, in.Params),
			Output:     in.Result,
			Failed:     errMsg != "",
			Error:      errMsg,
			DurationMS: in.DurationMS,
		}, nil
	case KindPromptSubmitted:
		var in struct {
			Prompt string `json:"prompt"`
		}
		_ = json.Unmarshal(fr.Event, &in)
		return &PromptEvent{Event: base, Prompt: in.Prompt}, nil
	case KindStop, KindSubagentStop:
		var in struct {
			RunID      string `json:"runId"`
			Success    bool   `json:"success"`
			Error      string `json:"error"`
			DurationMS *int   `json:"durationMs"`
			// FinalMessage and Usage are spliced into the agent_end frame by
			// the shim from its cached llm_output for the same runId: agent_end
			// itself carries only messages/success/duration.
			FinalMessage string `json:"finalMessage"`
			Usage        *struct {
				Input      *int `json:"input"`
				Output     *int `json:"output"`
				CacheRead  *int `json:"cacheRead"`
				CacheWrite *int `json:"cacheWrite"`
			} `json:"usage"`
		}
		_ = json.Unmarshal(fr.Event, &in)
		if base.Session.TurnID == "" {
			base.Session.TurnID = in.RunID
		}
		var usage *Usage
		if in.Usage != nil {
			usage = &Usage{
				InputTokens:      in.Usage.Input,
				OutputTokens:     in.Usage.Output,
				CacheReadTokens:  in.Usage.CacheRead,
				CacheWriteTokens: in.Usage.CacheWrite,
			}
		}
		if usage != nil && in.Error != "" {
			usage.Status = "error"
		}
		return &StopEvent{Event: base, FinalMessage: in.FinalMessage, Usage: usage}, nil
	case KindSessionStart:
		var in struct {
			SessionID   string `json:"sessionId"`
			SessionKey  string `json:"sessionKey"`
			ResumedFrom string `json:"resumedFrom"`
		}
		_ = json.Unmarshal(fr.Event, &in)
		if base.Session.ID == "" {
			base.Session.ID = in.SessionID
		}
		source := "startup"
		if in.ResumedFrom != "" {
			source = "resume"
		}
		return &SessionStartEvent{Event: base, Source: source}, nil
	case KindSessionEnd:
		var in struct {
			SessionID string `json:"sessionId"`
			Reason    string `json:"reason"`
		}
		_ = json.Unmarshal(fr.Event, &in)
		if base.Session.ID == "" {
			base.Session.ID = in.SessionID
		}
		return &SessionEndEvent{Event: base, Reason: in.Reason}, nil
	case KindSubagentStart:
		var in struct {
			ChildSessionKey string `json:"childSessionKey"`
			AgentID         string `json:"agentId"`
		}
		_ = json.Unmarshal(fr.Event, &in)
		ev := &SubagentStartEvent{Event: base}
		ev.Agent = &AgentInfo{ID: in.ChildSessionKey, Type: in.AgentID}
		return ev, nil
	case KindCompactPre, KindCompactPost:
		return &CompactEvent{Event: base}, nil
	case KindModelRequest, KindModelResponse:
		return &ModelEvent{Event: base}, nil
	}
	ev := base
	return &ev, nil
}

// encodeOpenClawReply builds the shim response frame (seq is filled by the
// serve loop). Output is the hook handler's return value: before_tool_call
// understands {block, blockReason, requireApproval, params}; before_agent_run
// understands the gate decision {outcome, reason, message}. Every decision
// the capability matrix admits is expressible, so encoding cannot fail.
func encodeOpenClawReply(base *Event, d decisionCore, st *openclawServeState, toolCallID string) *openclawReply {
	reply := &openclawReply{}
	set := func(k string, v any) {
		if reply.Output == nil {
			reply.Output = map[string]any{}
		}
		reply.Output[k] = v
	}

	switch base.Kind {
	case KindToolPre:
		switch d.kind {
		case DecisionDeny:
			reason := d.reason
			if reason == "" {
				reason = "blocked by agenthooks handler"
			}
			set("block", true)
			set("blockReason", reason)
			if st != nil && toolCallID != "" {
				st.blockedCalls[toolCallID] = reason
			}
		case DecisionAsk:
			reason := d.reason
			if reason == "" {
				reason = "Approval required"
			}
			// Headless gateways resolve requireApproval by timing out
			// (verified: no hang); timeoutBehavior deny keeps ask-shaped
			// decisions fail-safe there.
			set("requireApproval", map[string]any{
				"title":           firstLine(reason),
				"description":     reason,
				"timeoutMs":       60_000,
				"timeoutBehavior": "deny",
			})
		}
		if d.hasUpdatedInput {
			set("params", d.updatedInput)
		}
	case KindPromptSubmitted:
		if d.kind == DecisionBlockPrompt {
			reason := d.reason
			if reason == "" {
				reason = "blocked by agenthooks handler"
			}
			set("outcome", "block")
			set("reason", reason)
			set("message", reason)
		}
	}
	return reply
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
