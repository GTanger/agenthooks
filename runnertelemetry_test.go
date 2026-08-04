package agenthooks

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	cpb "go.opentelemetry.io/proto/otlp/common/v1"
	lpb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/speakeasy-api/agenthooks/telemetry"
)

func newTestTelemetry(t *testing.T) (*telemetry.Recorder, string) {
	t.Helper()
	dir := t.TempDir()
	rec, err := telemetry.New(telemetry.Config{
		Endpoint: "http://127.0.0.1:9/v1/logs", // never contacted: spool-only
		SpoolDir: dir,
	})
	if err != nil {
		t.Fatalf("telemetry.New: %v", err)
	}
	return rec, dir
}

// readSpooledTelemetry decodes the spool's protojson OTLP records the way
// the detached shipper does.
func readSpooledTelemetry(t *testing.T, dir string) []*lpb.LogRecord {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading spool dir: %v", err)
	}
	var records []*lpb.LogRecord
	for _, ent := range entries {
		if !strings.HasSuffix(ent.Name(), ".ndjson") {
			continue
		}
		f, err := os.Open(filepath.Join(dir, ent.Name()))
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64<<10), 2<<20)
		first := true
		for sc.Scan() {
			var line struct {
				Record json.RawMessage `json:"record"`
			}
			if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
				t.Fatalf("spool line is not JSON: %v", err)
			}
			if len(line.Record) == 0 {
				if !first {
					t.Fatalf("record-less spool line outside the header position")
				}
				first = false
				continue // header line
			}
			first = false
			var pr lpb.LogRecord
			if err := protojson.Unmarshal(line.Record, &pr); err != nil {
				t.Fatalf("spool line is not a protojson LogRecord: %v", err)
			}
			records = append(records, &pr)
		}
		if err := sc.Err(); err != nil {
			t.Fatalf("reading spool file: %v", err)
		}
		_ = f.Close()
	}
	return records
}

func telemetryAttrs(pr *lpb.LogRecord) map[string]any {
	out := map[string]any{}
	for _, kv := range pr.GetAttributes() {
		switch val := kv.GetValue().GetValue().(type) {
		case *cpb.AnyValue_BoolValue:
			out[kv.GetKey()] = val.BoolValue
		case *cpb.AnyValue_IntValue:
			out[kv.GetKey()] = val.IntValue
		case *cpb.AnyValue_DoubleValue:
			out[kv.GetKey()] = val.DoubleValue
		default:
			out[kv.GetKey()] = kv.GetValue().GetStringValue()
		}
	}
	return out
}

func TestWithTelemetryTapSeesFinalDecision(t *testing.T) {
	rec, dir := newTestTelemetry(t)
	r := quietRunner(WithTelemetry(rec))
	r.OnToolPre(func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
		return Deny("blocked"), nil
	})
	out, code := runWith(t, r, claudeArgs(), fixture(t, "claude/pre_tool_use.json"))
	want := `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"blocked"}}`
	if out != want || code != 0 {
		t.Fatalf("telemetry must not change the wire: got %q (exit %d)", out, code)
	}

	records := readSpooledTelemetry(t, dir)
	if len(records) != 1 {
		t.Fatalf("spooled records = %d, want 1", len(records))
	}
	pr := records[0]
	attrs := telemetryAttrs(pr)
	if attrs["gram.hook.decision"] != "deny" || attrs["agenthooks.decision.reason"] != "blocked" {
		t.Errorf("record must carry the final decision: %v", attrs)
	}
	if attrs["gram.hook.event"] != "PreToolUse" || attrs["event.name"] != "tool.pre" {
		t.Errorf("record identity wrong: %v", attrs)
	}
	if attrs["gen_ai.tool.call.id"] != "toolu_01ABC" || attrs["gram.tool.name"] != "Bash" {
		t.Errorf("tool identity wrong: %v", attrs)
	}
	if attrs["session.id"] != "sess-claude-1" {
		t.Errorf("session id wrong: %v", attrs)
	}
	// gram's hashToolCallIDToTraceID("toolu_01ABC").
	if got := hex.EncodeToString(pr.GetTraceId()); got != "7661011023ab0fab264a729fccde4ff1" {
		t.Errorf("trace id = %s, want gram derivation for toolu_01ABC", got)
	}
	if pr.GetSeverityText() != "ERROR" {
		t.Errorf("deny severity = %q, want ERROR", pr.GetSeverityText())
	}
}

func TestWithTelemetryRecordsPolicyDegradedDecision(t *testing.T) {
	rec, dir := newTestTelemetry(t)
	r := quietRunner(WithTelemetry(rec), WithPolicy(Policy{Unsupported: Degrade, AskFallback: FallbackDeny}))
	r.OnToolPre(func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
		return AskUser("confirm?"), nil
	})
	out, _ := runWith(t, r, []string{"agenthooks", "run", "--provider=codex"}, fixture(t, "codex/pre_tool_use.json"))
	if !strings.Contains(out, `"permissionDecision":"deny"`) {
		t.Fatalf("ask should degrade to deny on codex: %q", out)
	}

	records := readSpooledTelemetry(t, dir)
	if len(records) != 1 {
		t.Fatalf("spooled records = %d, want 1", len(records))
	}
	attrs := telemetryAttrs(records[0])
	// The record carries the verdict the provider actually got — deny after
	// capability degradation — not the handler's ask.
	if attrs["gram.hook.decision"] != "deny" {
		t.Errorf("post-degradation decision = %v, want deny", attrs["gram.hook.decision"])
	}
	// gram's hashToolCallIDToTraceID("call_9").
	if got := hex.EncodeToString(records[0].GetTraceId()); got != "5e447f59d541311dada70f8d9d26d0e3" {
		t.Errorf("trace id = %s, want gram derivation for call_9", got)
	}
}

func TestWithTelemetryFailingRecorderKeepsWire(t *testing.T) {
	parent := t.TempDir()
	spool := filepath.Join(parent, "spool")
	rec, err := telemetry.New(telemetry.Config{Endpoint: "http://127.0.0.1:9/v1/logs", SpoolDir: spool})
	if err != nil {
		t.Fatalf("telemetry.New: %v", err)
	}
	// Break the spool after construction: the dir becomes a file, so every
	// append fails at event time.
	if err := os.RemoveAll(spool); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(spool, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	r := quietRunner(WithTelemetry(rec))
	r.OnToolPre(func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
		return Deny("blocked"), nil
	})
	out, code := runWith(t, r, claudeArgs(), fixture(t, "claude/pre_tool_use.json"))
	want := `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"blocked"}}`
	if out != want || code != 0 {
		t.Errorf("failing recorder must never change the wire response: %q (exit %d)", out, code)
	}
}

func TestTelemetryTapPanicIsContained(t *testing.T) {
	r := quietRunner()
	r.OnToolPre(func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
		return Deny("blocked"), nil
	})
	r.afterDecision = func(any, *Event, decisionCore, recordTiming, error, string) {
		panic("recorder bug")
	}
	out, code := runWith(t, r, claudeArgs(), fixture(t, "claude/pre_tool_use.json"))
	if code != 0 || !strings.Contains(out, `"deny"`) {
		t.Errorf("tap panic must not leak into the wire: %q (exit %d)", out, code)
	}
}

func TestServeLoopTapsTelemetry(t *testing.T) {
	rec, dir := newTestTelemetry(t)
	r := quietRunner(WithTelemetry(rec))
	r.OnToolPre(func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
		return Deny("no bash in this session"), nil
	})
	lines := []string{
		`{"seq":1,"hook":"initialize","input":{"serverUrl":"http://127.0.0.1:1","directory":"/work","worktree":""}}`,
		strings.TrimSpace(string(fixture(t, "opencode/tool_execute_before.json"))),
	}
	var out, errb strings.Builder
	code := r.Run(context.Background(), []string{"agenthooks", "serve", "--provider=opencode"},
		strings.NewReader(strings.Join(lines, "\n")+"\n"), &out, &errb)
	if code != 0 {
		t.Fatalf("serve exit %d, stderr: %s", code, errb.String())
	}

	records := readSpooledTelemetry(t, dir)
	if len(records) != 1 {
		t.Fatalf("spooled records = %d, want 1 (initialize is not a hook event)", len(records))
	}
	attrs := telemetryAttrs(records[0])
	if attrs["gram.hook.decision"] != "deny" || attrs["gram.hook.source"] != "opencode" {
		t.Errorf("serve-loop record wrong: %v", attrs)
	}
}
