# OpenClaw fixture provenance

These frames were captured from **OpenClaw 2026.6.34** (npm global install,
Node 25.3.0, macOS) during the DNO-950 hook-payload spike, using an isolated
`--dev` profile (`~/.openclaw-dev`, gateway port 19001) and a plugin that
registered all 36 typed hooks and dumped every firing.

OpenClaw does not version its plugin API independently of OpenClaw itself, so
the supported range is whatever we have actually qualified — and that is pinned
by this corpus, not by anything in `codec_openclaw.go`. Record the version here
whenever the fixtures are re-recorded.

| File | Hook | Notes |
| --- | --- | --- |
| `session_start.json` | `session_start` | `sessionId` + `sessionKey`; `resumedFrom` on rotation |
| `before_agent_run.json` | `before_agent_run` | carries `event.prompt`; requires `allowConversationAccess` |
| `before_tool_call.json` | `before_tool_call` | `toolName`/`params`/`toolCallId`/`runId`; the blocking surface |
| `after_tool_call.json` | `after_tool_call` | adds `result` + `durationMs` |
| `after_tool_call_blocked.json` | `after_tool_call` | still fires for a call we blocked, with the block text as the result |
| `llm_output.json` | `llm_output` | `assistantTexts` + per-turn `usage`; requires `allowConversationAccess` |
| `agent_end.json` | `agent_end` | turn close: `success`/`error`/`durationMs`/`messages` |
| `session_end.json` | `session_end` | `reason` + `nextSessionId` |

`ctx.runId` is identical across `before_agent_run`, `before_tool_call`,
`llm_output` and `agent_end` for one turn. That stability is what Gram's
`canonicalAgentTurnID` relies on to correlate a prompt with its response — if a
future OpenClaw build breaks it, turn correlation degrades silently rather than
erroring, so check it explicitly when re-recording.

## Re-recording

Conversation-scope hooks (`before_agent_run`, `llm_input`, `llm_output`,
`agent_end`, `before_agent_finalize`) only fire with
`plugins.entries.<id>.hooks.allowConversationAccess: true`. Without it the
capture looks successful but silently omits half the corpus.

Coverage also depends on the model-auth mode: under the Claude CLI harness
(`agentRuntime: claude-cli`, which `models auth login` writes by default when a
claude-cli profile exists) the tool and LLM hooks never fire at all. Re-record
against the embedded runtime (`agentRuntime: { id: "openclaw" }`).

The customer-facing consequences of both are documented in the Gram repo at
`docs/runbooks/openclaw-install.md`.
