package moltis

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/speakeasy-api/agenthooks"
)

func rawEvent(t *testing.T, name, native string) *agenthooks.Event {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "agenthookstest", "fixtures", "moltis", name))
	if err != nil {
		t.Fatal(err)
	}
	return &agenthooks.Event{Provider: agenthooks.ProviderMoltis, NativeName: native, Raw: raw}
}

func TestTypedViews(t *testing.T) {
	pre, ok := BeforeToolCall(rawEvent(t, "before_tool_call.json", "BeforeToolCall"))
	if !ok || pre.ToolName != "exec" || pre.Channel == nil || pre.Channel.Surface != "web" {
		t.Fatalf("BeforeToolCall wrong: %+v ok=%v", pre, ok)
	}
	prompt, ok := MessageReceived(rawEvent(t, "message_received.json", "MessageReceived"))
	if !ok || prompt.Content == "" || prompt.ChannelBinding == nil || prompt.ChannelBinding.ChannelType != "telegram" {
		t.Fatalf("MessageReceived wrong: %+v ok=%v", prompt, ok)
	}
	if string(prompt.Extra["future_field"]) != `"preserved"` {
		t.Errorf("unknown field not captured: %+v", prompt.Extra)
	}
	post, ok := AfterToolCall(rawEvent(t, "after_tool_call_failure.json", "AfterToolCall"))
	if !ok || post.Success || len(post.Result) == 0 {
		t.Fatalf("AfterToolCall wrong: %+v ok=%v", post, ok)
	}
	if _, ok := BeforeToolCall(rawEvent(t, "after_tool_call.json", "AfterToolCall")); ok {
		t.Error("BeforeToolCall must reject another native event")
	}
}
