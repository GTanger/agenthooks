package copilot

import (
	"encoding/json"
	"testing"

	"github.com/speakeasy-api/agenthooks"
	"github.com/speakeasy-api/agenthooks/agenthookstest"
)

func event(t *testing.T, fixture, native string) *agenthooks.Event {
	t.Helper()
	return &agenthooks.Event{
		Provider:   agenthooks.ProviderCopilot,
		NativeName: native,
		Raw:        json.RawMessage(agenthookstest.Fixture(t, "copilot/"+fixture)),
	}
}

// Every view decodes by unmarshalling into its own struct, so a wrong json tag
// or a wrong Go type does not fail loudly — the view either returns zero fields
// or, on a type mismatch, ok=false, and a caller keyed on ok silently does
// nothing. Running the recorded wire payloads through each view is what catches
// that. The fixture corpus is the same one the codec is verified against, so a
// captured-payload change can't drift the two apart.
func TestViewsDecodeRecordedPayloads(t *testing.T) {
	cases := []struct {
		fixture string
		native  string
		decode  func(*agenthooks.Event) (bool, string)
	}{
		{"session_start.json", "sessionStart", func(e *agenthooks.Event) (bool, string) {
			v, ok := SessionStart(e)
			if !ok {
				return false, ""
			}
			return v.Source == "new" && v.InitialPrompt != "" && v.Timestamp > 0, "SessionStart"
		}},
		{"session_end.json", "sessionEnd", func(e *agenthooks.Event) (bool, string) {
			v, ok := SessionEnd(e)
			return ok && v.SessionID == "sess-copilot-1", "SessionEnd"
		}},
		{"user_prompt_submitted.json", "userPromptSubmitted", func(e *agenthooks.Event) (bool, string) {
			v, ok := UserPromptSubmitted(e)
			return ok && v.Prompt != "", "UserPromptSubmitted"
		}},
		{"post_tool_use.json", "postToolUse", func(e *agenthooks.Event) (bool, string) {
			v, ok := PostToolUse(e)
			return ok && v.ToolName != "" && v.ToolResult.ResultType != "", "PostToolUse"
		}},
		{"post_tool_use_failure.json", "postToolUseFailure", func(e *agenthooks.Event) (bool, string) {
			v, ok := PostToolUseFailure(e)
			return ok && v.ToolName != "" && v.Error != "", "PostToolUseFailure"
		}},
		{"agent_stop.json", "agentStop", func(e *agenthooks.Event) (bool, string) {
			v, ok := AgentStop(e)
			return ok && v.StopReason != "" && v.TranscriptPath != "", "AgentStop"
		}},
		{"subagent_start.json", "subagentStart", func(e *agenthooks.Event) (bool, string) {
			v, ok := SubagentStart(e)
			return ok && v.AgentName != "" && v.AgentDisplayName != "" && v.AgentDescription != "", "SubagentStart"
		}},
		{"subagent_stop.json", "subagentStop", func(e *agenthooks.Event) (bool, string) {
			v, ok := SubagentStop(e)
			return ok && v.AgentID != "" && v.AgentType != "" && v.Response != "", "SubagentStop"
		}},
		{"pre_compact.json", "preCompact", func(e *agenthooks.Event) (bool, string) {
			v, ok := PreCompact(e)
			return ok && v.TranscriptPath != "" && v.Trigger != "", "PreCompact"
		}},
		{"notification.json", "notification", func(e *agenthooks.Event) (bool, string) {
			v, ok := Notification(e)
			return ok && v.Message != "" && v.NotificationType != "" && v.HookEventName != "", "Notification"
		}},
	}
	for _, tc := range cases {
		got, name := tc.decode(event(t, tc.fixture, tc.native))
		if !got {
			t.Errorf("%s: view did not decode %s into populated fields", name, tc.fixture)
		}
	}
}

// The one wire quirk the views deliberately expose rather than smooth over:
// toolArgs is a JSON-encoded STRING on pre/postToolUse while permissionRequest
// ships a plain object in toolInput. Typing either one wrong makes the view
// fail to decode at all.
func TestToolArgumentShapes(t *testing.T) {
	pre, ok := PreToolUse(event(t, "pre_tool_use.json", "preToolUse"))
	if !ok {
		t.Fatal("PreToolUse did not decode; toolArgs must be a string, not an object")
	}
	var args struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(pre.ToolArgs), &args); err != nil {
		t.Fatalf("toolArgs is not a JSON-encoded string: %q", pre.ToolArgs)
	}
	if args.Command != "echo hello-from-gram" {
		t.Errorf("command = %q", args.Command)
	}

	perm, ok := PermissionRequest(event(t, "permission_request.json", "permissionRequest"))
	if !ok {
		t.Fatal("PermissionRequest did not decode; toolInput must be raw, not a string")
	}
	if err := json.Unmarshal(perm.ToolInput, &args); err != nil || args.Command != "echo hello-from-gram" {
		t.Errorf("toolInput = %s (%v)", perm.ToolInput, err)
	}
	if perm.HookName != "permissionRequest" {
		t.Errorf("permissionRequest is the one event carrying its own name; got %q", perm.HookName)
	}
}

func TestViewsRejectMismatchedEvent(t *testing.T) {
	e := event(t, "pre_tool_use.json", "preToolUse")
	if _, ok := PostToolUse(e); ok {
		t.Error("a view must not decode a different native event")
	}
	e.Provider = agenthooks.ProviderClaudeCode
	if _, ok := PreToolUse(e); ok {
		t.Error("a view must not decode another provider's event")
	}
}

// The package doc promises unknown fields land in Extra, and the `json:"-"` tag
// means stock encoding/json would never do that — jsonx.Unmarshal does, by
// diffing the raw object against the struct's known keys. A view that forgets
// the Extra field, or a key that stops being known, shows up here.
func TestPreToolUseViewWithExtraCapture(t *testing.T) {
	raw := `{"sessionId":"sess-copilot-1","timestamp":1786820437717,"cwd":"/work/repo","toolName":"bash","toolArgs":"{}","brand_new_field":"surprise"}`
	e := &agenthooks.Event{
		Provider:   agenthooks.ProviderCopilot,
		NativeName: "preToolUse",
		Raw:        json.RawMessage(raw),
	}
	v, ok := PreToolUse(e)
	if !ok {
		t.Fatal("view should decode")
	}
	if v.ToolName != "bash" || v.SessionID != "sess-copilot-1" || v.CWD != "/work/repo" {
		t.Errorf("fields wrong: %+v", v)
	}
	if string(v.Extra["brand_new_field"]) != `"surprise"` {
		t.Errorf("unknown field must land in Extra, got %v", v.Extra)
	}

	// Known keys, embedded Base included, must not be mistaken for unknown ones.
	clean, ok := PreToolUse(event(t, "pre_tool_use.json", "preToolUse"))
	if !ok {
		t.Fatal("recorded payload should decode")
	}
	if len(clean.Extra) != 0 {
		t.Errorf("recorded payload has no unknown fields, got Extra=%v", clean.Extra)
	}
}
