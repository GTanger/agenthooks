package telemetry

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	cpb "go.opentelemetry.io/proto/otlp/common/v1"
	lpb "go.opentelemetry.io/proto/otlp/logs/v1"
	rpb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/encoding/protojson"
)

// The spool is where the SDK meets disk: the hook process's LoggerProvider
// runs a custom exporter whose Export appends records to NDJSON spool files
// instead of touching the network. Entries are protojson-encoded OTLP
// LogRecords — already wire-shaped, losslessly round-trippable to the
// protobuf the exporter puts on the wire, and human-readable for debugging.
// Resource and scope are written once per file header, not per line.
//
// Layout: <spoolDir>/<unixnano>-<pid>.ndjson, created with O_APPEND; lexical
// order = chrono order. No tmp/rename dance is needed for append-only
// NDJSON — a torn last line is detected and skipped by the exporter's
// tailer.

const (
	spoolFileSuffix = ".ndjson"

	maxSpoolAge    = 14 * 24 * time.Hour
	maxSpoolBytes  = 64 << 20 // total across the spool dir
	maxRecordBytes = 1 << 20  // one encoded spool line
	maxFileBytes   = 8 << 20  // rotate the writing file past this size
)

// spoolHeader is the first line of every spool file: format version, the
// endpoint fingerprint the records were configured for, and the OTLP
// resource/scope shared by every record in the file.
type spoolHeader struct {
	V          int             `json:"v"`
	EndpointID string          `json:"endpoint_id"`
	Resource   json.RawMessage `json:"resource,omitempty"`
	Scope      json.RawMessage `json:"scope,omitempty"`
	SchemaURL  string          `json:"schema_url,omitempty"`
}

// spoolLine is every subsequent line: one protojson-encoded OTLP LogRecord.
type spoolLine struct {
	V          int             `json:"v"`
	EndpointID string          `json:"endpoint_id"`
	Record     json.RawMessage `json:"record,omitempty"`
}

// spoolExporter implements sdk/log.Exporter by appending records to the
// spool. Each export opens, appends, and closes the process's spool file —
// no handle is held between events, so a concurrent exporter daemon (or, on
// Windows, anything at all) can always delete shipped files, and a file
// deleted mid-session is simply recreated with a fresh header on the next
// event. All failure paths drop the record and stash the error for the
// caller to log — never an error to the hook pipeline (§4.8).
type spoolExporter struct {
	dir        string
	endpointID string

	mu       sync.Mutex
	fileName string // this process's spool file; rotated past maxFileBytes
	swept    bool
	lastErr  error
}

func newSpoolExporter(dir, endpointID string) *spoolExporter {
	return &spoolExporter{dir: dir, endpointID: endpointID}
}

// Export appends the records to the process's spool file. It always returns
// nil: surfacing the error through the SDK would only reach the OTel global
// error handler (stderr noise in the hook process); the runner tap reads it
// via takeErr and logs a proper warning instead.
func (e *spoolExporter) Export(_ context.Context, records []sdklog.Record) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.swept {
		// Write-time caps, enforced lock-free like the relay's trimSpool:
		// one sweep per process is enough for process-per-event hooks and
		// keeps serve mode bounded via the per-append budget check below.
		sweepSpool(e.dir, time.Now(), e.fileName)
		e.swept = true
	}
	for i := range records {
		if err := e.appendRecord(&records[i]); err != nil {
			e.lastErr = err
		}
	}
	return nil
}

func (e *spoolExporter) appendRecord(rec *sdklog.Record) error {
	entry, err := protojson.Marshal(toProtoRecord(rec))
	if err != nil {
		return fmt.Errorf("telemetry: encoding spool record: %w", err)
	}
	line, err := json.Marshal(spoolLine{V: 1, EndpointID: e.endpointID, Record: entry})
	if err != nil {
		return fmt.Errorf("telemetry: encoding spool line: %w", err)
	}
	line = append(line, '\n')
	if len(line) > maxRecordBytes {
		return fmt.Errorf("telemetry: record of %d bytes exceeds the %d byte cap; dropped", len(line), maxRecordBytes)
	}
	if spoolSize(e.dir)+int64(len(line)) > maxSpoolBytes {
		sweepSpool(e.dir, time.Now(), e.fileName)
		if spoolSize(e.dir)+int64(len(line)) > maxSpoolBytes {
			return fmt.Errorf("telemetry: spool over the %d byte cap; record dropped", int64(maxSpoolBytes))
		}
	}
	if e.fileName == "" {
		e.fileName = strconv.FormatInt(time.Now().UnixNano(), 10) + "-" + strconv.Itoa(os.Getpid()) + spoolFileSuffix
	}
	path := filepath.Join(e.dir, e.fileName)
	if info, err := os.Stat(path); err == nil && info.Size() > maxFileBytes {
		e.fileName = strconv.FormatInt(time.Now().UnixNano(), 10) + "-" + strconv.Itoa(os.Getpid()) + spoolFileSuffix
		path = filepath.Join(e.dir, e.fileName)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("telemetry: opening spool file: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("telemetry: stat spool file: %w", err)
	}
	if info.Size() == 0 {
		header, err := e.headerLine(rec)
		if err != nil {
			return err
		}
		line = append(header, line...)
	}
	if _, err := f.Write(line); err != nil {
		return fmt.Errorf("telemetry: appending spool record: %w", err)
	}
	return nil
}

// headerLine builds the file header carrying the record's resource and
// instrumentation scope.
func (e *spoolExporter) headerLine(rec *sdklog.Record) ([]byte, error) {
	hdr := spoolHeader{V: 1, EndpointID: e.endpointID}
	res := rec.Resource()
	if res.Len() > 0 {
		if raw, err := protojson.Marshal(&rpb.Resource{Attributes: attrIter(res.Iter())}); err == nil {
			hdr.Resource = raw
		}
		hdr.SchemaURL = res.SchemaURL()
	}
	scope := rec.InstrumentationScope()
	if raw, err := protojson.Marshal(&cpb.InstrumentationScope{Name: scope.Name, Version: scope.Version}); err == nil {
		hdr.Scope = raw
	}
	line, err := json.Marshal(hdr)
	if err != nil {
		return nil, fmt.Errorf("telemetry: encoding spool header: %w", err)
	}
	return append(line, '\n'), nil
}

// takeErr returns and clears the last append error, letting RecordHook
// surface failures the SDK's Emit path swallows.
func (e *spoolExporter) takeErr() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	err := e.lastErr
	e.lastErr = nil
	return err
}

// ForceFlush implements sdk/log.Exporter; appends are synchronous, so there
// is nothing buffered to flush.
func (e *spoolExporter) ForceFlush(context.Context) error { return nil }

// Shutdown implements sdk/log.Exporter; no handle is held between exports.
func (e *spoolExporter) Shutdown(context.Context) error { return nil }

// spoolFiles lists the spool's record files in lexical (= chronological)
// order.
func spoolFiles(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, ent := range entries {
		if !ent.IsDir() && strings.HasSuffix(ent.Name(), spoolFileSuffix) {
			names = append(names, ent.Name())
		}
	}
	sort.Strings(names)
	return names
}

func spoolSize(dir string) int64 {
	var total int64
	for _, name := range spoolFiles(dir) {
		if info, err := os.Stat(filepath.Join(dir, name)); err == nil {
			total += info.Size()
		}
	}
	return total
}

// sweepSpool enforces the age and size caps: files older than maxSpoolAge
// are removed, then oldest files go first until the spool fits the byte
// budget. keep names a file that must survive the size sweep (the writer's
// open file). Lock-free and best-effort — a racing exporter deleting the
// same file is harmless.
func sweepSpool(dir string, now time.Time, keep string) {
	names := spoolFiles(dir)
	type fileInfo struct {
		name string
		size int64
	}
	var live []fileInfo
	var total int64
	for _, name := range names {
		path := filepath.Join(dir, name)
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) > maxSpoolAge {
			_ = os.Remove(path)
			continue
		}
		live = append(live, fileInfo{name: name, size: info.Size()})
		total += info.Size()
	}
	for _, fi := range live {
		if total <= maxSpoolBytes {
			return
		}
		if fi.name == keep {
			continue
		}
		_ = os.Remove(filepath.Join(dir, fi.name))
		total -= fi.size
	}
}

// toProtoRecord transforms an sdk/log record into the OTLP LogRecord the
// spool stores and the exporter replays — the same mapping the official
// exporter's transform performs.
func toProtoRecord(rec *sdklog.Record) *lpb.LogRecord {
	pr := &lpb.LogRecord{
		TimeUnixNano:         timeUnixNano(rec.Timestamp()),
		ObservedTimeUnixNano: timeUnixNano(rec.ObservedTimestamp()),
		EventName:            rec.EventName(),
		SeverityNumber:       severityNumber(rec.Severity()),
		SeverityText:         rec.SeverityText(),
		Body:                 attrValue(rec.Body()),
		Attributes:           make([]*cpb.KeyValue, 0, rec.AttributesLen()),
		Flags:                uint32(rec.TraceFlags()),
	}
	rec.WalkAttributes(func(kv attribute.KeyValue) bool {
		pr.Attributes = append(pr.Attributes, attrKeyValue(kv))
		return true
	})
	if tid := rec.TraceID(); tid.IsValid() {
		pr.TraceId = tid[:]
	}
	if sid := rec.SpanID(); sid.IsValid() {
		pr.SpanId = sid[:]
	}
	return pr
}

// severityNumber maps a log severity onto the OTLP severity number; the two
// enums share values 1..24.
func severityNumber(s log.Severity) lpb.SeverityNumber {
	if s < log.SeverityTrace1 || s > log.SeverityFatal4 {
		return lpb.SeverityNumber_SEVERITY_NUMBER_UNSPECIFIED
	}
	return lpb.SeverityNumber(s) //nolint:gosec // bounded to 1..24 above
}

func timeUnixNano(t time.Time) uint64 {
	if t.IsZero() {
		return 0
	}
	nano := t.UnixNano()
	if nano < 0 {
		return 0
	}
	return uint64(nano)
}

func attrIter(iter attribute.Iterator) []*cpb.KeyValue {
	out := make([]*cpb.KeyValue, 0, iter.Len())
	for iter.Next() {
		out = append(out, attrKeyValue(iter.Attribute()))
	}
	return out
}

func attrKeyValue(kv attribute.KeyValue) *cpb.KeyValue {
	return &cpb.KeyValue{Key: string(kv.Key), Value: attrValue(kv.Value)}
}

func attrValue(v attribute.Value) *cpb.AnyValue {
	switch v.Type() {
	case attribute.EMPTY:
		return nil
	case attribute.BOOL:
		return &cpb.AnyValue{Value: &cpb.AnyValue_BoolValue{BoolValue: v.AsBool()}}
	case attribute.INT64:
		return &cpb.AnyValue{Value: &cpb.AnyValue_IntValue{IntValue: v.AsInt64()}}
	case attribute.FLOAT64:
		return &cpb.AnyValue{Value: &cpb.AnyValue_DoubleValue{DoubleValue: v.AsFloat64()}}
	case attribute.STRING:
		return &cpb.AnyValue{Value: &cpb.AnyValue_StringValue{StringValue: v.AsString()}}
	case attribute.BOOLSLICE:
		vals := v.AsBoolSlice()
		out := make([]*cpb.AnyValue, 0, len(vals))
		for _, b := range vals {
			out = append(out, &cpb.AnyValue{Value: &cpb.AnyValue_BoolValue{BoolValue: b}})
		}
		return arrayValue(out)
	case attribute.INT64SLICE:
		vals := v.AsInt64Slice()
		out := make([]*cpb.AnyValue, 0, len(vals))
		for _, n := range vals {
			out = append(out, &cpb.AnyValue{Value: &cpb.AnyValue_IntValue{IntValue: n}})
		}
		return arrayValue(out)
	case attribute.FLOAT64SLICE:
		vals := v.AsFloat64Slice()
		out := make([]*cpb.AnyValue, 0, len(vals))
		for _, f := range vals {
			out = append(out, &cpb.AnyValue{Value: &cpb.AnyValue_DoubleValue{DoubleValue: f}})
		}
		return arrayValue(out)
	case attribute.STRINGSLICE:
		vals := v.AsStringSlice()
		out := make([]*cpb.AnyValue, 0, len(vals))
		for _, s := range vals {
			out = append(out, &cpb.AnyValue{Value: &cpb.AnyValue_StringValue{StringValue: s}})
		}
		return arrayValue(out)
	}
	return &cpb.AnyValue{Value: &cpb.AnyValue_StringValue{StringValue: v.String()}}
}

func arrayValue(vals []*cpb.AnyValue) *cpb.AnyValue {
	return &cpb.AnyValue{Value: &cpb.AnyValue_ArrayValue{ArrayValue: &cpb.ArrayValue{Values: vals}}}
}

// resourceFromProto rebuilds the SDK resource a spool header carries, for
// the exporter's replay provider.
func resourceFromProto(raw json.RawMessage, schemaURL string) (*resource.Resource, error) {
	if len(raw) == 0 {
		return resource.Empty(), nil
	}
	var pb rpb.Resource
	if err := protojson.Unmarshal(raw, &pb); err != nil {
		return nil, err
	}
	attrs := make([]attribute.KeyValue, 0, len(pb.GetAttributes()))
	for _, kv := range pb.GetAttributes() {
		attrs = append(attrs, attribute.KeyValue{Key: attribute.Key(kv.GetKey()), Value: fromAnyValue(kv.GetValue())})
	}
	if schemaURL == "" {
		return resource.NewSchemaless(attrs...), nil
	}
	return resource.NewWithAttributes(schemaURL, attrs...), nil
}

// fromAnyValue maps an OTLP AnyValue back onto an attribute.Value. Shapes
// the attribute model cannot express (kvlist, bytes) fall back to their JSON
// text — this package never emits them.
func fromAnyValue(v *cpb.AnyValue) attribute.Value {
	switch val := v.GetValue().(type) {
	case *cpb.AnyValue_BoolValue:
		return attribute.BoolValue(val.BoolValue)
	case *cpb.AnyValue_IntValue:
		return attribute.Int64Value(val.IntValue)
	case *cpb.AnyValue_DoubleValue:
		return attribute.Float64Value(val.DoubleValue)
	case *cpb.AnyValue_StringValue:
		return attribute.StringValue(val.StringValue)
	case *cpb.AnyValue_ArrayValue:
		vals := val.ArrayValue.GetValues()
		strs := make([]string, 0, len(vals))
		allStrings := true
		for _, item := range vals {
			s, ok := item.GetValue().(*cpb.AnyValue_StringValue)
			if !ok {
				allStrings = false
				break
			}
			strs = append(strs, s.StringValue)
		}
		if allStrings {
			return attribute.StringSliceValue(strs)
		}
	}
	if raw, err := protojson.Marshal(v); err == nil {
		return attribute.StringValue(string(raw))
	}
	return attribute.StringValue("")
}
