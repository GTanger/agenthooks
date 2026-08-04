package telemetry

import (
	"bytes"
	"compress/gzip"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

// spoolTwoRecords writes a tool.pre and a tool.post record through a
// recorder configured for the given endpoint, backdating the file so the
// shipper does not treat it as mid-append.
func spoolTwoRecords(t *testing.T, endpoint string, headers map[string]string) shipConfig {
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
	return shipConfig{V: 1, SpoolDir: dir, Endpoint: endpoint, Headers: headers}
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

func TestShipperDeliversBatchThenDeletesFile(t *testing.T) {
	srv := newOTLPServer(t, nil)
	headers := map[string]string{"Gram-Key": "test-key"}
	cfg := spoolTwoRecords(t, srv.URL+"/v1/logs", headers)

	if err := runShip(cfg); err != nil {
		t.Fatalf("runShip: %v", err)
	}
	if got := srv.requestCount(); got != 1 {
		t.Fatalf("requests = %d, want 1 (both records in one batch)", got)
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
	if resAttrs["event.origin"] != "agenthooks" {
		t.Errorf("shipped resource lost event.origin: %v", resAttrs)
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
	if files := spoolFiles(cfg.SpoolDir); len(files) != 0 {
		t.Errorf("shipped file must be deleted, got %v", files)
	}
}

func TestShipperRetriesTransientFailure(t *testing.T) {
	srv := newOTLPServer(t, func(n int) int {
		if n == 0 {
			return http.StatusServiceUnavailable
		}
		return http.StatusOK
	})
	cfg := spoolTwoRecords(t, srv.URL+"/v1/logs", nil)

	if err := runShip(cfg); err != nil {
		t.Fatalf("runShip after transient failure: %v", err)
	}
	if got := srv.requestCount(); got < 2 {
		t.Errorf("requests = %d, want a retry after the 503", got)
	}
	if files := spoolFiles(cfg.SpoolDir); len(files) != 0 {
		t.Errorf("file must be deleted once the retry lands, got %v", files)
	}
}

func TestShipperStopsOnDefinitive4xxAndLeavesFiles(t *testing.T) {
	srv := newOTLPServer(t, func(int) int { return http.StatusUnauthorized })
	cfg := spoolTwoRecords(t, srv.URL+"/v1/logs", nil)

	if err := runShip(cfg); err == nil {
		t.Fatalf("definitive 4xx must end the run with an error")
	}
	if got := srv.requestCount(); got != 1 {
		t.Errorf("requests = %d, want 1 (no retry on a permanent 4xx)", got)
	}
	if files := spoolFiles(cfg.SpoolDir); len(files) != 1 {
		t.Errorf("files must stay for a later run, got %v", files)
	}
}

func TestShipperSkipsFilesFromOtherEndpointConfigs(t *testing.T) {
	srv := newOTLPServer(t, nil)
	cfg := spoolTwoRecords(t, srv.URL+"/v1/logs", map[string]string{"Gram-Key": "original"})

	other := cfg
	other.Headers = map[string]string{"Gram-Key": "different"}
	if err := runShip(other); err != nil {
		t.Fatalf("runShip: %v", err)
	}
	if got := srv.requestCount(); got != 0 {
		t.Errorf("records spooled under another endpoint config must not ship, got %d requests", got)
	}
	if files := spoolFiles(cfg.SpoolDir); len(files) != 1 {
		t.Errorf("foreign-config file must remain, got %v", files)
	}
}

func TestShipperSkipsTornTail(t *testing.T) {
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

	if err := runShip(cfg); err != nil {
		t.Fatalf("runShip: %v", err)
	}
	if got := len(srv.allRecords(t)); got != 2 {
		t.Errorf("shipped records = %d, want 2 intact records (torn tail skipped)", got)
	}
	if files := spoolFiles(cfg.SpoolDir); len(files) != 0 {
		t.Errorf("file with a torn tail still ships and deletes, got %v", files)
	}
}

func TestShipperWaitsOutFreshFiles(t *testing.T) {
	srv := newOTLPServer(t, nil)
	headers := map[string]string{}
	dir := t.TempDir()
	rec, err := New(Config{Endpoint: srv.URL + "/v1/logs", Headers: headers, SpoolDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := rec.RecordHook(toolPreRecord()); err != nil {
		t.Fatal(err)
	}
	// No backdating: the file is seconds-fresh, as it is when the spawning
	// hook process just appended. The shipper must wait out the age window
	// and still deliver.
	if err := runShip(shipConfig{V: 1, SpoolDir: dir, Endpoint: srv.URL + "/v1/logs", Headers: headers}); err != nil {
		t.Fatalf("runShip: %v", err)
	}
	if got := len(srv.allRecords(t)); got != 1 {
		t.Errorf("fresh file must ship after the wait, got %d records", got)
	}
}

func TestShipperAgeSweepDropsExpiredFilesWithoutShipping(t *testing.T) {
	srv := newOTLPServer(t, nil)
	cfg := spoolTwoRecords(t, srv.URL+"/v1/logs", nil)
	backdateSpool(t, cfg.SpoolDir, maxSpoolAge+time.Hour)

	if err := runShip(cfg); err != nil {
		t.Fatalf("runShip: %v", err)
	}
	if got := srv.requestCount(); got != 0 {
		t.Errorf("expired files must sweep, not ship; got %d requests", got)
	}
	if files := spoolFiles(cfg.SpoolDir); len(files) != 0 {
		t.Errorf("expired files must be removed, got %v", files)
	}
}

func TestShipLockSerializesRuns(t *testing.T) {
	srv := newOTLPServer(t, nil)
	cfg := spoolTwoRecords(t, srv.URL+"/v1/logs", nil)

	release, held, err := filelock.TryLock(filepath.Join(cfg.SpoolDir, shipLockName))
	if err != nil || !held {
		t.Fatalf("acquiring test lock: held=%v err=%v", held, err)
	}
	if err := runShip(cfg); err != nil {
		t.Fatalf("locked-out run must be a clean no-op: %v", err)
	}
	if got := srv.requestCount(); got != 0 {
		t.Errorf("locked-out shipper must not ship, got %d requests", got)
	}
	release()

	if err := runShip(cfg); err != nil {
		t.Fatalf("runShip after release: %v", err)
	}
	if got := len(srv.allRecords(t)); got != 2 {
		t.Errorf("post-release run ships the backlog, got %d records", got)
	}
}

func TestMaybeSpawnShipperDebounce(t *testing.T) {
	rec := newTestRecorder(t, nil)
	if err := rec.RecordHook(toolPreRecord()); err != nil {
		t.Fatal(err)
	}

	var spawns int
	var payload string
	spawn := func(stdin io.Reader) error {
		spawns++
		b, _ := io.ReadAll(stdin)
		payload = string(b)
		return nil
	}
	rec.MaybeSpawnShipper(spawn)
	rec.MaybeSpawnShipper(spawn)
	if spawns != 1 {
		t.Fatalf("spawns = %d, want 1 (debounced)", spawns)
	}
	if !strings.Contains(payload, `"spool_dir"`) || !strings.Contains(payload, testEndpoint) {
		t.Errorf("spawn payload must carry spool dir and endpoint config: %s", payload)
	}
	if !strings.HasSuffix(payload, "\n") {
		t.Errorf("spawn payload must be a single JSON line")
	}

	// An expired debounce marker re-arms the spawn.
	marker := filepath.Join(rec.spoolDir, lastShipMarker)
	past := time.Now().Add(-shipDebounce - time.Second)
	if err := os.Chtimes(marker, past, past); err != nil {
		t.Fatal(err)
	}
	rec.MaybeSpawnShipper(spawn)
	if spawns != 2 {
		t.Errorf("spawns = %d, want 2 after the debounce window", spawns)
	}

	// An empty spool never spawns.
	for _, name := range spoolFiles(rec.spoolDir) {
		if err := os.Remove(filepath.Join(rec.spoolDir, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(marker, past, past); err != nil {
		t.Fatal(err)
	}
	rec.MaybeSpawnShipper(spawn)
	if spawns != 2 {
		t.Errorf("empty spool must not spawn, got %d", spawns)
	}
}

func TestRunShipRejectsBadConfig(t *testing.T) {
	if err := RunShip(bytes.NewReader([]byte("not json\n"))); err == nil {
		t.Errorf("malformed config must error")
	}
	if err := RunShip(bytes.NewReader([]byte(`{"v":1}`))); err == nil {
		t.Errorf("config without spool_dir/endpoint must error")
	}
}

func TestRunShipEndToEnd(t *testing.T) {
	srv := newOTLPServer(t, nil)
	cfg := spoolTwoRecords(t, srv.URL+"/v1/logs", map[string]string{"Gram-Key": "k"})
	line := `{"v":1,"spool_dir":` + strconv.Quote(cfg.SpoolDir) + `,"endpoint":` + strconv.Quote(cfg.Endpoint) + `,"headers":{"Gram-Key":"k"}}` + "\n"
	if err := RunShip(strings.NewReader(line)); err != nil {
		t.Fatalf("RunShip: %v", err)
	}
	if got := len(srv.allRecords(t)); got != 2 {
		t.Errorf("RunShip shipped %d records, want 2", got)
	}
}
