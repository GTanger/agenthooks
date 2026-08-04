package telemetry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	rpb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/encoding/protojson"
)

// The spool round-trip: record → spool file → replay must preserve
// attributes, timestamps, and the deterministic trace/span identity.
func TestSpoolRoundTrip(t *testing.T) {
	rec := newTestRecorder(t, nil)
	hr := toolPreRecord()
	if err := rec.RecordHook(hr); err != nil {
		t.Fatalf("RecordHook: %v", err)
	}

	header, records := readSpool(t, rec.spoolDir)
	if header.V != 1 || header.EndpointID != rec.endpointID {
		t.Errorf("header = %+v, want v=1 endpoint_id=%s", header, rec.endpointID)
	}
	if len(header.Resource) == 0 || len(header.Scope) == 0 {
		t.Fatalf("header must carry resource and scope: %+v", header)
	}
	var res rpb.Resource
	if err := protojson.Unmarshal(header.Resource, &res); err != nil {
		t.Fatalf("header resource is not protojson: %v", err)
	}
	resAttrs := map[string]any{}
	for _, kv := range res.GetAttributes() {
		resAttrs[kv.GetKey()] = anyValueOf(kv.GetValue())
	}
	if resAttrs["gram.event.origin"] != "agenthooks" {
		t.Errorf("resource gram.event.origin = %v, want agenthooks", resAttrs["gram.event.origin"])
	}
	if !strings.Contains(string(header.Scope), scopeName) {
		t.Errorf("header scope = %s, want %s", header.Scope, scopeName)
	}

	if len(records) != 1 {
		t.Fatalf("spooled records = %d, want 1", len(records))
	}
	pr := records[0]
	if got := pr.GetTimeUnixNano(); got != uint64(testReceiveTime.UnixNano()) {
		t.Errorf("timestamp = %d, want receive time %d", got, testReceiveTime.UnixNano())
	}
	if pr.GetObservedTimeUnixNano() == 0 {
		t.Errorf("observed timestamp (spool time) must be set")
	}
	if pr.GetEventName() != "tool.pre" {
		t.Errorf("event name = %q, want tool.pre", pr.GetEventName())
	}
	if pr.GetSeverityText() != "ERROR" {
		t.Errorf("severity for a deny = %q, want ERROR", pr.GetSeverityText())
	}
	if body := pr.GetBody().GetStringValue(); body != "Hook: PreToolUse" {
		t.Errorf("body = %q, want %q", body, "Hook: PreToolUse")
	}
	// Native trace-context fields carry gram's exact derivation.
	if got := hex.EncodeToString(pr.GetTraceId()); got != "cec2e4457e6d548f3c3d4cbc02b8f15e" {
		t.Errorf("trace id = %s, want gram's hashToolCallIDToTraceID", got)
	}
	wantSpan := deriveSpanID(hr.SessionID, hr.TurnID, hr.NativeName, hr.Tool.ID, hr.Time)
	if got := hex.EncodeToString(pr.GetSpanId()); got != hex.EncodeToString(wantSpan[:]) {
		t.Errorf("span id = %s, want deterministic %s", got, hex.EncodeToString(wantSpan[:]))
	}

	attrs := attrMap(pr)
	for key, want := range map[string]any{
		"gram.hook.event":              "PreToolUse",
		"gram.hook.source":             "claude-code",
		"event.name":                   "tool.pre",
		"gram.event.origin":            "agenthooks",
		"agenthooks.provider":          "claude-code",
		"agenthooks.variant":           "cli",
		"session.id":                   "sess-123",
		"agenthooks.turn.id":           "turn-7",
		"gen_ai.response.model":        "claude-sonnet-4-5",
		"gen_ai.tool.call.id":          "toolu_01SsRreQbJuFTsZS9ZszkzNR",
		"gram.tool.name":               "mcp__github__create_issue",
		"gen_ai.tool.name":             "mcp__github__create_issue",
		"agenthooks.tool.canonical":    "mcp",
		"gram.hook.decision":           "deny",
		"agenthooks.decision.reason":   "blocked by policy",
		"agenthooks.decision.blocking": true,
		"agenthooks.decision.source":   "handler",
		"agenthooks.hook.duration_ms":  12.5,
		"agenthooks.mcp.server":        "github",
		"agenthooks.mcp.tool":          "create_issue",
	} {
		if attrs[key] != want {
			t.Errorf("attr %s = %v, want %v", key, attrs[key], want)
		}
	}
	// Default capture: digests stand in for tool input; no content keys.
	sum := sha256.Sum256(hr.Tool.Input)
	if attrs["agenthooks.tool.input.sha256"] != hex.EncodeToString(sum[:]) {
		t.Errorf("input sha256 = %v", attrs["agenthooks.tool.input.sha256"])
	}
	if attrs["agenthooks.tool.input.length"] != int64(len(hr.Tool.Input)) {
		t.Errorf("input length = %v, want %d", attrs["agenthooks.tool.input.length"], len(hr.Tool.Input))
	}
	if _, ok := attrs["gen_ai.tool.call.arguments"]; ok {
		t.Errorf("tool arguments must not spool at the default capture level")
	}
	if _, ok := attrs["agenthooks.session.cwd"]; ok {
		t.Errorf("cwd must not spool at the default capture level")
	}
	// Transport credentials never touch disk, at any level.
	if url, _ := attrs["gram.mcp.server_url"].(string); strings.Contains(url, "hunter2") || strings.Contains(url, "abc123") {
		t.Errorf("server url not redacted: %s", url)
	}
	if cmd, _ := attrs["agenthooks.mcp.command"].(string); strings.Contains(cmd, "ghp_") {
		t.Errorf("command not redacted: %s", cmd)
	}

	// Replay: the exporter-side decode must reproduce the record.
	replayed, sc := replayRecord(pr, time.Now())
	if sc.TraceID().String() != "cec2e4457e6d548f3c3d4cbc02b8f15e" {
		t.Errorf("replayed trace id = %s", sc.TraceID())
	}
	if sc.SpanID() != wantSpan {
		t.Errorf("replayed span id = %s, want %s", sc.SpanID(), wantSpan)
	}
	if !replayed.Timestamp().Equal(testReceiveTime) {
		t.Errorf("replayed timestamp = %v, want %v", replayed.Timestamp(), testReceiveTime)
	}
	if replayed.ObservedTimestamp().IsZero() {
		t.Errorf("replayed observed timestamp must be preserved")
	}
	got := map[string]any{}
	replayed.WalkAttributes(func(kv attribute.KeyValue) bool {
		got[string(kv.Key)] = kv.Value.AsInterface()
		return true
	})
	if got["gram.hook.decision"] != "deny" || got["session.id"] != "sess-123" {
		t.Errorf("replayed attributes lost: %v", got)
	}
}

func TestSpoolAppendsAcrossEventsInOneProcess(t *testing.T) {
	rec := newTestRecorder(t, nil)
	if err := rec.RecordHook(toolPreRecord()); err != nil {
		t.Fatalf("RecordHook: %v", err)
	}
	second := toolPreRecord()
	second.Kind, second.NativeName = "tool.post", "PostToolUse"
	if err := rec.RecordHook(second); err != nil {
		t.Fatalf("RecordHook: %v", err)
	}
	if files := spoolFiles(rec.spoolDir); len(files) != 1 {
		t.Errorf("one process must append to one file, got %d", len(files))
	}
	_, records := readSpool(t, rec.spoolDir)
	if len(records) != 2 {
		t.Errorf("records = %d, want 2", len(records))
	}
}

func TestSpoolRecordCapDropsOversizedRecord(t *testing.T) {
	rec := newTestRecorder(t, func(cfg *Config) { cfg.Capture = CaptureContent })
	hr := toolPreRecord()
	// Five content attributes near the per-value cap overflow the 1 MiB
	// per-record line cap even after per-value truncation.
	hr.Tool.Input = json.RawMessage(`"` + strings.Repeat("a", maxContentBytes-2) + `"`)
	hr.Tool.Output = json.RawMessage(`"` + strings.Repeat("b", maxContentBytes-2) + `"`)
	hr.Prompt = strings.Repeat("c", maxContentBytes)
	hr.FinalMessage = strings.Repeat("d", maxContentBytes)
	hr.Notification = strings.Repeat("e", maxContentBytes)
	if err := rec.RecordHook(hr); err == nil {
		t.Fatalf("oversized record must be dropped with an error for the tap to log")
	}
	if files := spoolFiles(rec.spoolDir); len(files) != 0 {
		t.Errorf("dropped record must not leave spool files, got %v", files)
	}
}

func TestSpoolSweepRemovesExpiredFiles(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "1000000000000000000-1"+spoolFileSuffix)
	if err := os.WriteFile(old, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-maxSpoolAge - time.Hour)
	if err := os.Chtimes(old, stale, stale); err != nil {
		t.Fatal(err)
	}
	fresh := filepath.Join(dir, "2000000000000000000-2"+spoolFileSuffix)
	if err := os.WriteFile(fresh, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sweepSpool(dir, time.Now(), "")
	files := spoolFiles(dir)
	if len(files) != 1 || files[0] != filepath.Base(fresh) {
		t.Errorf("sweep kept %v, want only the fresh file", files)
	}
}
