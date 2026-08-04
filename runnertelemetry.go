package agenthooks

import (
	"io"
	"time"

	"github.com/speakeasy-api/agenthooks/internal/hookrecord"
	"github.com/speakeasy-api/agenthooks/telemetry"
)

// telemetryShipFlag re-execs this binary as the detached telemetry shipper,
// following the --agenthooks-internal-* convention of the MCP inventory
// warms. The spool location and endpoint config arrive as a single-line
// JSON stdin payload (the codex launch-context pattern) so credentials
// never appear in argv.
const telemetryShipFlag = "--agenthooks-internal-telemetry-ship"

// recordTiming carries the tap's timing view of one event: the library
// receive time and the dispatch-to-response-encoded duration — hook
// overhead, distinct from tool execution time.
type recordTiming struct {
	receive  time.Time
	duration time.Duration
}

// afterDecision is the internal post-decision hook invoked by Runner.Run
// after applyPolicy and wire encoding, and by the OpenCode serve loop after
// the reply is encoded. Unlike OnAny observers — which run before the
// decision pipeline — it sees the final applied decision, the timing, the
// handler error, and where the decision came from ("handler", "policy" when
// the runner's failure policy substituted it, "backfill" for synthesized
// reporting-only events). WithTelemetry installs one.
type afterDecision func(typed any, base *Event, core decisionCore, timing recordTiming, herr error, source string)

// WithTelemetry installs rec as the runner's telemetry recorder: one OTel
// log record per hook event, appended to the local disk spool after the
// decision is on the wire and shipped asynchronously by a detached process.
// Opt-in and fail-open by construction — without the option nothing changes;
// with it, a recorder failure degrades to a logged warning, never an error
// on the pipeline. See the telemetry package for configuration.
//
// Runner.Decide does not record telemetry: it has no wire edge and its
// callers own their own observability.
func WithTelemetry(rec *telemetry.Recorder) Option {
	return func(r *Runner) {
		if rec == nil {
			return
		}
		r.afterDecision = func(typed any, base *Event, core decisionCore, timing recordTiming, herr error, source string) {
			if err := rec.RecordHook(buildHookRecord(typed, base, core, timing, herr, source)); err != nil {
				r.logger.Warn("agenthooks: telemetry record failed", "error", err)
				return
			}
			rec.MaybeSpawnShipper(r.telemetryShipStart)
		}
	}
}

// tapAfterDecision delivers the post-decision snapshot to the telemetry
// recorder. encodedAt is sampled at the encoding boundary so the duration
// measures dispatch-to-response-encoded, not the provider write. Fail-open,
// always: the tap runs after the response is written, is panic-guarded like
// observers, and its work is bounded I/O with no network — recorder errors
// log a warning and never change a decision, delay a response, or surface
// as a hook failure.
func (r *Runner) tapAfterDecision(typed any, base *Event, core decisionCore, herr error, encodedAt time.Time, source string) {
	if r.afterDecision == nil {
		return
	}
	defer func() {
		if p := recover(); p != nil {
			r.logger.Warn("agenthooks: telemetry tap panic", "panic", p)
		}
	}()
	timing := recordTiming{receive: base.Time, duration: encodedAt.Sub(base.Time)}
	r.afterDecision(typed, base, core, timing, herr, source)
}

// buildHookRecord projects the typed event plus the final decision into the
// flat record the telemetry package consumes.
func buildHookRecord(typed any, base *Event, core decisionCore, timing recordTiming, herr error, source string) *hookrecord.Record {
	hr := &hookrecord.Record{
		Provider:       string(base.Provider),
		Variant:        string(base.Variant),
		NativeName:     base.NativeName,
		Kind:           string(base.Kind),
		Time:           timing.receive,
		Backfilled:     base.Backfilled,
		SessionID:      base.Session.ID,
		TurnID:         base.Session.TurnID,
		CWD:            base.Session.CWD,
		Model:          base.Session.Model,
		UserEmail:      base.Session.UserEmail,
		HookDurationMS: float64(timing.duration) / float64(time.Millisecond),
		Decision: hookrecord.Decision{
			Kind:     core.kind.String(),
			Reason:   core.reason,
			Blocking: core.blocks(),
			Source:   source,
		},
	}
	if herr != nil {
		hr.HandlerErr = herr.Error()
	}
	if base.Agent != nil {
		hr.SubagentID = base.Agent.ID
		hr.SubagentType = base.Agent.Type
	}
	switch ev := typed.(type) {
	case *PromptEvent:
		hr.Prompt = ev.Prompt
	case *ToolPreEvent:
		hr.Tool = toolHookRecord(&ev.Tool, nil)
	case *PermissionEvent:
		hr.Tool = toolHookRecord(&ev.Tool, nil)
	case *ToolPostEvent:
		hr.Tool = toolHookRecord(&ev.Tool, ev)
	case *StopEvent:
		hr.FinalMessage = ev.FinalMessage
		hr.LoopCount = ev.LoopCount
		if u := ev.Usage; u != nil {
			hr.Usage = &hookrecord.Usage{
				InputTokens:      u.InputTokens,
				OutputTokens:     u.OutputTokens,
				CacheReadTokens:  u.CacheReadTokens,
				CacheWriteTokens: u.CacheWriteTokens,
				Cost:             u.Cost,
			}
		}
	case *SessionStartEvent:
		hr.SessionSource = ev.Source
	case *SessionEndEvent:
		hr.SessionEndReason = ev.Reason
	case *CompactEvent:
		hr.CompactTrigger = ev.Trigger
	case *NotificationEvent:
		hr.Notification = ev.Message
	case *FileEditedEvent:
		hr.FilePath = ev.Path
	}
	return hr
}

func toolHookRecord(t *ToolCall, post *ToolPostEvent) *hookrecord.Tool {
	tr := &hookrecord.Tool{
		ID:          t.ID,
		Synthesized: t.Synthesized,
		Name:        t.Name,
		Canonical:   string(t.Canonical),
		Input:       t.Input,
	}
	if m := t.MCP; m != nil {
		tr.MCP = &hookrecord.MCP{
			Server:     m.Server,
			Tool:       m.Tool,
			URL:        m.URL,
			Command:    m.Command,
			FromConfig: m.FromConfig,
		}
	}
	if post != nil {
		tr.Output = post.Output
		tr.Failed = post.Failed
		tr.Error = post.Error
		tr.DurationMS = post.DurationMS
	}
	return tr
}

// startTelemetryShip is the self-exec hook MaybeSpawnShipper drives: a
// detached copy of this binary running only the internal ship flag, config
// streamed over stdin.
func startTelemetryShip(stdin io.Reader) error {
	return startDetachedSelf([]string{telemetryShipFlag}, stdin)
}
