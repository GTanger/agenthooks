package telemetry

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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

// The detached shipper: a re-exec of the consumer binary (the runner's
// --agenthooks-internal-telemetry-ship flag) that replays spooled records
// through the official otlploghttp exporter. The SDK exporter owns the wire
// — protobuf encoding, HTTP, gzip, per-request timeout, transient
// retry/backoff — while this file owns process lifecycle: the ship lock,
// file ordering, per-file flush-then-delete, torn-tail handling, and the run
// budget. Deterministic record identity (§4.4) makes any re-send after a
// partial flush harmless.

const (
	// shipRunBudget bounds one detached shipper run; the next hook event
	// re-arms the debounce and picks up whatever is left.
	shipRunBudget = 60 * time.Second
	// shipExportTimeout is the per-request timeout handed to the exporter.
	shipExportTimeout = 10 * time.Second
	// shipMinFileAge keeps the shipper away from files another process may
	// still be appending to.
	shipMinFileAge = 2 * time.Second
	// shipReplayedAfter marks records shipped long after they were spooled
	// as downtime backlog: gram.hook.replayed=true, the key gram's
	// dashboards already read for spool-drain redeliveries.
	shipReplayedAfter = 5 * time.Minute
)

// shipConfig is the single-line JSON stdin payload the spawner hands the
// detached shipper — spool location plus endpoint config, off argv so
// credentials never appear in a process list.
type shipConfig struct {
	V        int               `json:"v"`
	SpoolDir string            `json:"spool_dir"`
	Endpoint string            `json:"endpoint"`
	Headers  map[string]string `json:"headers,omitempty"`
}

// RunShip reads the ship config from stdin and drains the spool once. It is
// invoked by agenthooks.Main when the internal ship flag is present;
// consumer binaries can also call it to implement an explicit
// "telemetry ship" subcommand fed by MaybeSpawnShipper's payload shape.
func RunShip(stdin io.Reader) error {
	rd := bufio.NewReaderSize(io.LimitReader(stdin, maxRecordBytes), 64<<10)
	line, err := rd.ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("telemetry: reading ship config: %w", err)
	}
	var cfg shipConfig
	if err := json.Unmarshal(line, &cfg); err != nil {
		return fmt.Errorf("telemetry: decoding ship config: %w", err)
	}
	if cfg.SpoolDir == "" || cfg.Endpoint == "" {
		return errors.New("telemetry: ship config requires spool_dir and endpoint")
	}
	return runShip(cfg)
}

// runShip drains one spool directory: lock, sweep, then ship files oldest-
// first, deleting each after its records are flushed. Any export error ends
// the run and leaves the remaining files in place — including definitive
// 4xx, where retrying in this run cannot help.
func runShip(cfg shipConfig) error {
	release, held, err := filelock.TryLock(filepath.Join(cfg.SpoolDir, shipLockName))
	if err != nil {
		return fmt.Errorf("telemetry: ship lock: %w", err)
	}
	if !held {
		return nil // another shipper owns this spool
	}
	defer release()

	sweepSpool(cfg.SpoolDir, time.Now(), "")

	ctx, cancel := context.WithTimeout(context.Background(), shipRunBudget)
	defer cancel()

	wantID := endpointID(cfg.Endpoint, cfg.Headers)
	for _, name := range spoolFiles(cfg.SpoolDir) {
		if ctx.Err() != nil {
			return nil // budget spent; the rest ships next run
		}
		path := filepath.Join(cfg.SpoolDir, name)
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		if age := time.Since(info.ModTime()); age < shipMinFileAge {
			// A live hook process may still be appending — the spawner's
			// own file is always this fresh. Wait out the age and re-check
			// rather than stranding the tail until the next run.
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(shipMinFileAge - age):
			}
			latest, err := os.Stat(path)
			if err != nil || !latest.ModTime().Equal(info.ModTime()) {
				continue // still being written (or gone): the next run gets it
			}
		}
		if err := shipFile(ctx, cfg, wantID, path); err != nil {
			return fmt.Errorf("telemetry: shipping %s: %w", name, err)
		}
	}
	return nil
}

// shipFile replays one spool file through a fresh SDK pipeline
// (BatchProcessor + otlploghttp exporter built from the file's header
// resource/scope), then flushes and deletes it. A nil error means the file
// is gone or intentionally skipped; an export error leaves it in place.
func shipFile(ctx context.Context, cfg shipConfig, wantID, path string) error {
	header, records, ok := readSpoolFile(path)
	if !ok {
		return nil // unreadable header: leave for the age sweep
	}
	if header.EndpointID != wantID {
		return nil // spooled under a different endpoint config
	}
	if len(records) == 0 {
		return os.Remove(path)
	}

	res, err := resourceFromProto(header.Resource, header.SchemaURL)
	if err != nil {
		res = resourceFromProtoFallback()
	}
	exporter, err := otlploghttp.New(ctx,
		otlploghttp.WithEndpointURL(cfg.Endpoint),
		otlploghttp.WithHeaders(cfg.Headers),
		otlploghttp.WithTimeout(shipExportTimeout),
		otlploghttp.WithCompression(otlploghttp.GzipCompression),
	)
	if err != nil {
		return err
	}
	provider := sdklog.NewLoggerProvider(
		sdklog.WithResource(res),
		sdklog.WithProcessor(sdklog.NewBatchProcessor(exporter)),
	)
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), shipExportTimeout)
		defer cancel()
		_ = provider.Shutdown(shutCtx)
	}()

	scopeName, scopeVersion := scopeFromHeader(header)
	logger := provider.Logger(scopeName, log.WithInstrumentationVersion(scopeVersion))
	now := time.Now()
	for _, pr := range records {
		rec, sc := replayRecord(pr, now)
		logger.Emit(trace.ContextWithSpanContext(ctx, sc), rec)
	}
	if err := provider.ForceFlush(ctx); err != nil {
		return err
	}
	return os.Remove(path)
}

// readSpoolFile parses one spool file into its header and record lines. A
// torn last line — the crash artifact of an interrupted append — is skipped;
// corrupt lines elsewhere are skipped too. ok is false when the header is
// missing or unreadable.
func readSpoolFile(path string) (spoolHeader, []*lpb.LogRecord, bool) {
	f, err := os.Open(path)
	if err != nil {
		return spoolHeader{}, nil, false
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), maxRecordBytes+1024)
	if !sc.Scan() {
		return spoolHeader{}, nil, false
	}
	var header spoolHeader
	if err := json.Unmarshal(sc.Bytes(), &header); err != nil || header.V != 1 {
		return spoolHeader{}, nil, false
	}
	var records []*lpb.LogRecord
	for sc.Scan() {
		var line spoolLine
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil || len(line.Record) == 0 {
			continue // torn tail or corrupt line: skip, never fail the file
		}
		var pr lpb.LogRecord
		if err := protojson.Unmarshal(line.Record, &pr); err != nil {
			continue
		}
		records = append(records, &pr)
	}
	return header, records, true
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
	scc.TraceFlags = trace.TraceFlags(pr.GetFlags())
	return rec, trace.NewSpanContext(scc)
}

// resourceFromProtoFallback is the resource used when a header's resource
// payload cannot be decoded: identity-free but keeps records flowing.
func resourceFromProtoFallback() *resource.Resource {
	return resource.NewSchemaless(attribute.String("event.origin", "agenthooks"))
}
