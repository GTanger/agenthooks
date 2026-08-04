package telemetry

import (
	"crypto/rand"
	"crypto/sha256"
	"strconv"
	"time"

	"go.opentelemetry.io/otel/trace"
)

// Deterministic trace-context identity, reproducing gram's derivation
// byte-for-byte so agent-emitted and server-derived rows for the same event
// share trace IDs and existing joins keep working (RFC §4.4). The reference
// implementation is gram's canonicalTraceID / hashToolCallIDToTraceID /
// syntheticToolCallID (server/internal/hooks/ingest_hooks.go,
// server/internal/hooks/impl.go):
//
//  1. tool events with a per-call id      → SHA-256(toolCallID)[:16]
//  2. tool events without one             → SHA-256(len(sessionID) + "|" +
//     sessionID + "|" + toolName)[:16]
//  3. everything else with a session id   → SHA-256(sessionID)[:16]
//  4. last resort                         → random
//
// In this library rule 1 covers effectively every tool event — ToolCall.ID
// is the native id or the synthesized hook_synth_* id, the same value the
// relay sends today — but the full ladder is reproduced so any input hashes
// identically to gram's.

// deriveTraceID returns the trace ID for an event and whether it was
// deterministically derived. ok is false only on the random fallback
// (empty session id on a non-tool event), which callers flag with
// agenthooks.session.unidentified=true.
func deriveTraceID(isTool bool, toolCallID, sessionID, toolName string) (trace.TraceID, bool) {
	switch {
	case isTool && toolCallID != "":
		return traceIDFrom(toolCallID), true
	case isTool && sessionID != "" && toolName != "":
		// gram's syntheticToolCallID: the session id is length-prefixed so
		// the encoding is injective even when session ids contain "|".
		return traceIDFrom(strconv.Itoa(len(sessionID)) + "|" + sessionID + "|" + toolName), true
	case sessionID != "":
		return traceIDFrom(sessionID), true
	}
	var id trace.TraceID
	_, _ = rand.Read(id[:])
	return id, false
}

// traceIDFrom is gram's hashToolCallIDToTraceID: the first 16 bytes of the
// key's SHA-256.
func traceIDFrom(key string) trace.TraceID {
	sum := sha256.Sum256([]byte(key))
	var id trace.TraceID
	copy(id[:], sum[:16])
	return id
}

// deriveSpanID is deterministic per event:
// SHA-256("agenthooks|event|" + sessionID + "|" + turnID + "|" + nativeName +
// "|" + toolCallID + "|" + receiveTimeNanos)[:8]. Identical double-fires and
// spool replays collide onto the same (trace_id, span_id) and dedupe at the
// storage layer; nothing joins on span ids today (gram's are random).
func deriveSpanID(sessionID, turnID, nativeName, toolCallID string, receive time.Time) trace.SpanID {
	key := "agenthooks|event|" + sessionID + "|" + turnID + "|" + nativeName + "|" +
		toolCallID + "|" + strconv.FormatInt(receive.UnixNano(), 10)
	sum := sha256.Sum256([]byte(key))
	var id trace.SpanID
	copy(id[:], sum[:8])
	return id
}
