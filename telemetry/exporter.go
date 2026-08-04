package telemetry

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/trace"
	cpb "go.opentelemetry.io/proto/otlp/common/v1"
	lpb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/speakeasy-api/agenthooks/internal/filelock"
)

// The exporter: a long-running daemon, started and supervised externally
// (service manager / device agent), that tails the disk spool and ships
// records over OTLP/HTTP. Hook processes only append to the spool — nothing
// is spawned from the hook path. The SDK exporter owns the wire — protobuf
// encoding, HTTP, gzip, per-request timeout, transient retry/backoff — while
// this file owns the daemon lifecycle: the exporter lock, directory polling,
// per-file offset checkpoints, delete-after-ship, sweeps, graceful shutdown.
// Delivery is at-least-once: the checkpoint is advanced only after a
// successful export, and deterministic record identity (§4.4) makes any
// re-ship after a crash or checkpoint loss harmless.

const (
	// exporterLockName serializes exporters on one spool: a second instance
	// waits for the lock rather than double-shipping.
	exporterLockName = "exporter.lock"
	// exporterCheckpointName is the persisted tail state: per-file byte
	// offsets of the first unshipped byte.
	exporterCheckpointName = "exporter.checkpoint"

	// defaultPollInterval is the spool poll cadence. Polling (not a
	// platform watcher) on purpose: it is Windows-safe, needs no
	// dependencies, and a couple of seconds of shipping latency is
	// irrelevant for telemetry.
	defaultPollInterval = 2 * time.Second
	// defaultExportTimeout is the per-request timeout handed to the SDK
	// exporter.
	defaultExportTimeout = 10 * time.Second
	// exportChunk bounds how many records are emitted between ForceFlush
	// calls, keeping the batch processor's queue from overflowing (it
	// drops, not blocks) and the checkpoint granular.
	exportChunk = 512
	// deleteGrace keeps deletion away from files modified moments ago — a
	// hook process may sit between its open-append-close and another
	// append. Fresh files are still tailed; only deletion waits.
	deleteGrace = 2 * time.Second
	// tornTailAfter is how long an unterminated trailing fragment (the
	// crash artifact of an interrupted append) must sit unchanged before
	// the file is considered dead and deleted anyway.
	tornTailAfter = 2 * time.Minute
	// shipReplayedAfter marks records shipped long after they were spooled
	// as downtime backlog: gram.hook.replayed=true, the key gram's
	// dashboards already read for spool-drain redeliveries.
	shipReplayedAfter = 5 * time.Minute
	// sweepEvery is the cadence of the age/size sweep, which the exporter
	// owns at runtime (the recorder keeps its append-side caps).
	sweepEvery = time.Minute

	// exportBackoffMin/Max bound the retry backoff after transient export
	// failures (network down, 5xx). Definitive config errors exit instead.
	exportBackoffMin = 2 * time.Second
	exportBackoffMax = 5 * time.Minute
)

// SignalConfig is the delivery configuration for one OTLP signal. Only logs
// exist today; the per-signal shape is deliberate so traces/metrics can be
// added to ExporterConfig later without breaking consumers.
type SignalConfig struct {
	// Endpoint is the full OTLP/HTTP endpoint URL for this signal, e.g.
	// "https://app.getgram.ai/rpc/hooks.otel/v1/logs". Required.
	Endpoint string
	// Headers are added to every export request (auth: e.g. Gram-Key).
	Headers map[string]string
	// Timeout is the per-request export timeout. Default 10s.
	Timeout time.Duration
}

// ExporterConfig configures RunExporter. Zero values take library defaults.
type ExporterConfig struct {
	// SpoolDir is the spool to drain. Default: the recorder's default
	// spool location ($XDG_STATE_HOME/agenthooks/telemetry/spool).
	SpoolDir string
	// Interval is the spool poll interval. Default 2s.
	Interval time.Duration
	// Logger receives exporter progress and errors. Default slog.Default().
	Logger *slog.Logger
	// Logs is the logs-signal delivery config.
	Logs SignalConfig
}

// ExporterMain backs the `mybinary agenthooks exporter` verb: it resolves
// the exporter config — the recorder's own Config as the base, the standard
// OTEL_EXPORTER_OTLP_* environment variables over it, command-line flags
// over both — installs SIGINT/SIGTERM handling for a graceful shutdown, and
// runs the exporter until the context ends. It returns the process exit
// code: 0 after a clean run or shutdown, 64 for flag errors, 1 for
// definitive delivery-config errors (bad endpoint, rejected credentials) so
// an external supervisor surfaces the failure instead of a silent stall.
//
// Flags (each overrides the corresponding env var and recorder config):
//
//	--endpoint=URL       full OTLP/HTTP logs endpoint
//	--header=K=V         export request header; repeatable
//	--spool-dir=PATH     spool directory to drain
//	--timeout=DURATION   per-request export timeout
//	--interval=DURATION  spool poll interval
func (r *Recorder) ExporterMain(ctx context.Context, args []string, stderr io.Writer) int {
	base := ExporterConfig{
		SpoolDir: r.spoolDir,
		Logs: SignalConfig{
			Endpoint: r.cfg.Endpoint,
			Headers:  r.cfg.Headers,
		},
	}
	cfg, err := resolveExporterConfig(base, args)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "agenthooks:", err)
		return 64
	}
	cfg.Logger = slog.New(slog.NewTextHandler(stderr, nil))
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := RunExporter(ctx, cfg); err != nil {
		_, _ = fmt.Fprintln(stderr, "agenthooks: telemetry exporter:", err)
		return 1
	}
	return 0
}

// resolveExporterConfig layers the standard OTel exporter environment
// variables (signal-specific first, then general; endpoint/headers/timeout
// per the OTLP exporter spec) and then flags over the base config.
func resolveExporterConfig(base ExporterConfig, args []string) (ExporterConfig, error) {
	cfg := base
	if v := os.Getenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"); v != "" {
		cfg.Logs.Endpoint = v // signal-specific: used verbatim
	} else if v := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"); v != "" {
		// General endpoint: the spec appends the signal path.
		cfg.Logs.Endpoint = strings.TrimRight(v, "/") + "/v1/logs"
	}
	if v := envFirst("OTEL_EXPORTER_OTLP_LOGS_HEADERS", "OTEL_EXPORTER_OTLP_HEADERS"); v != "" {
		headers, err := parseHeadersList(v)
		if err != nil {
			return cfg, err
		}
		cfg.Logs.Headers = headers
	}
	if v := envFirst("OTEL_EXPORTER_OTLP_LOGS_TIMEOUT", "OTEL_EXPORTER_OTLP_TIMEOUT"); v != "" {
		ms, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil || ms <= 0 {
			return cfg, fmt.Errorf("telemetry: OTLP timeout env must be positive milliseconds, got %q", v)
		}
		cfg.Logs.Timeout = time.Duration(ms) * time.Millisecond
	}
	for _, a := range args {
		switch {
		case strings.HasPrefix(a, "--endpoint="):
			cfg.Logs.Endpoint = strings.TrimPrefix(a, "--endpoint=")
		case strings.HasPrefix(a, "--header="):
			k, v, ok := strings.Cut(strings.TrimPrefix(a, "--header="), "=")
			if !ok || k == "" {
				return cfg, fmt.Errorf("telemetry: --header wants K=V, got %q", a)
			}
			headers := make(map[string]string, len(cfg.Logs.Headers)+1)
			for hk, hv := range cfg.Logs.Headers {
				headers[hk] = hv
			}
			headers[k] = v
			cfg.Logs.Headers = headers
		case strings.HasPrefix(a, "--spool-dir="):
			cfg.SpoolDir = strings.TrimPrefix(a, "--spool-dir=")
		case strings.HasPrefix(a, "--timeout="):
			d, err := time.ParseDuration(strings.TrimPrefix(a, "--timeout="))
			if err != nil {
				return cfg, fmt.Errorf("telemetry: bad --timeout: %w", err)
			}
			cfg.Logs.Timeout = d
		case strings.HasPrefix(a, "--interval="):
			d, err := time.ParseDuration(strings.TrimPrefix(a, "--interval="))
			if err != nil {
				return cfg, fmt.Errorf("telemetry: bad --interval: %w", err)
			}
			cfg.Interval = d
		default:
			return cfg, fmt.Errorf("telemetry: unknown exporter flag %q", a)
		}
	}
	return cfg, nil
}

func envFirst(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

// parseHeadersList parses the OTLP exporter headers env format:
// comma-separated key=value pairs, values optionally percent-encoded.
func parseHeadersList(v string) (map[string]string, error) {
	headers := map[string]string{}
	for _, pair := range strings.Split(v, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		k, val, ok := strings.Cut(pair, "=")
		if !ok || strings.TrimSpace(k) == "" {
			return nil, fmt.Errorf("telemetry: OTLP headers env wants key=value pairs, got %q", pair)
		}
		if dec, err := url.QueryUnescape(strings.TrimSpace(val)); err == nil {
			val = dec
		}
		headers[strings.TrimSpace(k)] = val
	}
	return headers, nil
}

// RunExporter runs the telemetry exporter until ctx is canceled: it
// acquires the spool's exporter lock (waiting if another instance holds
// it), then polls the spool, shipping sealed files oldest-first and tailing
// growing ones with a persisted offset checkpoint. It returns nil on a
// graceful shutdown (in-flight batch flushed, checkpoint persisted) and an
// error only for definitive configuration failures — an unusable endpoint
// or an endpoint that rejects the request outright (HTTP 400/401/403/404) —
// so a supervisor restarts and surfaces it. Transient failures (network,
// 5xx) are retried forever with capped exponential backoff.
//
// Consumers that don't use the `agenthooks exporter` verb can call this
// directly to embed the exporter in their own daemon.
func RunExporter(ctx context.Context, cfg ExporterConfig) error {
	endpoint := strings.TrimSpace(cfg.Logs.Endpoint)
	if endpoint == "" {
		return errors.New("telemetry: exporter needs a logs endpoint (config, OTEL_EXPORTER_OTLP_ENDPOINT, or --endpoint)")
	}
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("telemetry: exporter endpoint must be an absolute http(s) URL, got %q", endpoint)
	}
	dir := cfg.SpoolDir
	if dir == "" {
		if dir, err = defaultSpoolDir(); err != nil {
			return fmt.Errorf("telemetry: resolving spool dir: %w", err)
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("telemetry: creating spool dir: %w", err)
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = defaultPollInterval
	}
	timeout := cfg.Logs.Timeout
	if timeout <= 0 {
		timeout = defaultExportTimeout
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	release, err := acquireExporterLock(ctx, dir, logger)
	if err != nil || release == nil {
		return err // nil release: ctx ended while waiting — a clean shutdown
	}
	defer release()

	out, err := otlploghttp.New(ctx,
		otlploghttp.WithEndpointURL(endpoint),
		otlploghttp.WithHeaders(cfg.Logs.Headers),
		otlploghttp.WithTimeout(timeout),
		otlploghttp.WithCompression(otlploghttp.GzipCompression),
	)
	if err != nil {
		return fmt.Errorf("telemetry: building OTLP exporter: %w", err)
	}
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		_ = out.Shutdown(shutCtx)
	}()

	e := &exporter{
		dir:    dir,
		wantID: endpointID(endpoint, cfg.Logs.Headers),
		out:    out,
		cp:     loadCheckpoint(filepath.Join(dir, exporterCheckpointName)),
		logger: logger,
	}
	logger.Info("agenthooks telemetry exporter running", "spool_dir", dir, "endpoint", endpoint)

	sweepSpool(dir, time.Now(), "")
	lastSweep := time.Now()
	backoff := time.Duration(0)
	for {
		err := e.drainOnce(ctx)
		switch {
		case err == nil:
			backoff = 0
		case ctx.Err() != nil:
			// Shutdown in progress; the select below exits. Mid-flush
			// cancellation errors are not delivery failures.
		case isDefinitiveExportError(err):
			logger.Error("agenthooks telemetry exporter: endpoint rejected the export; exiting for the supervisor", "error", err)
			return err
		default:
			backoff = nextBackoff(backoff)
			logger.Warn("agenthooks telemetry exporter: transient export failure; backing off", "error", err, "backoff", backoff)
		}
		if time.Since(lastSweep) > sweepEvery {
			sweepSpool(dir, time.Now(), "")
			e.cp.prune(dir)
			lastSweep = time.Now()
		}
		wait := interval
		if backoff > wait {
			wait = backoff
		}
		select {
		case <-ctx.Done():
			// Graceful, idempotent shutdown: drainOnce flushes
			// synchronously and checkpoints after every chunk, so there is
			// no in-flight state beyond what is already persisted.
			logger.Info("agenthooks telemetry exporter: shutting down")
			return nil
		case <-time.After(wait):
		}
	}
}

// acquireExporterLock waits for the spool's exporter lock. It returns a nil
// release func (and nil error) when ctx ends first.
func acquireExporterLock(ctx context.Context, dir string, logger *slog.Logger) (func(), error) {
	waited := false
	for {
		release, held, err := filelock.TryLock(filepath.Join(dir, exporterLockName))
		if err != nil {
			return nil, fmt.Errorf("telemetry: exporter lock: %w", err)
		}
		if held {
			return release, nil
		}
		if !waited {
			logger.Info("agenthooks telemetry exporter: another exporter holds the spool lock; waiting")
			waited = true
		}
		select {
		case <-ctx.Done():
			return nil, nil
		case <-time.After(time.Second):
		}
	}
}

func nextBackoff(cur time.Duration) time.Duration {
	if cur <= 0 {
		return exportBackoffMin
	}
	if cur >= exportBackoffMax/2 {
		return exportBackoffMax
	}
	return cur * 2
}

// statusCodeRe matches embedded HTTP status codes in exporter errors; the
// SDK exporter does not expose the status structurally.
var statusCodeRe = regexp.MustCompile(`(^|[^0-9])(400|401|403|404)([^0-9]|$)`)

// isDefinitiveExportError reports whether an export failure is a
// configuration problem retrying cannot fix: the endpoint understood the
// request and rejected it (bad request, bad credentials, wrong path).
func isDefinitiveExportError(err error) bool {
	return err != nil && statusCodeRe.MatchString(err.Error())
}

// exporter is the daemon state around one spool directory.
type exporter struct {
	dir    string
	wantID string
	out    sdklog.Exporter
	cp     *checkpointState
	logger *slog.Logger
}

// drainOnce makes one pass over the spool, oldest file first. A transient
// export error stops the pass (preserving file order) and is returned for
// the caller's backoff.
func (e *exporter) drainOnce(ctx context.Context) error {
	for _, name := range spoolFiles(e.dir) {
		if err := ctx.Err(); err != nil {
			return err // shutting down: the caller's ctx check absorbs it
		}
		if err := e.processFile(ctx, name); err != nil {
			return err
		}
	}
	return nil
}

// processFile tails one spool file from its checkpointed offset: ships the
// complete lines that appeared since, advances the checkpoint after each
// flushed chunk, and deletes the file once fully shipped and quiescent.
func (e *exporter) processFile(ctx context.Context, name string) error {
	path := filepath.Join(e.dir, name)
	before, err := os.Stat(path)
	if err != nil {
		return nil //nolint:nilerr // vanished (sweep or racing cleanup): nothing to ship
	}
	tail, ok := readSpoolTail(path, e.cp.offset(name))
	if !ok {
		return nil // unreadable or header still being written: next tick
	}
	if tail.header.EndpointID != e.wantID {
		return nil // spooled for another delivery config; not this exporter's data
	}

	if len(tail.entries) > 0 {
		if err := e.shipEntries(ctx, name, tail); err != nil {
			return err
		}
	}

	// Deletion: only when everything shipped, nothing changed since the
	// read (a writer may have appended — the next tick tails it), and the
	// file is quiescent. A torn tail that sat unchanged past tornTailAfter
	// is a dead writer's crash artifact: unparseable, never completing.
	fullyShipped := !tail.torn && e.cp.offset(name) >= tail.size
	deadTorn := tail.torn && time.Since(before.ModTime()) > tornTailAfter
	if !fullyShipped && !deadTorn {
		return nil
	}
	latest, err := os.Stat(path)
	if err != nil {
		return nil //nolint:nilerr // vanished since the read: nothing to delete
	}
	if latest.Size() != before.Size() || !latest.ModTime().Equal(before.ModTime()) ||
		time.Since(latest.ModTime()) < deleteGrace {
		return nil
	}
	// Checkpoint entry goes first: if the delete then crashes, the file
	// re-ships whole (at-least-once); a stale entry surviving a delete
	// could silently skip a recreated file's records.
	e.cp.remove(name)
	e.cp.persist(e.logger)
	_ = os.Remove(path) // Windows sharing violation: the next tick retries
	return nil
}

// shipEntries replays the tail's records through a per-file SDK pipeline —
// the file header's resource and scope, a BatchProcessor, and the daemon's
// shared OTLP exporter — flushing and checkpointing every exportChunk
// records so the batch queue never overflows and progress survives a crash.
func (e *exporter) shipEntries(ctx context.Context, name string, tail spoolTail) error {
	res, err := resourceFromProto(tail.header.Resource, tail.header.SchemaURL)
	if err != nil {
		res = resourceFromProtoFallback()
	}
	provider := sdklog.NewLoggerProvider(
		sdklog.WithResource(res),
		sdklog.WithProcessor(sdklog.NewBatchProcessor(keepAliveExporter{e.out})),
	)
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), defaultExportTimeout)
		defer cancel()
		_ = provider.Shutdown(shutCtx) // stops the batch goroutine; the shared exporter survives
	}()

	scopeName, scopeVersion := scopeFromHeader(tail.header)
	logger := provider.Logger(scopeName, log.WithInstrumentationVersion(scopeVersion))
	now := time.Now()
	for start := 0; start < len(tail.entries); start += exportChunk {
		chunk := tail.entries[start:min(start+exportChunk, len(tail.entries))]
		for _, entry := range chunk {
			rec, sc := replayRecord(entry.rec, now)
			logger.Emit(trace.ContextWithSpanContext(ctx, sc), rec)
		}
		if err := provider.ForceFlush(ctx); err != nil {
			return fmt.Errorf("shipping %s: %w", name, err)
		}
		e.cp.set(name, chunk[len(chunk)-1].end)
		e.cp.persist(e.logger)
	}
	return nil
}

// keepAliveExporter shields the daemon's shared OTLP exporter from the
// per-file provider's Shutdown.
type keepAliveExporter struct{ sdklog.Exporter }

func (keepAliveExporter) Shutdown(context.Context) error { return nil }

// spoolTail is one file's unshipped content: parsed header, the complete
// record lines past the checkpoint offset (each with the byte offset its
// line ends at), whether an unterminated fragment trails them, and the file
// size at read time.
type spoolTail struct {
	header  spoolHeader
	entries []spoolEntry
	torn    bool
	size    int64
}

type spoolEntry struct {
	rec *lpb.LogRecord
	end int64 // absolute offset of the first byte after this record's line
}

// readSpoolTail reads a spool file's content from the given offset. Only
// newline-terminated lines are returned — an unterminated tail is an append
// in progress (or a crash artifact) and is never shipped, so no age
// heuristics are needed to tail a growing file. Terminated lines that fail
// to parse are skipped and the offset advances past them. ok is false when
// the file or its header line is missing or unreadable.
func readSpoolTail(path string, from int64) (spoolTail, bool) {
	f, err := os.Open(path)
	if err != nil {
		return spoolTail{}, false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return spoolTail{}, false
	}
	tail := spoolTail{size: info.Size()}

	br := bufio.NewReaderSize(f, 64<<10)
	headerLine, err := br.ReadBytes('\n')
	if err != nil {
		return spoolTail{}, false // header still being written (or empty file)
	}
	if err := json.Unmarshal(headerLine, &tail.header); err != nil || tail.header.V != 1 {
		return spoolTail{}, false
	}
	pos := int64(len(headerLine))
	if from > pos {
		if from > tail.size {
			// Smaller than the checkpoint says: the file was replaced
			// after a checkpointed generation. Start over (re-ship is
			// harmless; skipping would lose records).
			from = pos
		}
		if _, err := f.Seek(from, io.SeekStart); err != nil {
			return spoolTail{}, false
		}
		br.Reset(f)
		pos = from
	}

	for {
		line, err := br.ReadBytes('\n')
		if err != nil {
			if len(line) > 0 {
				tail.torn = true // unterminated fragment: mid-append or crash
			}
			return tail, true
		}
		pos += int64(len(line))
		var sl spoolLine
		if err := json.Unmarshal(line, &sl); err != nil || len(sl.Record) == 0 {
			continue // corrupt line: skip, advance past it
		}
		var pr lpb.LogRecord
		if err := protojson.Unmarshal(sl.Record, &pr); err != nil {
			continue
		}
		tail.entries = append(tail.entries, spoolEntry{rec: &pr, end: pos})
	}
}

// checkpointState is the exporter's persisted tail state: file name → byte
// offset of the first unshipped byte. Persisted via write-temp-then-rename;
// a lost or corrupt checkpoint only causes re-shipping (at-least-once).
type checkpointState struct {
	path  string
	files map[string]int64
}

type checkpointDoc struct {
	V     int              `json:"v"`
	Files map[string]int64 `json:"files"`
}

func loadCheckpoint(path string) *checkpointState {
	cp := &checkpointState{path: path, files: map[string]int64{}}
	raw, err := os.ReadFile(path)
	if err != nil {
		return cp
	}
	var doc checkpointDoc
	if err := json.Unmarshal(raw, &doc); err != nil || doc.V != 1 {
		return cp
	}
	if doc.Files != nil {
		cp.files = doc.Files
	}
	return cp
}

func (c *checkpointState) offset(name string) int64 { return c.files[name] }

func (c *checkpointState) set(name string, off int64) { c.files[name] = off }

func (c *checkpointState) remove(name string) { delete(c.files, name) }

// prune drops entries for spool files that no longer exist.
func (c *checkpointState) prune(dir string) {
	live := map[string]bool{}
	for _, name := range spoolFiles(dir) {
		live[name] = true
	}
	for name := range c.files {
		if !live[name] {
			delete(c.files, name)
		}
	}
}

func (c *checkpointState) persist(logger *slog.Logger) {
	raw, err := json.Marshal(checkpointDoc{V: 1, Files: c.files})
	if err != nil {
		return
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		logger.Warn("agenthooks telemetry exporter: writing checkpoint", "error", err)
		return
	}
	if err := os.Rename(tmp, c.path); err != nil {
		logger.Warn("agenthooks telemetry exporter: replacing checkpoint", "error", err)
	}
}

func scopeFromHeader(header spoolHeader) (name, version string) {
	name = scopeName
	if len(header.Scope) == 0 {
		return name, version
	}
	var scope cpb.InstrumentationScope
	if err := protojson.Unmarshal(header.Scope, &scope); err != nil {
		return name, version
	}
	if scope.GetName() != "" {
		name = scope.GetName()
	}
	return name, scope.GetVersion()
}

// replayRecord rebuilds the API record from its spooled OTLP form with the
// spooled timestamps preserved, and the spooled trace/span identity carried
// on a synthetic span context for re-injection at Emit.
func replayRecord(pr *lpb.LogRecord, now time.Time) (log.Record, trace.SpanContext) {
	var rec log.Record
	observed := time.Unix(0, int64(min(pr.GetObservedTimeUnixNano(), 1<<62))) //nolint:gosec // clamped
	if pr.GetTimeUnixNano() > 0 {
		rec.SetTimestamp(time.Unix(0, int64(min(pr.GetTimeUnixNano(), 1<<62)))) //nolint:gosec // clamped
	}
	if pr.GetObservedTimeUnixNano() > 0 {
		rec.SetObservedTimestamp(observed)
	}
	rec.SetEventName(pr.GetEventName())
	rec.SetSeverity(log.Severity(pr.GetSeverityNumber()))
	rec.SetSeverityText(pr.GetSeverityText())
	if body := pr.GetBody(); body != nil {
		rec.SetBody(fromAnyValue(body))
	}
	attrs := make([]attribute.KeyValue, 0, len(pr.GetAttributes())+1)
	for _, kv := range pr.GetAttributes() {
		attrs = append(attrs, attribute.KeyValue{Key: attribute.Key(kv.GetKey()), Value: fromAnyValue(kv.GetValue())})
	}
	if pr.GetObservedTimeUnixNano() > 0 && now.Sub(observed) > shipReplayedAfter {
		attrs = append(attrs, attribute.Bool("gram.hook.replayed", true))
	}
	rec.AddAttributes(attrs...)

	var scc trace.SpanContextConfig
	if tid := pr.GetTraceId(); len(tid) == 16 {
		copy(scc.TraceID[:], tid)
	}
	if sid := pr.GetSpanId(); len(sid) == 8 {
		copy(scc.SpanID[:], sid)
	}
	scc.TraceFlags = trace.TraceFlags(pr.GetFlags()) //nolint:gosec // low byte only
	return rec, trace.NewSpanContext(scc)
}

// resourceFromProtoFallback is the resource used when a header's resource
// payload cannot be decoded: identity-free but keeps records flowing.
func resourceFromProtoFallback() *resource.Resource {
	return resource.NewSchemaless(attribute.String("gram.event.origin", "agenthooks"))
}
