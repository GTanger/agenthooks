package telemetry

import (
	"encoding/hex"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"
)

// Fixture vectors computed by running gram's reference derivation —
// hashToolCallIDToTraceID (server/internal/hooks/impl.go),
// syntheticToolCallID (impl.go), canonicalTraceID (ingest_hooks.go) — over
// known inputs. The agent side must match byte-for-byte: the shadow-MCP
// provenance lookup joins recorded tool-call ids to telemetry rows via
// trace_id = hashToolCallIDToTraceID(recorded id), and dual-emit parity
// diffs join on (trace_id, event.name).
func TestDeriveTraceIDMatchesGramDerivation(t *testing.T) {
	tests := []struct {
		name       string
		isTool     bool
		toolCallID string
		sessionID  string
		toolName   string
		want       string
	}{
		{
			name:   "rule 1: tool event with a native per-call id",
			isTool: true, toolCallID: "toolu_01SsRreQbJuFTsZS9ZszkzNR",
			sessionID: "sess-123", toolName: "Bash",
			want: "cec2e4457e6d548f3c3d4cbc02b8f15e",
		},
		{
			name:   "rule 1: synthesized hook_synth id hashes like any other id",
			isTool: true, toolCallID: "hook_synth_0123456789abcdef",
			sessionID: "sess-123", toolName: "Bash",
			want: "bd9bd987e5c96f59bed9589e2f3fd1dc",
		},
		{
			name:   "rule 2: tool event without an id uses the length-prefixed synthetic key",
			isTool: true, toolCallID: "",
			sessionID: "sess-123", toolName: "Bash",
			want: "c2541915e6fe97a45eac686e137028be", // sha256("8|sess-123|Bash")[:16]
		},
		{
			name:   "rule 2: injective encoding for session ids containing pipes",
			isTool: true, toolCallID: "",
			sessionID: "s|p", toolName: "mcp__srv__t",
			want: "32bd97bcc6bc60e69915e141833a3105", // sha256("3|s|p|mcp__srv__t")[:16]
		},
		{
			name:   "rule 3: non-tool events trace per session",
			isTool: false, sessionID: "sess-123",
			want: "c8d9cf2851b3e2ac6f87788b7745331a",
		},
		{
			name:   "rule 3: uuid session",
			isTool: false, sessionID: "049b8ff5-a44e-4e0c-8b9e-a9ecd221ac4a",
			want: "93ef04f4a09db03df4c60d025c294339",
		},
		{
			name:   "rule 3: tool event with neither id nor tool name falls back to the session hash",
			isTool: true, toolCallID: "", sessionID: "sess-123", toolName: "",
			want: "c8d9cf2851b3e2ac6f87788b7745331a",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, ok := deriveTraceID(tt.isTool, tt.toolCallID, tt.sessionID, tt.toolName)
			if !ok {
				t.Fatalf("expected deterministic derivation")
			}
			if got := hex.EncodeToString(id[:]); got != tt.want {
				t.Errorf("trace id = %s, want %s (gram derivation)", got, tt.want)
			}
		})
	}
}

func TestDeriveTraceIDRandomFallback(t *testing.T) {
	a, ok := deriveTraceID(false, "", "", "")
	if ok {
		t.Fatalf("empty session must not claim a deterministic id")
	}
	if !a.IsValid() {
		t.Fatalf("random fallback produced an invalid trace id")
	}
	b, _ := deriveTraceID(false, "", "", "")
	if a == b {
		t.Errorf("random fallback repeated a trace id")
	}
}

func TestDeriveSpanIDDeterministic(t *testing.T) {
	at := time.Unix(1700000000, 123456789)
	a := deriveSpanID("sess-1", "turn-1", "PreToolUse", "toolu_1", at)
	if b := deriveSpanID("sess-1", "turn-1", "PreToolUse", "toolu_1", at); a != b {
		t.Errorf("identical inputs must collide onto one span id: %s vs %s", a, b)
	}
	variants := []struct {
		name string
		got  trace.SpanID
	}{
		{"session", deriveSpanID("sess-2", "turn-1", "PreToolUse", "toolu_1", at)},
		{"turn", deriveSpanID("sess-1", "turn-2", "PreToolUse", "toolu_1", at)},
		{"native name", deriveSpanID("sess-1", "turn-1", "PostToolUse", "toolu_1", at)},
		{"tool id", deriveSpanID("sess-1", "turn-1", "PreToolUse", "toolu_2", at)},
		{"time", deriveSpanID("sess-1", "turn-1", "PreToolUse", "toolu_1", at.Add(time.Nanosecond))},
	}
	for _, v := range variants {
		if v.got == a {
			t.Errorf("changing %s must change the span id", v.name)
		}
	}
}
