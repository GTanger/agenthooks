package telemetry

import (
	"compress/gzip"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	"google.golang.org/protobuf/proto"

	"github.com/speakeasy-api/agenthooks/internal/filelock"
	"github.com/speakeasy-api/agenthooks/internal/hookrecord"
)

// otlpServer is an httptest OTLP/HTTP logs endpoint that decodes the
// protobuf request bodies the SDK exporter sends.
type otlpServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests []*collogspb.ExportLogsServiceRequest
	headers  []http.Header
	// status returns the HTTP status for the n-th request (0-based).
	status func(n int) int
}

func newOTLPServer(t *testing.T, status func(n int) int) *otlpServer {
	t.Helper()
	if status == nil {
		status = func(int) int { return http.StatusOK }
	}
	srv := &otlpServer{status: status}
	srv.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := io.Reader(r.Body)
		if r.Header.Get("Content-Encoding") == "gzip" {
			gz, err := gzip.NewReader(r.Body)
			if err != nil {
				t.Errorf("bad gzip body: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			defer func() { _ = gz.Close() }()
			body = gz
		}
		raw, err := io.ReadAll(body)
		if err != nil {
			t.Errorf("reading body: %v", err)
		}
		var req collogspb.ExportLogsServiceRequest
		if err := proto.Unmarshal(raw, &req); err != nil {
			t.Errorf("request is not OTLP protobuf: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		srv.mu.Lock()
		n := len(srv.requests)
		srv.requests = append(srv.requests, &req)
		srv.headers = append(srv.headers, r.Header.Clone())
		srv.mu.Unlock()
		code := srv.status(n)
		if code == http.StatusServiceUnavailable {
			w.Header().Set("Retry-After", "1")
		}
		if code != http.StatusOK {
			w.WriteHeader(code)
			return
		}
		resp, _ := proto.Marshal(&collogspb.ExportLogsServiceResponse{})
		w.Header().Set("Content-Type", "application/x-protobuf")
		_, _ = w.Write(resp)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (s *otlpServer) requestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

func (s *otlpServer) allRecords(t *testing.T) []map[string]any {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]any
	for _, req := range s.requests {
		for _, rl := range req.GetResourceLogs() {
			for _, sl := range rl.GetScopeLogs() {
				for _, lr := range sl.GetLogRecords() {
					attrs := attrMap(lr)
					attrs["_trace_id"] = hex.EncodeToString(lr.GetTraceId())
					attrs["_span_id"] = hex.EncodeToString(lr.GetSpanId())
					attrs["_body"] = lr.GetBody().GetStringValue()
					out = append(out, attrs)
				}
			}
		}
	}
	return out
}

func (s *otlpServer) recordCount(t *testing.T) int {
	t.Helper()
	return len(s.allRecords(t))
}

// spoolTwoRecords writes a tool.pre and a tool.post record through a
// recorder configured for the given endpoint, backdating the file so the
// exporter's delete grace does not keep it around.
func spoolTwoRecords(t *testing.T, endpoint string, headers map[string]string) ExporterConfig {
	t.Helper()
	dir := t.TempDir()
	rec, err := New(Config{Endpoint: endpoint, Headers: headers, SpoolDir: dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := rec.RecordHook(toolPreRecord()); err != nil {
		t.Fatalf("RecordHook: %v", err)
	}
	post := toolPreRecord()
	post.Kind, post.NativeName = "tool.post", "PostToolUse"
	post.Decision = hookrecord.Decision{Kind: "observed", Source: "handler"}
	if err := rec.RecordHook(post); err != nil {
		t.Fatalf("RecordHook: %v", err)
	}
	backdateSpool(t, dir, 10*time.Second)
	return testExporterConfig(dir, endpoint, headers)
}

func testExporterConfig(dir, endpoint string, headers map[string]string) ExporterConfig {
	return ExporterConfig{
		SpoolDir: dir,
		Interval: 20 * time.Millisecond,
		Logger:   slog.New(slog.DiscardHandler),
		Logs:     SignalConfig{Endpoint: endpoint, Headers: headers},
	}
}

func backdateSpool(t *testing.T, dir string, by time.Duration) {
	t.Helper()
	past := time.Now().Add(-by)
	for _, name := range spoolFiles(dir) {
		if err := os.Chtimes(filepath.Join(dir, name), past, past); err != nil {
			t.Fatal(err)
		}
	}
}

// runExporterUntil starts RunExporter and cancels it once done() reports
// true (polled), then returns RunExporter's error.
func runExporterUntil(t *testing.T, cfg ExporterConfig, done func() bool) error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- RunExporter(ctx, cfg) }()
	deadline := time.Now().Add(15 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			cancel()
			<-errc
			t.Fatalf("exporter did not reach the expected state in time")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	return <-errc
}

func TestExporterShipsBacklogThenDeletesFile(t *testing.T) {
	srv := newOTLPServer(t, nil)
	headers := map[string]string{"Gram-Key": "test-key"}
	cfg := spoolTwoRecords(t, srv.URL+"/v1/logs", headers)

	err := runExporterUntil(t, cfg, func() bool {
		return srv.requestCount() >= 1 && len(spoolFiles(cfg.SpoolDir)) == 0
	})
	if err != nil {
		t.Fatalf("RunExporter: %v (graceful shutdown must return nil)", err)
	}
	srv.mu.Lock()
	if got := srv.headers[0].Get("Gram-Key"); got != "test-key" {
		t.Errorf("auth header = %q, want test-key", got)
	}
	if ct := srv.headers[0].Get("Content-Type"); ct != "application/x-protobuf" {
		t.Errorf("content type = %q, want application/x-protobuf", ct)
	}
	resAttrs := map[string]any{}
	for _, kv := range srv.requests[0].GetResourceLogs()[0].GetResource().GetAttributes() {
		resAttrs[kv.GetKey()] = anyValueOf(kv.GetValue())
	}
	srv.mu.Unlock()
	if resAttrs["gram.event.origin"] != "agenthooks" {
		t.Errorf("shipped resource lost gram.event.origin: %v", resAttrs)
	}

	records := srv.allRecords(t)
	if len(records) != 2 {
		t.Fatalf("shipped records = %d, want 2", len(records))
	}
	if records[0]["_trace_id"] != "cec2e4457e6d548f3c3d4cbc02b8f15e" {
		t.Errorf("shipped trace id = %v, want gram derivation", records[0]["_trace_id"])
	}
	if records[0]["gram.hook.decision"] != "deny" || records[1]["gram.hook.decision"] != "observed" {
		t.Errorf("shipped decisions wrong: %v / %v", records[0]["gram.hook.decision"], records[1]["gram.hook.decision"])
	}
	if records[0]["_body"] != "Hook: PreToolUse" {
		t.Errorf("shipped body = %v", records[0]["_body"])
	}
	// Fully-shipped-and-deleted files leave no checkpoint entries behind.
	cp := loadCheckpoint(filepath.Join(cfg.SpoolDir, exporterCheckpointName))
	if len(cp.files) != 0 {
		t.Errorf("checkpoint entries for deleted files must be gone: %v", cp.files)
	}
}

func TestExporterTailsGrowingActiveFile(t *testing.T) {
	srv := newOTLPServer(t, nil)
	dir := t.TempDir()
	endpoint := srv.URL + "/v1/logs"
	rec, err := New(Config{Endpoint: endpoint, SpoolDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := rec.RecordHook(toolPreRecord()); err != nil {
		t.Fatal(err)
	}

	cfg := testExporterConfig(dir, endpoint, nil)
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- RunExporter(ctx, cfg) }()

	waitFor(t, func() bool { return srv.recordCount(t) >= 1 })
	// The writer appends to the same (fresh, undeleted) file; the exporter
	// must pick the new record up from its checkpointed offset.
	second := toolPreRecord()
	second.Kind, second.NativeName = "tool.post", "PostToolUse"
	if err := rec.RecordHook(second); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return srv.recordCount(t) >= 2 })
	cancel()
	if err := <-errc; err != nil {
		t.Fatalf("RunExporter: %v", err)
	}
	records := srv.allRecords(t)
	if records[0]["gram.hook.event"] != "PreToolUse" || records[1]["gram.hook.event"] != "PostToolUse" {
		t.Errorf("tail order wrong: %v / %v", records[0]["gram.hook.event"], records[1]["gram.hook.event"])
	}
	if records[0]["gram.hook.event"] == records[1]["gram.hook.event"] {
		t.Errorf("tailing must not re-ship the checkpointed prefix")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("condition not reached in time")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestExporterResumesFromCheckpointAcrossRestarts(t *testing.T) {
	srv := newOTLPServer(t, nil)
	dir := t.TempDir()
	endpoint := srv.URL + "/v1/logs"
	rec, err := New(Config{Endpoint: endpoint, SpoolDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := rec.RecordHook(toolPreRecord()); err != nil {
		t.Fatal(err)
	}
	cfg := testExporterConfig(dir, endpoint, nil)
	if err := runExporterUntil(t, cfg, func() bool { return srv.recordCount(t) >= 1 }); err != nil {
		t.Fatalf("first run: %v", err)
	}
	shipped := srv.recordCount(t)

	// Restart with new records appended while no exporter ran: only the
	// suffix past the persisted checkpoint may ship.
	second := toolPreRecord()
	second.Kind, second.NativeName = "tool.post", "PostToolUse"
	if err := rec.RecordHook(second); err != nil {
		t.Fatal(err)
	}
	if err := runExporterUntil(t, cfg, func() bool { return srv.recordCount(t) >= shipped+1 }); err != nil {
		t.Fatalf("second run: %v", err)
	}
	// Give the restarted exporter a couple more ticks: no re-ships.
	time.Sleep(100 * time.Millisecond)
	if got := srv.recordCount(t); got != shipped+1 {
		t.Errorf("shipped records = %d, want %d (checkpoint must prevent re-shipping)", got, shipped+1)
	}
}

func TestExporterReshipsAfterCheckpointLoss(t *testing.T) {
	srv := newOTLPServer(t, nil)
	dir := t.TempDir()
	endpoint := srv.URL + "/v1/logs"
	rec, err := New(Config{Endpoint: endpoint, SpoolDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := rec.RecordHook(toolPreRecord()); err != nil {
		t.Fatal(err)
	}
	cfg := testExporterConfig(dir, endpoint, nil)
	if err := runExporterUntil(t, cfg, func() bool { return srv.recordCount(t) >= 1 }); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if len(spoolFiles(dir)) == 0 {
		t.Skip("file already deleted; nothing left to demonstrate re-ship")
	}
	// Losing the checkpoint re-ships the file: at-least-once, with the
	// duplicate carrying identical deterministic identity for dedup.
	if err := os.Remove(filepath.Join(dir, exporterCheckpointName)); err != nil {
		t.Fatal(err)
	}
	if err := runExporterUntil(t, cfg, func() bool { return srv.recordCount(t) >= 2 }); err != nil {
		t.Fatalf("second run: %v", err)
	}
	records := srv.allRecords(t)
	if records[0]["_trace_id"] != records[1]["_trace_id"] || records[0]["_span_id"] != records[1]["_span_id"] {
		t.Errorf("re-shipped duplicate must carry identical trace/span identity: %v vs %v",
			records[0], records[1])
	}
}

func TestExporterRetriesTransientFailure(t *testing.T) {
	srv := newOTLPServer(t, func(n int) int {
		if n == 0 {
			return http.StatusServiceUnavailable
		}
		return http.StatusOK
	})
	cfg := spoolTwoRecords(t, srv.URL+"/v1/logs", nil)

	// The first request 503s (recorded by the harness before failing); the
	// SDK exporter's retry delivers on the second.
	err := runExporterUntil(t, cfg, func() bool { return srv.requestCount() >= 2 })
	if err != nil {
		t.Fatalf("transient failures must not end the exporter: %v", err)
	}
}

func TestExporterExitsOnDefinitive4xx(t *testing.T) {
	srv := newOTLPServer(t, func(int) int { return http.StatusUnauthorized })
	cfg := spoolTwoRecords(t, srv.URL+"/v1/logs", nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := RunExporter(ctx, cfg)
	if err == nil {
		t.Fatalf("a definitive 4xx must exit non-nil for the supervisor")
	}
	if !isDefinitiveExportError(err) {
		t.Errorf("error must classify as definitive: %v", err)
	}
	if files := spoolFiles(cfg.SpoolDir); len(files) != 1 {
		t.Errorf("files must stay for the next (fixed) run, got %v", files)
	}
}

func TestExporterSkipsFilesFromOtherEndpointConfigs(t *testing.T) {
	srv := newOTLPServer(t, nil)
	cfg := spoolTwoRecords(t, srv.URL+"/v1/logs", map[string]string{"Gram-Key": "original"})

	other := cfg
	other.Logs.Headers = map[string]string{"Gram-Key": "different"}
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	if err := RunExporter(ctx, other); err != nil {
		t.Fatalf("RunExporter: %v", err)
	}
	if got := srv.requestCount(); got != 0 {
		t.Errorf("records spooled under another endpoint config must not ship, got %d requests", got)
	}
	if files := spoolFiles(cfg.SpoolDir); len(files) != 1 {
		t.Errorf("foreign-config file must remain, got %v", files)
	}
}

func TestExporterSkipsTornTailThenReapsDeadFile(t *testing.T) {
	srv := newOTLPServer(t, nil)
	cfg := spoolTwoRecords(t, srv.URL+"/v1/logs", nil)

	// Simulate a crash mid-append: a torn, newline-less JSON fragment.
	name := spoolFiles(cfg.SpoolDir)[0]
	f, err := os.OpenFile(filepath.Join(cfg.SpoolDir, name), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"v":1,"endpoint_id":"x","record":{"timeUn`); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	backdateSpool(t, cfg.SpoolDir, 10*time.Second)

	if err := runExporterUntil(t, cfg, func() bool { return srv.recordCount(t) >= 2 }); err != nil {
		t.Fatalf("RunExporter: %v", err)
	}
	if got := srv.recordCount(t); got != 2 {
		t.Errorf("shipped records = %d, want 2 intact records (torn tail never ships)", got)
	}
	if files := spoolFiles(cfg.SpoolDir); len(files) != 1 {
		t.Errorf("file with a recent torn tail must survive (the append may complete), got %v", files)
	}

	// Once the fragment sits unchanged past tornTailAfter, the writer is
	// dead and the file is reaped.
	backdateSpool(t, cfg.SpoolDir, tornTailAfter+time.Minute)
	err = runExporterUntil(t, cfg, func() bool { return len(spoolFiles(cfg.SpoolDir)) == 0 })
	if err != nil {
		t.Fatalf("reap run: %v", err)
	}
	if got := srv.recordCount(t); got != 2 {
		t.Errorf("reaping must not re-ship, got %d records", got)
	}
}

func TestExporterLockExcludesSecondInstance(t *testing.T) {
	srv := newOTLPServer(t, nil)
	cfg := spoolTwoRecords(t, srv.URL+"/v1/logs", nil)

	release, held, err := filelock.TryLock(filepath.Join(cfg.SpoolDir, exporterLockName))
	if err != nil || !held {
		t.Fatalf("acquiring test lock: held=%v err=%v", held, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := RunExporter(ctx, cfg); err != nil {
		t.Fatalf("a locked-out exporter waits, then shuts down cleanly: %v", err)
	}
	if got := srv.requestCount(); got != 0 {
		t.Errorf("locked-out exporter must not ship, got %d requests", got)
	}
	release()

	if err := runExporterUntil(t, cfg, func() bool { return srv.recordCount(t) >= 2 }); err != nil {
		t.Fatalf("post-release run: %v", err)
	}
}

func TestExporterStartSweepDropsExpiredFilesWithoutShipping(t *testing.T) {
	srv := newOTLPServer(t, nil)
	cfg := spoolTwoRecords(t, srv.URL+"/v1/logs", nil)
	backdateSpool(t, cfg.SpoolDir, maxSpoolAge+time.Hour)

	err := runExporterUntil(t, cfg, func() bool { return len(spoolFiles(cfg.SpoolDir)) == 0 })
	if err != nil {
		t.Fatalf("RunExporter: %v", err)
	}
	if got := srv.requestCount(); got != 0 {
		t.Errorf("expired files must sweep, not ship; got %d requests", got)
	}
}

func TestRunExporterRejectsBadConfig(t *testing.T) {
	if err := RunExporter(context.Background(), ExporterConfig{SpoolDir: t.TempDir()}); err == nil {
		t.Errorf("missing endpoint must error")
	}
	cfg := testExporterConfig(t.TempDir(), "not a url", nil)
	if err := RunExporter(context.Background(), cfg); err == nil {
		t.Errorf("malformed endpoint must error")
	}
}

func TestResolveExporterConfigEnvAndFlagPrecedence(t *testing.T) {
	base := ExporterConfig{
		SpoolDir: "/base/spool",
		Logs:     SignalConfig{Endpoint: "https://base.example.com/v1/logs", Headers: map[string]string{"Gram-Key": "base"}},
	}

	// General env endpoint gains the signal path; recorder config loses.
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://env.example.com/")
	cfg, err := resolveExporterConfig(base, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Logs.Endpoint != "https://env.example.com/v1/logs" {
		t.Errorf("general env endpoint = %q, want the /v1/logs path appended", cfg.Logs.Endpoint)
	}

	// The logs-specific endpoint wins over the general one, verbatim.
	t.Setenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", "https://logs.example.com/custom/path")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "Gram-Key=env%20key,Gram-Project=proj")
	t.Setenv("OTEL_EXPORTER_OTLP_TIMEOUT", "2500")
	cfg, err = resolveExporterConfig(base, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Logs.Endpoint != "https://logs.example.com/custom/path" {
		t.Errorf("logs env endpoint = %q, want verbatim signal-specific value", cfg.Logs.Endpoint)
	}
	if cfg.Logs.Headers["Gram-Key"] != "env key" || cfg.Logs.Headers["Gram-Project"] != "proj" {
		t.Errorf("env headers = %v, want percent-decoded pairs", cfg.Logs.Headers)
	}
	if cfg.Logs.Timeout != 2500*time.Millisecond {
		t.Errorf("env timeout = %v, want 2.5s", cfg.Logs.Timeout)
	}

	// Flags override everything.
	cfg, err = resolveExporterConfig(base, []string{
		"--endpoint=https://flag.example.com/v1/logs",
		"--header=Gram-Key=flagkey",
		"--spool-dir=/flag/spool",
		"--timeout=7s",
		"--interval=5s",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Logs.Endpoint != "https://flag.example.com/v1/logs" {
		t.Errorf("flag endpoint lost: %q", cfg.Logs.Endpoint)
	}
	if cfg.Logs.Headers["Gram-Key"] != "flagkey" || cfg.Logs.Headers["Gram-Project"] != "proj" {
		t.Errorf("flag header must override per key: %v", cfg.Logs.Headers)
	}
	if cfg.SpoolDir != "/flag/spool" || cfg.Logs.Timeout != 7*time.Second || cfg.Interval != 5*time.Second {
		t.Errorf("flag overrides lost: %+v", cfg)
	}

	if _, err := resolveExporterConfig(base, []string{"--bogus"}); err == nil {
		t.Errorf("unknown exporter flag must error")
	}
}

func TestIsDefinitiveExportError(t *testing.T) {
	for s, want := range map[string]bool{
		"failed to upload: 401 Unauthorized":          true,
		"failed to upload: 403 Forbidden":             true,
		"failed to upload: 404 Not Found":             true,
		"failed to upload: 400 Bad Request":           true,
		"failed to upload: 503 Service Unavailable":   false,
		"dial tcp 127.0.0.1:4010: connection refused": false,
	} {
		if got := isDefinitiveExportError(errors.New(s)); got != want {
			t.Errorf("isDefinitiveExportError(%q) = %v, want %v", s, got, want)
		}
	}
}
