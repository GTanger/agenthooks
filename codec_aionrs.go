package agenthooks

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

// aionrs runs one shell command per lifecycle event and exposes event data as
// environment variables. Consumers use a tiny launcher to project those
// variables into this JSON envelope before entering the normal agenthooks
// pipeline. Prompt hook stdout is appended to the model-visible user turn;
// pre-tool non-zero exit blocks; later events are observation-only.

var aionrsKinds = map[string]EventKind{
	"PromptSubmitted": KindPromptSubmitted,
	"PreToolUse":      KindToolPre,
	"PostToolUse":     KindToolPost,
	"PreCompact":      KindCompactPre,
	"Stop":            KindStop,
}

type aionrsIn struct {
	Event        string          `json:"event"`
	SessionID    string          `json:"session_id"`
	TurnID       string          `json:"turn_id"`
	MsgID        string          `json:"msg_id"`
	CWD          string          `json:"cwd"`
	Prompt       string          `json:"prompt"`
	ToolUseID    string          `json:"tool_use_id"`
	ToolName     string          `json:"tool_name"`
	ToolInput    json.RawMessage `json:"tool_input"`
	ToolOutput   json.RawMessage `json:"tool_output"`
	Failed       bool            `json:"failed"`
	FinalMessage string          `json:"final_message"`
}

func aionrsPayloadFromEnv(event string) ([]byte, error) {
	if _, ok := aionrsKinds[event]; !ok {
		return nil, fmt.Errorf("agenthooks: unsupported aionrs event %q", event)
	}
	cwd, _ := os.Getwd()
	sessionID := os.Getenv("AIONRS_SESSION_ID")
	if sessionID == "" {
		sessionID = os.Getenv("AIONUI_CONVERSATION_ID")
	}
	failed, _ := strconv.ParseBool(os.Getenv("AIONRS_TOOL_ERROR"))
	in := aionrsIn{
		Event:        event,
		SessionID:    sessionID,
		TurnID:       os.Getenv("AIONRS_TURN_ID"),
		MsgID:        os.Getenv("AIONRS_MSG_ID"),
		CWD:          cwd,
		Prompt:       os.Getenv("AIONRS_PROMPT"),
		ToolUseID:    os.Getenv("AIONRS_TOOL_USE_ID"),
		ToolName:     os.Getenv("TOOL_NAME"),
		ToolInput:    environmentJSON("TOOL_INPUT", []byte("{}")),
		ToolOutput:   environmentJSON("TOOL_OUTPUT", []byte("null")),
		Failed:       failed,
		FinalMessage: os.Getenv("AIONRS_FINAL_MESSAGE"),
	}
	return json.Marshal(in)
}

func environmentJSON(name string, fallback []byte) json.RawMessage {
	value := []byte(os.Getenv(name))
	if len(bytes.TrimSpace(value)) == 0 {
		return json.RawMessage(fallback)
	}
	if json.Valid(value) {
		return json.RawMessage(value)
	}
	encoded, _ := json.Marshal(string(value))
	return encoded
}

func decodeAionrs(v Variant, conf DetectionConfidence, now time.Time, payload []byte) (any, error) {
	var in aionrsIn
	if err := json.Unmarshal(payload, &in); err != nil {
		return nil, err
	}
	if in.Event == "" {
		return nil, errors.New("agenthooks: aionrs payload has no event discriminator")
	}

	kind, ok := aionrsKinds[in.Event]
	if !ok {
		kind = KindOther
	}
	if in.Event == "PostToolUse" && in.Failed {
		kind = KindToolError
	}
	base := Event{
		Provider:            ProviderAionrs,
		Variant:             v,
		NativeName:          in.Event,
		Kind:                kind,
		Time:                now,
		DetectionConfidence: conf,
		Session: SessionInfo{
			ID:             in.SessionID,
			TurnID:         in.TurnID,
			CWD:            in.CWD,
			WorkspaceRoots: rootsFor(in.CWD),
		},
		Raw: json.RawMessage(payload),
	}

	switch in.Event {
	case "PromptSubmitted":
		return &PromptEvent{Event: base, Prompt: in.Prompt}, nil
	case "PreToolUse":
		rawInput := normalizeAionrsJSON(in.ToolInput, []byte("{}"))
		return &ToolPreEvent{
			Event: base,
			Tool:  makeToolCall(base.Session, in.ToolName, in.ToolUseID, rawInput, rawInput),
		}, nil
	case "PostToolUse":
		rawInput := normalizeAionrsJSON(in.ToolInput, []byte("{}"))
		output := normalizeAionrsJSON(in.ToolOutput, []byte("null"))
		errorMessage := ""
		if in.Failed {
			errorMessage = "aionrs tool call failed"
		}
		return &ToolPostEvent{
			Event:  base,
			Tool:   makeToolCall(base.Session, in.ToolName, in.ToolUseID, rawInput, rawInput),
			Output: output,
			Failed: in.Failed,
			Error:  errorMessage,
		}, nil
	case "PreCompact":
		return &CompactEvent{Event: base}, nil
	case "Stop":
		return &StopEvent{Event: base, FinalMessage: in.FinalMessage}, nil
	default:
		return &base, nil
	}
}

func normalizeAionrsJSON(raw json.RawMessage, fallback []byte) json.RawMessage {
	if len(bytes.TrimSpace(raw)) == 0 || !json.Valid(raw) {
		return json.RawMessage(fallback)
	}
	return raw
}

func encodeAionrs(base *Event, d decisionCore) (wireResponse, error) {
	if d.blocks() {
		reason := d.reason
		if reason == "" {
			reason = "blocked by agenthooks policy"
		}
		return wireResponse{Stderr: []byte(reason), ExitCode: 1}, nil
	}
	if base.Kind == KindPromptSubmitted && len(d.context) > 0 {
		return wireResponse{Stdout: []byte(joinContext(d.context))}, nil
	}
	return wireResponse{}, nil
}
