package agenthooks

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/speakeasy-api/agenthooks/internal/ipc"
)

// The in-process client/server suite: servers run as goroutines via
// Runner.Run (the same entry the argv mode uses) and clients connect through
// the real socket/pipe transport in internal/ipc. Auto-spawn is exercised
// through the Runner's spawn seam; the true detached re-exec is covered by
// the subprocess test in clientserver_e2e_test.go.

const denyWire = `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"server says no"}}`

// testIdentity isolates one test's rendezvous: a per-test state dir (unix
// sockets and locks) plus unique pre-sentinel args (which hash into the
// endpoint name, so Windows pipes are unique too).
func testIdentity(t *testing.T) []string {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	return []string{"--config=" + filepath.Join(t.TempDir(), "cfg.json")}
}

func serverRunArgs(preArgs []string, idle string) []string {
	args := append(append([]string(nil), preArgs...), "agenthooks", "server")
	if idle != "" {
		args = append(args, "--idle-timeout="+idle)
	}
	return args
}

func clientRunArgs(preArgs []string, extra ...string) []string {
	args := append(append([]string(nil), preArgs...), "agenthooks", "client")
	return append(args, extra...)
}

// denyServerRunner is a hermetic server-side Runner that denies tool.pre.
func denyServerRunner(t *testing.T) *Runner {
	t.Helper()
	r := quietRunner(WithDedupDir(t.TempDir()), WithoutMCPResolution(), WithoutBackfill())
	r.OnToolPre(func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
		return Deny("server says no"), nil
	})
	return r
}

// startServer runs the server mode in a goroutine and blocks until it
// accepts connections. The returned channel yields the exit code.
func startServer(t *testing.T, r *Runner, args []string) chan int {
	t.Helper()
	exit := make(chan int, 1)
	go func() {
		exit <- r.Run(context.Background(), args, strings.NewReader(""), io.Discard, io.Discard)
	}()
	waitForServer(t, args)
	return exit
}

// waitForServer polls the endpoint derived from args' pre-sentinel flags
// until something accepts.
func waitForServer(t *testing.T, args []string) {
	t.Helper()
	inv, err := parseArgs(args)
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	addr, err := ipc.Resolve(exe, inv.preArgs)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		conn, err := ipc.Dial(addr.Endpoint, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never came up on %s: %v", addr.Endpoint, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitExit(t *testing.T, exit chan int, what string) int {
	t.Helper()
	select {
	case code := <-exit:
		return code
	case <-time.After(15 * time.Second):
		t.Fatalf("%s never exited", what)
		return -1
	}
}

// noSpawn disables auto-spawn so a client test fails fast instead of
// re-execing the test binary.
func noSpawn(r *Runner) {
	r.spawnServer = func([]string) error { return errors.New("spawning disabled in this test") }
}

func TestClientServerGatingRoundTrip(t *testing.T) {
	preArgs := testIdentity(t)
	exit := startServer(t, denyServerRunner(t), serverRunArgs(preArgs, "5s"))

	// The client's own handler would allow: a deny response proves the
	// decision came over the wire from the server, not from this process.
	client := quietRunner()
	noSpawn(client)
	client.OnToolPre(func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
		return Allow(), nil
	})
	for i := 0; i < 2; i++ { // second request reuses the same server
		out, code := runWith(t, client, clientRunArgs(preArgs, "--provider=claude-code"), fixture(t, "claude/pre_tool_use.json"))
		if out != denyWire || code != 0 {
			t.Fatalf("request %d: got %q (exit %d), want the server's deny", i, out, code)
		}
	}
	if code := waitExit(t, exit, "idle server"); code != 0 {
		t.Errorf("server exit = %d, want 0", code)
	}
}

func TestClientRelaysExitCodeAndStderr(t *testing.T) {
	preArgs := testIdentity(t)
	// Kimi's prompt-blocking mechanism is exit 2 with the reason on stderr
	// (quirk #23): the response frame must carry all three channels back.
	server := quietRunner(WithDedupDir(t.TempDir()), WithoutMCPResolution(), WithoutBackfill())
	server.OnPromptSubmitted(func(ctx context.Context, e *PromptEvent) (PromptDecision, error) {
		return BlockPrompt("kimi block"), nil
	})
	exit := startServer(t, server, serverRunArgs(preArgs, "5s"))

	client := quietRunner()
	noSpawn(client)
	var out, errb bytes.Buffer
	code := client.Run(context.Background(), clientRunArgs(preArgs, "--provider=kimi-code"),
		bytes.NewReader(kimiPrompt("sess-cs-kimi")), &out, &errb)
	if code != 2 {
		t.Errorf("exit = %d, want kimi's blocking exit 2 (stderr %q)", code, errb.String())
	}
	if !strings.Contains(errb.String(), "kimi block") {
		t.Errorf("stderr must carry the reason: %q", errb.String())
	}
	waitExit(t, exit, "idle server")
}

func TestClientSpawnsServerOnDemand(t *testing.T) {
	preArgs := testIdentity(t)
	server := denyServerRunner(t)
	exit := make(chan int, 1)

	var spawns atomic.Int32
	client := quietRunner()
	client.OnToolPre(func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
		return Allow(), nil
	})
	client.spawnServer = func(gotPre []string) error {
		spawns.Add(1)
		if len(gotPre) != len(preArgs) || gotPre[0] != preArgs[0] {
			t.Errorf("spawn must preserve pre-sentinel flags: %v", gotPre)
		}
		go func() {
			exit <- server.Run(context.Background(), serverRunArgs(gotPre, "5s"), strings.NewReader(""), io.Discard, io.Discard)
		}()
		return nil
	}

	out, code := runWith(t, client, clientRunArgs(preArgs, "--provider=claude-code"), fixture(t, "claude/pre_tool_use.json"))
	if out != denyWire || code != 0 {
		t.Fatalf("got %q (exit %d), want the spawned server's deny", out, code)
	}
	if got := spawns.Load(); got != 1 {
		t.Errorf("spawns = %d, want 1", got)
	}
	waitExit(t, exit, "idle server")
}

func TestClientSpawnRaceStartsOneServer(t *testing.T) {
	preArgs := testIdentity(t)
	server := denyServerRunner(t)
	exit := make(chan int, 1)

	var spawns atomic.Int32
	spawn := func(gotPre []string) error {
		if spawns.Add(1) > 1 {
			return errors.New("second spawn attempted; the spawn lock failed")
		}
		go func() {
			exit <- server.Run(context.Background(), serverRunArgs(gotPre, "5s"), strings.NewReader(""), io.Discard, io.Discard)
		}()
		return nil
	}

	const clients = 4
	var wg sync.WaitGroup
	results := make([]string, clients)
	codes := make([]int, clients)
	for i := 0; i < clients; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c := quietRunner()
			c.spawnServer = spawn
			var out, errb bytes.Buffer
			codes[i] = c.Run(context.Background(), clientRunArgs(preArgs, "--provider=claude-code"),
				bytes.NewReader(fixture(t, "claude/pre_tool_use.json")), &out, &errb)
			results[i] = out.String()
		}(i)
	}
	wg.Wait()

	for i := range results {
		if results[i] != denyWire || codes[i] != 0 {
			t.Errorf("client %d: got %q (exit %d), want the server's deny", i, results[i], codes[i])
		}
	}
	// The spawn lock admits one spawner; racing losers reconnect instead.
	if got := spawns.Load(); got != 1 {
		t.Errorf("spawns = %d, want 1 (spawn lock must serialize the herd)", got)
	}
	waitExit(t, exit, "idle server")
}

func TestClientFailsOpenWhenServerUnavailable(t *testing.T) {
	preArgs := testIdentity(t)
	client := quietRunner(WithDedupDir(t.TempDir()), WithoutMCPResolution(), WithoutBackfill())
	noSpawn(client)
	// A gating deny handler that must never run: in client mode the server
	// is a hard dependency, and an unreachable server means fail open —
	// never a silent in-process run of the pipeline.
	client.OnToolPre(func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
		t.Error("client mode must never run the pipeline in-process")
		return Deny("must not run"), nil
	})

	start := time.Now()
	var out, errb bytes.Buffer
	code := client.Run(context.Background(), clientRunArgs(preArgs, "--provider=claude-code"),
		bytes.NewReader(fixture(t, "claude/pre_tool_use.json")), &out, &errb)
	if code != 0 || out.Len() != 0 || errb.Len() != 0 {
		t.Fatalf("unreachable server must fail open (exit 0, no output): stdout %q, stderr %q (exit %d)",
			out.String(), errb.String(), code)
	}
	// The fail-open must be prompt: a failed spawn errors the seam
	// immediately, so at most the connect/spawn retry budget (plus slack)
	// precedes the exit.
	if elapsed := time.Since(start); elapsed > clientSpawnBudget+3*time.Second {
		t.Errorf("fail-open took %s, want under the spawn budget plus slack", elapsed)
	}
}

func TestServerEarlyAcksNonGatingEvents(t *testing.T) {
	preArgs := testIdentity(t)
	handlerDone := make(chan struct{})
	release := make(chan struct{})
	server := quietRunner(WithDedupDir(t.TempDir()), WithoutMCPResolution(), WithoutBackfill())
	server.OnNotification(func(ctx context.Context, e *NotificationEvent) error {
		<-release
		close(handlerDone)
		return nil
	})
	exit := startServer(t, server, serverRunArgs(preArgs, "5s"))

	client := quietRunner()
	noSpawn(client)
	out, code := runWith(t, client, clientRunArgs(preArgs, "--provider=claude-code"), fixture(t, "claude/notification.json"))
	if code != 0 || out != "{}" {
		t.Fatalf("early-ack must return the provider no-op: %q (exit %d)", out, code)
	}
	select {
	case <-handlerDone:
		t.Fatalf("handler finished before the ack returned — not early-acked")
	default:
	}
	// The handler is still parked: the client got its answer first.
	close(release)
	select {
	case <-handlerDone:
	case <-time.After(10 * time.Second):
		t.Fatalf("handler never completed after the ack")
	}
	waitExit(t, exit, "idle server")
}

func TestServerGatingEventsWaitForHandlers(t *testing.T) {
	preArgs := testIdentity(t)
	exit := startServer(t, denyServerRunner(t), serverRunArgs(preArgs, "5s"))

	client := quietRunner()
	noSpawn(client)
	// tool.pre is gating: the response must be the handler's decision, not
	// an early no-op.
	out, _ := runWith(t, client, clientRunArgs(preArgs, "--provider=claude-code"), fixture(t, "claude/pre_tool_use.json"))
	if out != denyWire {
		t.Errorf("gating event must carry the decision: %q", out)
	}
	waitExit(t, exit, "idle server")
}

func TestServerIdleShutdown(t *testing.T) {
	preArgs := testIdentity(t)
	start := time.Now()
	exit := startServer(t, denyServerRunner(t), serverRunArgs(preArgs, "300ms"))
	if code := waitExit(t, exit, "idle server"); code != 0 {
		t.Errorf("idle shutdown exit = %d, want 0", code)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("idle shutdown took %s", elapsed)
	}
}

func TestServerSingleton(t *testing.T) {
	preArgs := testIdentity(t)
	exit := startServer(t, denyServerRunner(t), serverRunArgs(preArgs, "5s"))

	// A second server for the same identity yields immediately with 0.
	second := quietRunner()
	code := second.Run(context.Background(), serverRunArgs(preArgs, "5s"), strings.NewReader(""), io.Discard, io.Discard)
	if code != 0 {
		t.Errorf("second server exit = %d, want 0 (already running)", code)
	}
	waitExit(t, exit, "idle server")
}

func TestServerVersionMismatchDrains(t *testing.T) {
	preArgs := testIdentity(t)
	// Idle long enough that only the mismatch can explain the exit.
	exit := startServer(t, denyServerRunner(t), serverRunArgs(preArgs, "2m"))

	resp := rawRequest(t, preArgs, ipc.Request{
		V:     ipc.ProtocolVersion,
		Build: "different-build-stamp",
		Argv:  clientRunArgs(preArgs, "--provider=claude-code"),
		Stdin: fixture(t, "claude/pre_tool_use.json"),
	})
	if resp.Error != "" || string(resp.Stdout) != denyWire {
		t.Fatalf("the mismatched request must still be served: %+v", resp)
	}
	if code := waitExit(t, exit, "draining server"); code != 0 {
		t.Errorf("upgrade drain exit = %d, want 0", code)
	}
}

func TestServerRejectsProtocolMismatch(t *testing.T) {
	preArgs := testIdentity(t)
	exit := startServer(t, denyServerRunner(t), serverRunArgs(preArgs, "5s"))

	resp := rawRequest(t, preArgs, ipc.Request{
		V:     99,
		Argv:  clientRunArgs(preArgs, "--provider=claude-code"),
		Stdin: fixture(t, "claude/pre_tool_use.json"),
	})
	if resp.Error == "" {
		t.Errorf("unknown protocol version must produce an error frame: %+v", resp)
	}
	waitExit(t, exit, "idle server")
}

func TestServerReportsBadArgv(t *testing.T) {
	preArgs := testIdentity(t)
	exit := startServer(t, denyServerRunner(t), serverRunArgs(preArgs, "5s"))

	resp := rawRequest(t, preArgs, ipc.Request{
		V:    ipc.ProtocolVersion,
		Argv: clientRunArgs(preArgs, "--provider=claude-code", "--timeout=bogus"),
	})
	if resp.ExitCode != 64 || !strings.Contains(string(resp.Stderr), "--timeout") {
		t.Errorf("bad argv must round-trip as exit 64 + stderr: %+v", resp)
	}
	waitExit(t, exit, "idle server")
}

func TestServerFlushesTelemetryOnShutdown(t *testing.T) {
	preArgs := testIdentity(t)
	rec := &captureRecorder{}
	server := quietRunner(WithDedupDir(t.TempDir()), WithoutMCPResolution(), WithoutBackfill(), WithTelemetry(rec))
	server.OnToolPre(func(ctx context.Context, e *ToolPreEvent) (ToolPreDecision, error) {
		return Deny("server says no"), nil
	})
	exit := startServer(t, server, serverRunArgs(preArgs, "500ms"))

	client := quietRunner()
	noSpawn(client)
	if out, code := runWith(t, client, clientRunArgs(preArgs, "--provider=claude-code"), fixture(t, "claude/pre_tool_use.json")); code != 0 || out != denyWire {
		t.Fatalf("request failed: %q (exit %d)", out, code)
	}
	waitExit(t, exit, "idle server")
	if rec.records.Load() == 0 {
		t.Errorf("server-side events must reach the recorder")
	}
	if !rec.shutdown.Load() {
		t.Errorf("idle shutdown must flush the recorder via Shutdown")
	}
}

// rawRequest opens one connection to the test server and performs a framed
// exchange, bypassing clientMain (for protocol-level assertions).
func rawRequest(t *testing.T, preArgs []string, req ipc.Request) ipc.Response {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	addr, err := ipc.Resolve(exe, preArgs)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := ipc.Dial(addr.Endpoint, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if err := ipc.WriteFrame(conn, req); err != nil {
		t.Fatal(err)
	}
	var resp ipc.Response
	if err := ipc.ReadFrame(conn, &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}
