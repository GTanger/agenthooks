package telemetry

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	cpb "go.opentelemetry.io/proto/otlp/common/v1"
	lpb "go.opentelemetry.io/proto/otlp/logs/v1"

	"github.com/speakeasy-api/agenthooks/internal/hookrecord"
)

// testEndpoint is syntactically valid but never contacted by the recorder:
// the hook-process pipeline is spool-only.
const testEndpoint = "http://127.0.0.1:9/v1/logs"

func newTestRecorder(t *testing.T, mutate func(*Config)) *Recorder {
	t.Helper()
	cfg := Config{Endpoint: testEndpoint, SpoolDir: t.TempDir()}
	if mutate != nil {
		mutate(&cfg)
	}
	rec, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return rec
}

var testReceiveTime = time.Unix(1700000000, 123456789)

// toolPreRecord is a fully-populated tool.pre snapshot: MCP transport,
// final deny decision, timing.
func toolPreRecord() *hookrecord.Record {
	return &hookrecord.Record{
		Provider:   "claude-code",
		Variant:    "cli",
		NativeName: "PreToolUse",
		Kind:       "tool.pre",
		Time:       testReceiveTime,
		SessionID:  "sess-123",
		TurnID:     "turn-7",
		CWD:        "/work/repo",
		Model:      "claude-sonnet-4-5",
		Tool: &hookrecord.Tool{
			ID:        "toolu_01SsRreQbJuFTsZS9ZszkzNR",
			Name:      "mcp__github__create_issue",
			Canonical: "mcp",
			Input:     json.RawMessage(`{"title":"hi","token":"sk-abcdef1234567890"}`),
			MCP: &hookrecord.MCP{
				Server:  "github",
				Tool:    "create_issue",
				URL:     "https://user:hunter2@mcp.example.com/sse?api_key=abc123&x=1",
				Command: "npx mcp-github --token=ghp_1234567890abcdef",
			},
		},
		Decision:       hookrecord.Decision{Kind: "deny", Reason: "blocked by policy", Blocking: true, Source: "handler"},
		HookDurationMS: 12.5,
	}
}

// readSpool decodes every spooled record across the directory's files.
func readSpool(t *testing.T, dir string) (spoolHeader, []*lpb.LogRecord) {
	t.Helper()
	names := spoolFiles(dir)
	if len(names) == 0 {
		t.Fatalf("no spool files in %s", dir)
	}
	var header spoolHeader
	var all []*lpb.LogRecord
	for _, name := range names {
		h, records, ok := readSpoolFile(filepath.Join(dir, name))
		if !ok {
			t.Fatalf("unreadable spool file %s", name)
		}
		header = h
		all = append(all, records...)
	}
	return header, all
}

// attrMap flattens a proto record's attributes into Go values for
// assertions.
func attrMap(pr *lpb.LogRecord) map[string]any {
	out := map[string]any{}
	for _, kv := range pr.GetAttributes() {
		out[kv.GetKey()] = anyValueOf(kv.GetValue())
	}
	return out
}

func anyValueOf(v *cpb.AnyValue) any {
	switch val := v.GetValue().(type) {
	case *cpb.AnyValue_BoolValue:
		return val.BoolValue
	case *cpb.AnyValue_IntValue:
		return val.IntValue
	case *cpb.AnyValue_DoubleValue:
		return val.DoubleValue
	case *cpb.AnyValue_StringValue:
		return val.StringValue
	}
	return v.String()
}
