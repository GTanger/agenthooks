package agenthooks

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	collpb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	cpb "go.opentelemetry.io/proto/otlp/common/v1"
	lpb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

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
// the exporter's tailer does.
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

func TestExporterVerbWithoutRecorder(t *testing.T) {
	r := quietRunner()
	var out, errb strings.Builder
	code := r.Run(context.Background(), []string{"agenthooks", "exporter"}, strings.NewReader(""), &out, &errb)
	if code != 64 {
		t.Errorf("exporter verb without WithTelemetry: exit %d, want 64", code)
	}
	if !strings.Contains(errb.String(), "WithTelemetry") {
		t.Errorf("stderr must point at WithTelemetry: %q", errb.String())
	}
}

func TestExporterVerbRejectsBadFlags(t *testing.T) {
	rec, _ := newTestTelemetry(t)
	r := quietRunner(WithTelemetry(rec))
	var out, errb strings.Builder
	code := r.Run(context.Background(), []string{"agenthooks", "exporter", "--bogus"}, strings.NewReader(""), &out, &errb)
	if code != 64 {
		t.Errorf("bad exporter flag: exit %d, want 64 (stderr: %s)", code, errb.String())
	}
}

// TestExporterVerbShipsSpool is the end-to-end argv path: a hook run spools
// a record, then `agenthooks exporter` (config inherited from the recorder)
// ships it and shuts down cleanly on context cancellation.
func TestExporterVerbShipsSpool(t *testing.T) {
	requests := make(chan *collpb.ExportLogsServiceRequest, 16)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- decodeOTLPLogs(t, r)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	dir := t.TempDir()
	rec, err := telemetry.New(telemetry.Config{Endpoint: srv.URL + "/v1/logs", SpoolDir: dir})
	if err != nil {
		t.Fatalf("telemetry.New: %v", err)
	}
	r := quietRunner(WithTelemetry(rec))
	r.OnToolPre(func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
		return Deny("blocked"), nil
	})
	if _, code := runWith(t, r, claudeArgs(), fixture(t, "claude/pre_tool_use.json")); code != 0 {
		t.Fatalf("hook run exit %d", code)
	}
	if len(readSpooledTelemetry(t, dir)) != 1 {
		t.Fatalf("expected one spooled record before the exporter runs")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	exit := make(chan int, 1)
	var errb safeBuffer
	go func() {
		exit <- r.Run(ctx, []string{"agenthooks", "exporter", "--interval=20ms"},
			strings.NewReader(""), io.Discard, &errb)
	}()
	select {
	case req := <-requests:
		assertShippedPreToolUse(t, req)
	case <-time.After(15 * time.Second):
		t.Fatalf("exporter never delivered; stderr: %s", errb.String())
	}
	cancel()
	select {
	case code := <-exit:
		if code != 0 {
			t.Errorf("graceful exporter shutdown: exit %d, want 0; stderr: %s", code, errb.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("exporter did not shut down on cancellation")
	}
}

// decodeOTLPLogs decodes the OTLP/HTTP protobuf body of an export request
// (gzip per the exporter's compression setting).
func decodeOTLPLogs(t *testing.T, r *http.Request) *collpb.ExportLogsServiceRequest {
	t.Helper()
	var body io.Reader = r.Body
	if r.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Errorf("gzip reader: %v", err)
			return &collpb.ExportLogsServiceRequest{}
		}
		defer func() { _ = gz.Close() }()
		body = gz
	}
	raw, err := io.ReadAll(body)
	if err != nil {
		t.Errorf("reading export body: %v", err)
		return &collpb.ExportLogsServiceRequest{}
	}
	var req collpb.ExportLogsServiceRequest
	if err := proto.Unmarshal(raw, &req); err != nil {
		t.Errorf("decoding export body: %v", err)
	}
	return &req
}

// assertShippedPreToolUse checks the exported payload is the spooled hook
// record — the right event, not just any bytes on the endpoint.
func assertShippedPreToolUse(t *testing.T, req *collpb.ExportLogsServiceRequest) {
	t.Helper()
	var events []string
	for _, rl := range req.GetResourceLogs() {
		for _, sl := range rl.GetScopeLogs() {
			for _, lr := range sl.GetLogRecords() {
				for _, kv := range lr.GetAttributes() {
					if kv.GetKey() == "gram.hook.event" {
						events = append(events, kv.GetValue().GetStringValue())
					}
				}
			}
		}
	}
	if len(events) != 1 || events[0] != "PreToolUse" {
		t.Errorf("shipped gram.hook.event values = %v, want [PreToolUse]", events)
	}
}

// safeBuffer is a mutex-guarded strings.Builder for cross-goroutine writes.
type safeBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
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
