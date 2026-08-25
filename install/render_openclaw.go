package install

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"time"

	"github.com/speakeasy-api/agenthooks"
)

// openclawKindHooks maps subscribed unified kinds to OpenClaw typed plugin
// hook names. KindStop also subscribes llm_output: agent_end carries no final
// message or usage, so the shim caches the turn's llm_output and splices
// finalMessage/usage into the agent_end frame (mirroring the OpenCode
// session.idle splice). KindToolError rides after_tool_call too — failed and
// blocked calls arrive on the same native hook (quirk #37).
var openclawKindHooks = map[agenthooks.EventKind][]string{
	agenthooks.KindToolPre:         {"before_tool_call"},
	agenthooks.KindToolPost:        {"after_tool_call"},
	agenthooks.KindToolError:       {"after_tool_call"},
	agenthooks.KindPromptSubmitted: {"before_agent_run"},
	agenthooks.KindSessionStart:    {"session_start"},
	agenthooks.KindSessionEnd:      {"session_end"},
	agenthooks.KindStop:            {"agent_end", "llm_output"},
	agenthooks.KindSubagentStart:   {"subagent_spawned"},
	agenthooks.KindSubagentStop:    {"subagent_ended"},
	agenthooks.KindModelRequest:    {"llm_input"},
	agenthooks.KindModelResponse:   {"llm_output"},
	agenthooks.KindCompactPre:      {"before_compaction"},
	agenthooks.KindCompactPost:     {"after_compaction"},
}

// openclawGateHooks are the hooks whose replies gate the agent; each carries
// its own shim-owned deadline from the manifest's blocking spec.
var openclawGateHooks = map[agenthooks.EventKind]string{
	agenthooks.KindToolPre:         "before_tool_call",
	agenthooks.KindPromptSubmitted: "before_agent_run",
}

const openclawDefaultGateTimeout = 10 * time.Second

// renderOpenClaw writes a native OpenClaw plugin (openclaw.plugin.json +
// package.json + index.js) that proxies typed api.on hooks to the consumer
// binary over NDJSON stdio (agenthooks serve --provider=openclaw). Install the
// rendered directory with `openclaw plugins install <dir>` and restart the
// Gateway. Conversation-scope hooks (before_agent_run, llm_*, agent_end)
// additionally require plugins.entries.<id>.hooks.allowConversationAccess:
// true in the OpenClaw config (quirk #35).
func renderOpenClaw(m Manifest, _ Target) (fs.FS, error) {
	cmd, err := json.Marshal(m.Command)
	if err != nil {
		return nil, err
	}

	seen := map[string]bool{}
	var hooks []string
	subscribe := func(h string) {
		if !seen[h] {
			seen[h] = true
			hooks = append(hooks, h)
		}
	}
	for _, spec := range m.Hooks {
		for _, h := range openclawKindHooks[spec.Kind] {
			subscribe(h)
		}
	}
	// Always observe gateway lifecycle: it carries the daemon shutdown signal,
	// and forwarding it sanitized (the raw ctx includes the full Gateway
	// config with auth secrets) keeps the fidelity channel honest.
	subscribe("gateway_start")
	subscribe("gateway_stop")
	hooksJSON, err := json.Marshal(hooks)
	if err != nil {
		return nil, err
	}

	// The shim owns the gate deadlines: OpenClaw applies no default timeout to
	// typed hook handlers (quirk #36), so an unbounded consumer would stall
	// the agent turn indefinitely. Each gate keeps its own manifest timeout.
	gateTimeouts := map[string]int64{}
	maxGate := openclawDefaultGateTimeout
	for _, spec := range m.Hooks {
		hook, ok := openclawGateHooks[spec.Kind]
		if !ok || !spec.Blocking {
			continue
		}
		t := spec.Timeout
		if t <= 0 {
			t = openclawDefaultGateTimeout
		}
		if prev, ok := gateTimeouts[hook]; !ok || t.Milliseconds() > prev {
			gateTimeouts[hook] = t.Milliseconds()
		}
		if t > maxGate {
			maxGate = t
		}
	}
	gateTimeoutsJSON, err := json.Marshal(gateTimeouts)
	if err != nil {
		return nil, err
	}

	id := m.Identity.Name
	if id == "" {
		id = "agenthooks"
	}
	name := m.Identity.Name
	if name == "" {
		name = "agenthooks"
	}
	desc := m.Identity.Description
	if desc == "" {
		desc = "Proxies OpenClaw plugin hooks to the agenthooks consumer binary."
	}
	version := m.Identity.Version
	if version == "" {
		version = "0.0.0"
	}

	manifest, err := jsonFile(map[string]any{
		"id":          id,
		"name":        name,
		"description": desc,
		"version":     version,
		"configSchema": map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties":           map[string]any{},
		},
		"activation": map[string]any{"onStartup": true},
	})
	if err != nil {
		return nil, err
	}

	// The package.json openclaw.extensions entry is what `openclaw plugins
	// install` keys plugin detection on — without it the installer falls back
	// to hook-pack detection and rejects the directory (HOOK.md missing). The
	// entry must be plain JavaScript: package installs reject TypeScript
	// entries ("compiled runtime output required"); TS is dev-path only.
	pkg, err := jsonFile(map[string]any{
		"name":     "openclaw-plugin-" + id,
		"version":  version,
		"type":     "module",
		"private":  true,
		"openclaw": map[string]any{"extensions": []string{"./index.js"}},
	})
	if err != nil {
		return nil, err
	}

	shim := fmt.Sprintf(openClawShimTemplate,
		string(cmd), maxGate.String(), string(hooksJSON), string(gateTimeoutsJSON),
		openclawDefaultGateTimeout.Milliseconds(),
		m.Fail == agenthooks.FailClosed,
		jsString(id), jsString(name), jsString(desc))
	return memFS(map[string][]byte{
		"openclaw.plugin.json": manifest,
		"package.json":         pkg,
		"index.js":             []byte(shim),
	}), nil
}

func jsString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

const openClawShimTemplate = `// Generated by agenthooks install — do not edit.
// Proxies OpenClaw typed plugin hooks to the consumer binary over NDJSON
// stdio (agenthooks serve --provider=openclaw). Frames: {seq, hook, event,
// ctx} -> {seq, output?}; the reply output is returned verbatim as the hook
// handler's return value (block/requireApproval/params on before_tool_call,
// the gate decision on before_agent_run). Plain JavaScript: OpenClaw package
// installs reject TypeScript entry modules.
import { spawn } from "node:child_process"
import { createInterface } from "node:readline"

const COMMAND = %s
const SERVE_ARGS = ["agenthooks", "serve", "--provider=openclaw", "--timeout=%s"]
const HOOKS = %s
// Gating hooks await the reply under a per-hook shim-owned deadline (OpenClaw
// itself applies none); every other hook is fire-and-forget so telemetry
// never stalls the agent turn.
const GATE_TIMEOUT_MS = %s
const DEFAULT_TIMEOUT_MS = %d
const FAIL_CLOSED = %t

export default {
  id: %s,
  name: %s,
  description: %s,
  register(api) {
    const child = spawn(COMMAND[0], [...COMMAND.slice(1), ...SERVE_ARGS], {
      stdio: ["pipe", "pipe", "inherit"],
    })
    let seq = 0
    const pending = new Map()
    // agent_end carries no final message or usage; cache the turn's
    // llm_output and splice it into the agent_end frame.
    const llmByRun = new Map()

    createInterface({ input: child.stdout }).on("line", (line) => {
      if (!line.trim()) return
      let reply
      try {
        reply = JSON.parse(line)
      } catch {
        return
      }
      const resolve = pending.get(reply.seq)
      if (!resolve) return
      pending.delete(reply.seq)
      resolve(reply)
    })
    child.on("exit", () => {
      // An exited consumer cannot evaluate gates: resolve as timed out so
      // FAIL_CLOSED applies instead of silently allowing.
      for (const [, resolve] of pending) resolve({ timedOut: true })
      pending.clear()
    })

    const call = (hook, event, ctx, timeoutMs) => {
      if (child.exitCode !== null || !child.stdin?.writable) {
        return Promise.resolve({ timedOut: true })
      }
      const id = ++seq
      // Gate frames carry the shim deadline so the daemon can stop working
      // as soon as the shim gives up (observe frames omit it).
      const frame = { seq: id, hook, event, ctx }
      if (timeoutMs !== undefined) frame.timeoutMs = timeoutMs
      child.stdin.write(JSON.stringify(frame) + "\n")
      return new Promise((resolve) => {
        pending.set(id, resolve)
        const timer = setTimeout(() => {
          if (pending.delete(id)) resolve({ timedOut: true })
        }, timeoutMs ?? DEFAULT_TIMEOUT_MS)
        if (typeof timer.unref === "function") timer.unref()
      })
    }

    // The daemon never reads these history-sized fields (finalMessage/usage
    // ride the llm_output splice), and one oversized frame would be dropped
    // at the serve loop's size cap — strip them before they reach the pipe.
    const slimEvent = (hook, event) => {
      if (event == null || typeof event !== "object") return event
      if (hook === "agent_end" || hook === "before_agent_run") {
        const { messages, ...rest } = event
        return rest
      }
      if (hook === "llm_input") {
        const { historyMessages, ...rest } = event
        return rest
      }
      return event
    }

    const sanitizeCtx = (hook, ctx) => {
      if (hook !== "gateway_start" && hook !== "gateway_stop") return ctx
      // Gateway hooks hand plugins the full config including auth secrets
      // (observed 2026.6.34); never forward it.
      return { port: ctx?.port, workspaceDir: ctx?.workspaceDir }
    }

    const failClosedResult = (hook, event, reason) => {
      if (hook === "before_agent_run") {
        return { outcome: "block", reason }
      }
      // Tell the daemon this call was blocked locally so its after_tool_call
      // sibling still decodes as blocked rather than a successful completion.
      if (event?.toolCallId) {
        void call("gate_timeout", { toolCallId: event.toolCallId, reason }, null)
      }
      return { block: true, blockReason: reason }
    }

    for (const hook of HOOKS) {
      const gateTimeoutMs = GATE_TIMEOUT_MS[hook]
      api.on(hook, (rawEvent, ctx) => {
        const event = slimEvent(hook, rawEvent)
        if (hook === "llm_output") {
          const texts = Array.isArray(event?.assistantTexts) ? event.assistantTexts : []
          const key = event?.runId ?? event?.sessionId ?? ""
          llmByRun.set(key, { finalMessage: texts.join("\n") || undefined, usage: event?.usage })
          void call(hook, event, sanitizeCtx(hook, ctx))
          return
        }
        if (hook === "agent_end") {
          // Consume exactly the cache entry that serves the splice: llm_output
          // keys by runId when it has one and sessionId otherwise. Deleting
          // any other key could destroy a concurrent turn's pending entry;
          // leaving the consumed one would splice it into a later agent_end.
          const runKey = event?.runId ?? ""
          let cached = llmByRun.get(runKey)
          if (cached !== undefined) {
            llmByRun.delete(runKey)
          } else {
            const sessionKey = ctx?.sessionId ?? ""
            cached = llmByRun.get(sessionKey)
            if (cached !== undefined) llmByRun.delete(sessionKey)
          }
          const spliced = cached ? { ...event, finalMessage: cached.finalMessage, usage: cached.usage } : event
          void call(hook, spliced, sanitizeCtx(hook, ctx))
          return
        }
        if (gateTimeoutMs === undefined) {
          void call(hook, event, sanitizeCtx(hook, ctx))
          return
        }
        return call(hook, event, sanitizeCtx(hook, ctx), gateTimeoutMs).then((reply) => {
          // A reply without output is the daemon's legitimate "no decision";
          // only a shim timeout or a daemon-reported error may fail closed.
          if (reply?.timedOut && FAIL_CLOSED) {
            return failClosedResult(hook, event, "agenthooks: hook timed out (fail-closed)")
          }
          if (reply?.error && FAIL_CLOSED) {
            return failClosedResult(hook, event, "agenthooks: hook failed (fail-closed): " + reply.error)
          }
          return reply?.output
        })
      })
    }

    api.on("gateway_stop", () => {
      try {
        child.stdin.end()
        child.kill()
      } catch {}
    })
  },
}
`
