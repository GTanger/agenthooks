// Package telemetry emits one OpenTelemetry log record (a wide event) per
// hook event, spooled to local disk in the hook process and shipped over
// OTLP/HTTP by a separate long-running exporter process — never adding
// network latency (or process spawning) to the hook's critical path.
//
// Wire the recorder into a Runner with agenthooks.WithTelemetry:
//
//	rec, err := telemetry.New(telemetry.Config{
//		Endpoint: "https://app.getgram.ai/rpc/hooks.otel/v1/logs",
//		Headers:  map[string]string{"Gram-Key": key, "Gram-Project": project},
//	})
//	if err != nil { ... }
//	r := agenthooks.New(agenthooks.WithTelemetry(rec))
//
// Hook processes only append to the spool. Delivery is the exporter's job:
// `mybinary agenthooks exporter`, a daemon started and supervised
// externally (a service manager or device agent), which tails the spool and
// ships it (see ExporterMain and RunExporter). Until an exporter runs,
// records sit in the spool, bounded by the age/size caps.
//
// The feature is opt-in and fail-open by construction: without the option
// nothing changes; with it, recorder failures degrade to a logged warning
// and never affect the decision path. Any OTLP logs endpoint works; gram is
// one consumer configuration.
package telemetry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/trace"

	"github.com/speakeasy-api/agenthooks/internal/hookrecord"
)

// CaptureLevel selects how much event content leaves the process.
type CaptureLevel int

const (
	// CaptureAttributes (the default) emits structured attributes only: no
	// prompt text, no tool input/output bodies, no assistant messages, no
	// cwd. Sizes and SHA-256 digests stand in for content.
	CaptureAttributes CaptureLevel = iota
	// CaptureContent additionally records prompt text, tool input/output,
	// and assistant messages — after the built-in transport-credential
	// redaction and the consumer's Redactor.
	CaptureContent
)

// Config configures a Recorder. Misconfiguration fails at New — construction
// time, in the consumer's control — not at event time.
type Config struct {
	// Endpoint is the OTLP/HTTP logs endpoint, e.g.
	// "https://app.getgram.ai/rpc/hooks.otel/v1/logs" or any collector's
	// "/v1/logs". Required.
	Endpoint string
	// Headers are added to every export request (auth: e.g. Gram-Key,
	// Gram-Project).
	Headers map[string]string
	// Resource attributes merged over the library defaults (service.name,
	// service.version, host.name, os.type, host.arch,
	// gram.event.origin=agenthooks, ...).
	Resource map[string]string

	// SpoolDir overrides the spool location. Default:
	// $XDG_STATE_HOME/agenthooks/telemetry/spool, falling back to
	// os.UserCacheDir.
	SpoolDir string
	// Capture selects the content level. Default: CaptureAttributes.
	Capture CaptureLevel
	// Redactor rewrites attribute and body values before they touch disk.
	// It is called with the attribute key (the body uses key "body") and
	// the value, and returns the replacement. The library always applies
	// its built-in transport-credential redaction (URLs, commands, token-
	// shaped values) first; Redactor runs after it.
	Redactor func(key string, value string) string

	// HonorTraceparent opts into W3C trace-context parenting: when the
	// hook process carries a valid TRACEPARENT environment variable, its
	// trace ID and sampled flag replace the deterministic trace ID on
	// emitted records, and the deterministic ID moves to the
	// agenthooks.deterministic_trace_id attribute so hash-derived joins
	// (e.g. gram's) still work. Off by default: the deterministic
	// derivation is what backend joins key on, and an ambient trace ID
	// would silently regroup records. Each record keeps its own
	// deterministic span ID either way.
	HonorTraceparent bool
}

// Recorder captures hook events as OTel log records into the disk spool.
// Construct with New; install with agenthooks.WithTelemetry. Methods are
// safe for concurrent use.
type Recorder struct {
	cfg        Config
	spoolDir   string
	endpointID string
	spool      *spoolExporter
	provider   *sdklog.LoggerProvider
	logger     log.Logger

	// Ambient W3C trace context, read once at construction when
	// Config.HonorTraceparent is set and TRACEPARENT is valid.
	ambientTrace trace.TraceID
	ambientFlags trace.TraceFlags
	ambientOK    bool
}

// scopeName identifies this package as the instrumentation scope of every
// emitted record.
const scopeName = "github.com/speakeasy-api/agenthooks/telemetry"

// New builds a Recorder: it validates the endpoint, creates the spool
// directory, and stands up the sdk/log pipeline — a synchronous simple
// processor feeding the spool exporter. No network I/O happens here or on
// any later Recorder call; delivery is the exporter daemon's job.
func New(cfg Config) (*Recorder, error) {
	endpoint := strings.TrimSpace(cfg.Endpoint)
	if endpoint == "" {
		return nil, errors.New("telemetry: Config.Endpoint is required")
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, errors.New("telemetry: Config.Endpoint is not a valid URL: " + err.Error())
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("telemetry: Config.Endpoint must be an absolute http(s) URL")
	}
	cfg.Endpoint = endpoint

	dir := cfg.SpoolDir
	if dir == "" {
		dir, err = defaultSpoolDir()
		if err != nil {
			return nil, errors.New("telemetry: resolving spool dir: " + err.Error())
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, errors.New("telemetry: creating spool dir: " + err.Error())
	}

	res, err := buildResource(cfg.Resource)
	if err != nil {
		return nil, errors.New("telemetry: building resource: " + err.Error())
	}

	id := endpointID(cfg.Endpoint, cfg.Headers)
	spool := newSpoolExporter(dir, id)
	provider := sdklog.NewLoggerProvider(
		sdklog.WithResource(res),
		sdklog.WithProcessor(sdklog.NewSimpleProcessor(spool)),
	)
	r := &Recorder{
		cfg:        cfg,
		spoolDir:   dir,
		endpointID: id,
		spool:      spool,
		provider:   provider,
		logger:     provider.Logger(scopeName, log.WithInstrumentationVersion(agenthooksVersion())),
	}
	if cfg.HonorTraceparent {
		r.ambientTrace, r.ambientFlags, r.ambientOK = parseTraceparent(os.Getenv("TRACEPARENT"))
	}
	return r, nil
}

// RecordHook captures one post-decision hook event: it builds the log
// record, injects the deterministic trace/span identity via a synthetic span
// context on the emit context, and appends the record to the spool through
// the synchronous SDK pipeline — one buffered file append, no network.
//
// RecordHook is invoked by the runner tap agenthooks.WithTelemetry installs.
// Its parameter type lives in an internal package, so it is not callable by
// external consumers.
func (r *Recorder) RecordHook(hr *hookrecord.Record) error {
	rec, sc := r.buildRecord(hr)
	ctx := trace.ContextWithSpanContext(context.Background(), sc)
	r.logger.Emit(ctx, rec)
	// The simple processor exports synchronously, but Logger.Emit swallows
	// the exporter's error; surface it so the runner can log a warning.
	return r.spool.takeErr()
}

// defaultSpoolDir resolves the platform spool location:
// $XDG_STATE_HOME/agenthooks/telemetry/spool when set, else the user cache
// dir (%LOCALAPPDATA% on Windows).
func defaultSpoolDir() (string, error) {
	if s := os.Getenv("XDG_STATE_HOME"); s != "" {
		return filepath.Join(s, "agenthooks", "telemetry", "spool"), nil
	}
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "agenthooks", "telemetry", "spool"), nil
}

// endpointID fingerprints the delivery target (endpoint + headers) so spool
// files written under one configuration are never shipped to another.
func endpointID(endpoint string, headers map[string]string) string {
	h := sha256.New()
	h.Write([]byte(endpoint))
	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		h.Write([]byte{'\n'})
		h.Write([]byte(k))
		h.Write([]byte{':'})
		h.Write([]byte(headers[k]))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// buildResource merges the library defaults with the consumer's overrides.
func buildResource(extra map[string]string) (*resource.Resource, error) {
	attrs := []attribute.KeyValue{
		attribute.String("service.name", serviceName()),
		attribute.String("os.type", runtime.GOOS),
		attribute.String("host.arch", runtime.GOARCH),
		attribute.String("gram.event.origin", "agenthooks"),
		attribute.String("agenthooks.version", agenthooksVersion()),
	}
	if host, err := os.Hostname(); err == nil && host != "" {
		attrs = append(attrs, attribute.String("host.name", host))
	}
	if v := binaryVersion(); v != "" {
		attrs = append(attrs, attribute.String("service.version", v))
	}
	for k, v := range extra {
		attrs = append(attrs, attribute.String(k, v))
	}
	return resource.Merge(resource.Default(), resource.NewSchemaless(attrs...))
}

func serviceName() string {
	exe, err := os.Executable()
	if err != nil || exe == "" {
		return "agenthooks"
	}
	return strings.TrimSuffix(filepath.Base(exe), ".exe")
}

// agenthooksVersion reports this module's version as built into the consumer
// binary, or "unknown" outside module builds.
func agenthooksVersion() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	const module = "github.com/speakeasy-api/agenthooks"
	if bi.Main.Path == module && bi.Main.Version != "" {
		return bi.Main.Version
	}
	for _, dep := range bi.Deps {
		if dep.Path == module && dep.Version != "" {
			return dep.Version
		}
	}
	return "unknown"
}

// binaryVersion reports the consumer binary's own module version, "" when
// unavailable.
func binaryVersion() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok || bi.Main.Version == "" || bi.Main.Version == "(devel)" {
		return ""
	}
	return bi.Main.Version
}
