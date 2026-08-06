package agenthooks

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"github.com/speakeasy-api/agenthooks/internal/filelock"
	"github.com/speakeasy-api/agenthooks/internal/ipc"
)

// The `agenthooks client` mode: the lightweight per-hook process that
// generated configs install in place of `run`. It reads the payload,
// forwards the invocation to the long-running hook server over the
// consumer-identity socket (spawning the server first if none answers), and
// relays the server's stdout/stderr/exit code back to the provider.
//
// The server is an optimization, never a dependency: any failure — no
// server, spawn blocked, connect timeout, protocol mismatch, truncated
// response — degrades to running the exact same pipeline in-process, which
// is byte-for-byte today's `run` behavior. Decisions never wait on server
// health; only the warm caches and in-process telemetry do.

const (
	// clientDialTimeout bounds one connection attempt.
	clientDialTimeout = 250 * time.Millisecond
	// clientSpawnBudget bounds the whole connect-spawn-reconnect dance
	// before the client gives up and runs in-process.
	clientSpawnBudget = 2 * time.Second
	// clientResponseSlack rides on top of the hook deadline when waiting
	// for the server's response.
	clientResponseSlack = 5 * time.Second
)

// forwardedEnv is the allowlist of environment variables a client snapshots
// into the request: provider detection signals (detect.go) and the trace
// context. Deeper best-effort quirk paths (MCP config discovery, launch
// probes, transcript paths) read the server's own environment, which it
// inherited from the client that spawned it.
var forwardedEnv = []string{
	"TRACEPARENT",
	"CURSOR_VERSION", "CURSOR_TRACE_ID", "CURSOR_AGENT", "CURSOR_TRANSCRIPT_PATH",
	"CODEX_HOME", "CODEX_SANDBOX",
	"GEMINI_CWD", "GEMINI_CLI",
	"OPENCODE_SERVER", "OPENCODE",
	"CLAUDE_PROJECT_DIR", "CLAUDE_PLUGIN_ROOT", "CLAUDE_CODE_REMOTE", "CLAUDE_PID",
	"KIMI_CODE_HOME",
}

// clientMain implements the `agenthooks client` argv mode.
func (r *Runner) clientMain(ctx context.Context, inv *invocation, stdin io.Reader, stdout, stderr io.Writer) int {
	payload := r.readPayload(inv, stdin)
	resp, err := r.callServer(inv, payload)
	if err == nil {
		if len(resp.Stdout) > 0 {
			_, _ = stdout.Write(resp.Stdout)
		}
		if len(resp.Stderr) > 0 {
			_, _ = stderr.Write(resp.Stderr)
		}
		return resp.ExitCode
	}
	r.logger.Warn("agenthooks: hook server unavailable; running in-process", "error", err)
	return r.runEvent(ctx, inv, payload, runOpts{getenv: os.Getenv}, stdout, stderr)
}

// callServer performs one framed request/response exchange, spawning the
// server if nothing answers on the endpoint.
func (r *Runner) callServer(inv *invocation, payload []byte) (*ipc.Response, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolving executable: %w", err)
	}
	addr, err := ipc.Resolve(exe, inv.preArgs)
	if err != nil {
		return nil, err
	}
	conn, err := r.connectOrSpawn(addr, inv.preArgs)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()

	env := make(map[string]string, len(forwardedEnv))
	for _, key := range forwardedEnv {
		if v := os.Getenv(key); v != "" {
			env[key] = v
		}
	}
	cwd, _ := os.Getwd()
	req := ipc.Request{
		V:     ipc.ProtocolVersion,
		Build: ipc.BuildStamp(exe),
		Argv:  inv.raw,
		Stdin: payload,
		Env:   env,
		CWD:   cwd,
	}

	// The response can legitimately take as long as the hook deadline (the
	// server runs the same policy timeouts run mode would); past that plus
	// slack, falling back in-process could still answer before the provider
	// gives up on us.
	wait := defaultDeadline
	if inv.timeout > 0 {
		wait = inv.timeout
	}
	_ = conn.SetDeadline(time.Now().Add(wait + clientResponseSlack))
	if err := ipc.WriteFrame(conn, req); err != nil {
		return nil, err
	}
	var resp ipc.Response
	if err := ipc.ReadFrame(conn, &resp); err != nil {
		return nil, err
	}
	if resp.Error != "" {
		return nil, fmt.Errorf("server error: %s", resp.Error)
	}
	return &resp, nil
}

// connectOrSpawn dials the endpoint, auto-spawning the server on a miss. A
// file lock serializes the spawn so a burst of hook invocations starts one
// server; losers of the lock race just keep re-dialing while the winner's
// server comes up.
func (r *Runner) connectOrSpawn(addr ipc.Address, preArgs []string) (net.Conn, error) {
	conn, err := ipc.Dial(addr.Endpoint, clientDialTimeout)
	if err == nil {
		return conn, nil
	}

	release, locked, lockErr := filelock.TryLock(addr.SpawnLock)
	if lockErr == nil && locked {
		// Hold the lock through the reconnect loop: as long as this client
		// is still waiting for its spawn to bind, nobody else spawns.
		defer release()
		if r.spawnServer == nil {
			return nil, fmt.Errorf("dialing hook server: %w (spawning disabled)", err)
		}
		if spawnErr := r.spawnServer(preArgs); spawnErr != nil {
			return nil, fmt.Errorf("spawning hook server: %w", spawnErr)
		}
	}

	deadline := time.Now().Add(clientSpawnBudget)
	backoff := 20 * time.Millisecond
	for {
		time.Sleep(backoff)
		backoff = min(backoff*2, 250*time.Millisecond)
		conn, err = ipc.Dial(addr.Endpoint, clientDialTimeout)
		if err == nil {
			return conn, nil
		}
		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("dialing hook server (after spawn window): %w", err)
		}
	}
}

// spawnServerDetached re-execs this binary as the detached hook server,
// preserving the consumer flags that define the server identity
// ("mybinary --config=x agenthooks server"). It is the default behind
// Runner.spawnServer; tests substitute in-process spawns.
func spawnServerDetached(preArgs []string) error {
	args := make([]string, 0, len(preArgs)+2)
	args = append(args, preArgs...)
	args = append(args, "agenthooks", "server")
	return startDetachedSelf(args, nil)
}
