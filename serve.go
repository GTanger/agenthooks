package agenthooks

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"
)

// serve is the long-lived daemon mode behind the in-process-plugin shims
// (OpenCode §8, OpenClaw). Frames are processed sequentially, matching the
// providers' per-session hook semantics (open question #1 resolved
// conservatively). The shim owns the timeout policy the provider lacks; the
// daemon still bounds each handler with the resolved Policy deadline.
func (r *Runner) serve(ctx context.Context, inv *invocation, stdin io.Reader, stdout, stderr io.Writer) int {
	if inv.provider == "" {
		inv.provider = ProviderOpenCode
	}
	if inv.provider == ProviderOpenClaw {
		return r.serveOpenClaw(ctx, inv, stdin, stdout)
	}
	if inv.provider != ProviderOpenCode {
		_, _ = fmt.Fprintf(stderr, "agenthooks: serve mode supports --provider=opencode or --provider=openclaw, got %q\n", inv.provider)
		return 64
	}

	sc := bufio.NewScanner(stdin)
	sc.Buffer(make([]byte, 0, 64<<10), maxPayloadBytes)
	enc := json.NewEncoder(stdout)

	var serverInfo struct {
		ServerURL string `json:"serverUrl"`
		Directory string `json:"directory"`
		Worktree  string `json:"worktree"`
		MCP       []mcpConfigEntry
		MCPExact  bool
	}

	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var fr opencodeFrame
		if err := json.Unmarshal(line, &fr); err != nil {
			r.logger.Error("agenthooks: bad shim frame", "error", err)
			continue
		}
		// The shim's first runtime hook sends server info plus the resolved MCP
		// inventory; omitted MCP falls back to direct config reads.
		if fr.Hook == "initialize" {
			var info struct {
				ServerURL string                      `json:"serverUrl"`
				Directory string                      `json:"directory"`
				Worktree  string                      `json:"worktree"`
				MCP       *map[string]opencodeMCPJSON `json:"mcp"`
			}
			if json.Unmarshal(fr.Input, &info) == nil {
				serverInfo.ServerURL = info.ServerURL
				serverInfo.Directory = info.Directory
				serverInfo.Worktree = info.Worktree
				serverInfo.MCPExact = info.MCP != nil
				serverInfo.MCP = nil
				if info.MCP != nil {
					serverInfo.MCP = openCodeMCPEntries(*info.MCP)
				}
			}
			_ = enc.Encode(opencodeReply{Seq: fr.Seq})
			continue
		}

		lineCopy := make([]byte, len(line))
		copy(lineCopy, line)
		typed, err := decodeOpenCodeFrame(inv.variant, DetectionConfig, r.now(), &fr, lineCopy)
		if err != nil {
			r.logger.Error("agenthooks: decode failed", "hook", fr.Hook, "error", err)
			_ = enc.Encode(opencodeReply{Seq: fr.Seq})
			continue
		}
		base := eventOf(typed)
		if base.Session.CWD == "" {
			base.Session.CWD = serverInfo.Directory
			base.Session.WorkspaceRoots = rootsFor(serverInfo.Directory)
		}
		tool := toolOf(typed)
		reportInventory := shouldReportMCPInventory(base, tool)
		inventory, inventoryComplete := serverInfo.MCP, serverInfo.MCPExact
		if !inventoryComplete && (tool != nil || reportInventory) {
			inventory = loadMCPConfigEntries(ProviderOpenCode, base.Session.CWD)
		}
		if tool != nil {
			r.resolveMCPWithOpenCodeInventory(ctx, typed, &inventory)
			reportInventory = shouldReportMCPInventory(base, tool)
		}
		pol := r.policy(base)
		deadline := pol.Timeout
		if deadline == 0 {
			deadline = defaultDeadline
		}
		if reportInventory {
			inventoryCtx, inventoryCancel := context.WithTimeout(withLogger(ctx, r.logger), deadline)
			err := r.reportMCPInventorySnapshot(inventoryCtx, base, inventory, inventoryComplete)
			inventoryCancel()
			if err != nil {
				r.logger.Error("agenthooks: MCP inventory handler failed", "error", err)
			}
		}
		hctx, cancel := context.WithTimeout(withLogger(ctx, r.logger), deadline)
		core, herr := r.dispatch(hctx, typed)
		cancel()
		if herr != nil {
			r.logger.Error("agenthooks: handler failed", "hook", fr.Hook, "error", herr)
			core = failCore(pol, base)
		}
		core = r.applyPolicy(typed, base, core, pol)

		reply, encErr := encodeOpenCodeReply(typed, base, core)
		if encErr != nil {
			r.logger.Error("agenthooks: encode failed", "hook", fr.Hook, "error", encErr)
			reply = &opencodeReply{}
		}
		reply.Seq = fr.Seq
		if err := enc.Encode(reply); err != nil {
			r.logger.Error("agenthooks: writing reply", "error", err)
			return 1
		}
	}
	if err := sc.Err(); err != nil {
		r.logger.Error("agenthooks: reading shim stream", "error", err)
		return 1
	}
	return 0
}

// serveOpenClaw is the NDJSON loop behind the generated OpenClaw shim plugin.
// It differs from the OpenCode loop in wire semantics only: replies are hook
// return values rather than mutable-output merges, there is no initialize
// frame (every frame's ctx carries its own identity), and the per-connection
// state backfills workspaceDir/model onto tool-scope frames and flips the
// after_tool_call of a denied call to a failure (quirk #37).
//
// Gating frames (tool.pre, prompt.submitted) dispatch inline so their reply
// carries the decision; every other frame is acknowledged immediately and
// dispatched on a single background worker, so a slow telemetry handler
// cannot delay a queued gate. Observe frames keep their relative order on the
// worker; a gate may run before an earlier observe handler finishes.
func (r *Runner) serveOpenClaw(ctx context.Context, inv *invocation, stdin io.Reader, stdout io.Writer) int {
	sc := bufio.NewScanner(stdin)
	sc.Buffer(make([]byte, 0, 64<<10), maxPayloadBytes)
	enc := json.NewEncoder(stdout)
	st := newOpenclawServeState()

	deadlineFor := func(pol Policy) time.Duration {
		if pol.Timeout > 0 {
			return pol.Timeout
		}
		// The shim renders --timeout from its gate deadline; without it a
		// handler could keep burning long after the shim gave up.
		if inv.timeout > 0 {
			return inv.timeout * 9 / 10
		}
		return defaultDeadline
	}

	observe := make(chan any, 256)
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		for typed := range observe {
			base := eventOf(typed)
			pol := r.policy(base)
			hctx, cancel := context.WithTimeout(withLogger(ctx, r.logger), deadlineFor(pol))
			if _, err := r.dispatch(hctx, typed); err != nil {
				r.logger.Error("agenthooks: handler failed", "hook", base.NativeName, "error", err)
			}
			cancel()
		}
	}()
	defer workers.Wait()
	defer close(observe)

	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var fr openclawFrame
		if err := json.Unmarshal(line, &fr); err != nil {
			r.logger.Error("agenthooks: bad shim frame", "error", err)
			continue
		}
		// The shim reports a gate it had to fail-close locally (consumer
		// unreachable or over deadline) so the denied call's after_tool_call
		// still decodes as blocked (quirks #36, #37).
		if fr.Hook == "gate_timeout" {
			var in struct {
				ToolCallID string `json:"toolCallId"`
				Reason     string `json:"reason"`
			}
			_ = json.Unmarshal(fr.Event, &in)
			if in.ToolCallID != "" {
				reason := in.Reason
				if reason == "" {
					reason = "agenthooks: hook timed out (fail-closed)"
				}
				st.blockedCalls[in.ToolCallID] = reason
			}
			_ = enc.Encode(openclawReply{Seq: fr.Seq})
			continue
		}
		lineCopy := make([]byte, len(line))
		copy(lineCopy, line)
		typed, err := decodeOpenClawFrame(inv.variant, DetectionConfig, r.now(), &fr, lineCopy, st)
		if err != nil {
			r.logger.Error("agenthooks: decode failed", "hook", fr.Hook, "error", err)
			_ = enc.Encode(openclawReply{Seq: fr.Seq})
			continue
		}
		base := eventOf(typed)

		if base.Kind != KindToolPre && base.Kind != KindPromptSubmitted {
			if err := enc.Encode(openclawReply{Seq: fr.Seq}); err != nil {
				r.logger.Error("agenthooks: writing reply", "error", err)
				return 1
			}
			observe <- typed
			continue
		}

		pol := r.policy(base)
		hctx, cancel := context.WithTimeout(withLogger(ctx, r.logger), deadlineFor(pol))
		core, herr := r.dispatch(hctx, typed)
		cancel()
		if herr != nil {
			r.logger.Error("agenthooks: handler failed", "hook", fr.Hook, "error", herr)
			core = failCore(pol, base)
		}
		core = r.applyPolicy(typed, base, core, pol)

		toolCallID := ""
		if tool := toolOf(typed); tool != nil && !tool.Synthesized {
			toolCallID = tool.ID
		}
		reply, encErr := encodeOpenClawReply(base, core, st, toolCallID)
		if encErr != nil {
			r.logger.Error("agenthooks: encode failed", "hook", fr.Hook, "error", encErr)
			reply = &openclawReply{}
		}
		reply.Seq = fr.Seq
		if err := enc.Encode(reply); err != nil {
			r.logger.Error("agenthooks: writing reply", "error", err)
			return 1
		}
	}
	if err := sc.Err(); err != nil {
		r.logger.Error("agenthooks: reading shim stream", "error", err)
		return 1
	}
	return 0
}
