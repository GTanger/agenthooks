package ipc

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	in := Request{
		V:     ProtocolVersion,
		Build: "abcdef0123456789",
		Argv:  []string{"--config=/x", "agenthooks", "client", "--provider=claude-code"},
		Stdin: []byte(`{"hook_event_name":"PreToolUse"}`),
		Env:   map[string]string{"TRACEPARENT": "00-11-22-01"},
		CWD:   "/work",
	}
	if err := WriteFrame(&buf, in); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	var out Request
	if err := ReadFrame(&buf, &out); err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if out.V != in.V || out.Build != in.Build || out.CWD != in.CWD ||
		!bytes.Equal(out.Stdin, in.Stdin) || len(out.Argv) != len(in.Argv) ||
		out.Env["TRACEPARENT"] != "00-11-22-01" {
		t.Errorf("round trip mangled the frame: %+v", out)
	}
	if buf.Len() != 0 {
		t.Errorf("frame left %d trailing bytes", buf.Len())
	}
}

func TestFrameSequenceOnOneConnection(t *testing.T) {
	// One request then one response over the same buffer, like a
	// connection carries them.
	var buf bytes.Buffer
	if err := WriteFrame(&buf, Request{V: 1}); err != nil {
		t.Fatal(err)
	}
	if err := WriteFrame(&buf, Response{V: 1, Stdout: []byte("{}"), ExitCode: 2}); err != nil {
		t.Fatal(err)
	}
	var req Request
	if err := ReadFrame(&buf, &req); err != nil {
		t.Fatal(err)
	}
	var resp Response
	if err := ReadFrame(&buf, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.ExitCode != 2 || string(resp.Stdout) != "{}" {
		t.Errorf("second frame wrong: %+v", resp)
	}
}

func TestReadFrameRejectsOversizedLength(t *testing.T) {
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], MaxFrameBytes+1)
	var out Request
	err := ReadFrame(bytes.NewReader(prefix[:]), &out)
	if err == nil || !strings.Contains(err.Error(), "cap") {
		t.Errorf("oversized length prefix must be rejected before allocation: %v", err)
	}
}

func TestReadFrameTruncatedBody(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, Request{V: 1}); err != nil {
		t.Fatal(err)
	}
	trunc := buf.Bytes()[:buf.Len()-2]
	var out Request
	if err := ReadFrame(bytes.NewReader(trunc), &out); err == nil {
		t.Errorf("truncated frame must error")
	}
}

func TestIdentity(t *testing.T) {
	base := Identity("/usr/local/bin/speakeasy-hooks", []string{"--config=/a.json"})
	if len(base) != 16 {
		t.Fatalf("identity length = %d, want 16", len(base))
	}
	if got := Identity("/usr/local/bin/speakeasy-hooks", []string{"--config=/a.json"}); got != base {
		t.Errorf("identity must be deterministic: %s vs %s", got, base)
	}
	if got := Identity("/usr/local/bin/speakeasy-hooks", []string{"--config=/b.json"}); got == base {
		t.Errorf("distinct configs must get distinct identities")
	}
	if got := Identity("/other/binary", []string{"--config=/a.json"}); got == base {
		t.Errorf("distinct binaries must get distinct identities")
	}
	// The separator must keep the encoding injective across arg boundaries.
	if Identity("/bin/x", []string{"ab", "c"}) == Identity("/bin/x", []string{"a", "bc"}) {
		t.Errorf("arg boundaries must participate in the identity")
	}
}

func TestBuildStamp(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "bin")
	if err := os.WriteFile(exe, []byte("v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	a := BuildStamp(exe)
	if a == "" {
		t.Fatalf("stamp empty for existing file")
	}
	if b := BuildStamp(exe); b != a {
		t.Errorf("stamp must be stable for an unchanged file")
	}
	// Replacing the binary (new size or mtime) changes the stamp.
	if err := os.WriteFile(exe, []byte("v2-bigger"), 0o755); err != nil {
		t.Fatal(err)
	}
	if b := BuildStamp(exe); b == a {
		t.Errorf("stamp must change when the executable is replaced")
	}
	if got := BuildStamp(filepath.Join(dir, "missing")); got != "" {
		t.Errorf("missing file must stamp empty, got %q", got)
	}
}

func TestResolveDerivesEndpointAndLocks(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Setenv("XDG_STATE_HOME", t.TempDir())
	}
	addr, err := Resolve("/usr/local/bin/myhooks", []string{"--config=/a.json"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if addr.ID == "" || addr.Endpoint == "" || addr.ServerLock == "" || addr.SpawnLock == "" {
		t.Fatalf("incomplete address: %+v", addr)
	}
	if !strings.Contains(addr.Endpoint, addr.ID) {
		t.Errorf("endpoint must embed the identity: %+v", addr)
	}
	if runtime.GOOS == "windows" {
		if !strings.HasPrefix(addr.Endpoint, `\\.\pipe\agenthooks-`) {
			t.Errorf("windows endpoint must be a named pipe: %s", addr.Endpoint)
		}
	} else {
		if !strings.HasSuffix(addr.Endpoint, ".sock") || len(addr.Endpoint) > maxSocketPath {
			t.Errorf("unix endpoint must be a bounded socket path: %s (%d bytes)", addr.Endpoint, len(addr.Endpoint))
		}
		if fi, err := os.Stat(filepath.Dir(addr.ServerLock)); err != nil || fi.Mode().Perm() != 0o700 {
			t.Errorf("state dir must exist with 0700: %v %v", fi, err)
		}
	}
	if addr.ServerLock == addr.SpawnLock {
		t.Errorf("server and spawn locks must differ: %+v", addr)
	}

	other, err := Resolve("/usr/local/bin/myhooks", []string{"--config=/b.json"})
	if err != nil {
		t.Fatal(err)
	}
	if other.Endpoint == addr.Endpoint {
		t.Errorf("distinct configs must rendezvous on distinct endpoints")
	}
}

func TestSocketPathLengthFallsBackToTempDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("named pipes have no path-length constraint")
	}
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), strings.Repeat("deep", 30)))
	addr, err := Resolve("/usr/local/bin/myhooks", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(addr.Endpoint) > maxSocketPath {
		t.Errorf("endpoint exceeds sun_path budget: %s (%d bytes)", addr.Endpoint, len(addr.Endpoint))
	}
}

func TestListenDialRoundTrip(t *testing.T) {
	addr := testAddress(t)
	ln, err := Listen(addr.Endpoint)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	done := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			done <- err
			return
		}
		defer func() { _ = conn.Close() }()
		var req Request
		if err := ReadFrame(conn, &req); err != nil {
			done <- err
			return
		}
		done <- WriteFrame(conn, Response{V: 1, Stdout: []byte("ok"), ExitCode: 0})
	}()

	conn, err := Dial(addr.Endpoint, time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if err := WriteFrame(conn, Request{V: 1}); err != nil {
		t.Fatal(err)
	}
	var resp Response
	if err := ReadFrame(conn, &resp); err != nil {
		t.Fatal(err)
	}
	if string(resp.Stdout) != "ok" {
		t.Errorf("response = %+v", resp)
	}
	if err := <-done; err != nil {
		t.Fatalf("server side: %v", err)
	}
}

func TestListenDetectsLiveServer(t *testing.T) {
	addr := testAddress(t)
	ln, err := Listen(addr.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	// Keep the listener accepting so the probe's dial succeeds.
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	if _, err := Listen(addr.Endpoint); !errors.Is(err, ErrAlreadyRunning) {
		t.Errorf("second Listen = %v, want ErrAlreadyRunning", err)
	}
}

func TestListenSweepsStaleSocket(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pipe instances die with their process; no stale files on windows")
	}
	addr := testAddress(t)
	ln, err := Listen(addr.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a crash: the socket file stays behind with nobody listening.
	if ul, ok := ln.(interface{ SetUnlinkOnClose(bool) }); ok {
		ul.SetUnlinkOnClose(false)
	}
	_ = ln.Close()
	if _, err := os.Stat(addr.Endpoint); err != nil {
		t.Skipf("platform unlinked the socket on close: %v", err)
	}

	ln2, err := Listen(addr.Endpoint)
	if err != nil {
		t.Fatalf("Listen must sweep a stale socket file: %v", err)
	}
	_ = ln2.Close()
}

// testAddress resolves an Address rooted in a per-test state dir (unix) or
// with a unique identity (windows, where pipes are process-scoped anyway).
func testAddress(t *testing.T) Address {
	t.Helper()
	if runtime.GOOS != "windows" {
		t.Setenv("XDG_STATE_HOME", t.TempDir())
	}
	addr, err := Resolve("/test/bin/agenthooks", []string{"--test-id=" + t.Name(), "--nonce=" + t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return addr
}
