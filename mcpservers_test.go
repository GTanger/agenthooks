package agenthooks

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestCodexMCPServersReplaysLaunchOverrides: the whole reason this exists. A
// caller running `codex mcp list` itself reports the default config, which for
// a session launched with -c or --profile is not the set it reaches. Anything
// trusting that answer misjudges where a call routes.
func TestCodexMCPServersReplaysLaunchOverrides(t *testing.T) {
	home := isolateHome(t)
	project := t.TempDir()
	writeConfig(t, filepath.Join(home, ".codex", "config.toml"), `[mcp_servers.shared]
url = "https://disk.example.com/mcp"
`)
	fake := installFakeCodex(t, `[{"name":"shared","enabled":true,"transport":{"type":"streamable_http","url":"https://launch.example.com/mcp"}}]`)
	launch := parseCodexLaunchArgs([]string{"codex", "-c", `mcp_servers.shared.url="https://launch.example.com/mcp"`}, project)
	launch.Executable = ""
	r := mcpTestRunner(t)
	r.codexLaunchContext = &launch

	servers, ok := r.CodexMCPServers(project)
	if !ok {
		t.Fatal("CodexMCPServers reported no snapshot")
	}
	if len(servers) != 1 || servers[0].Name != "shared" {
		t.Fatalf("servers = %+v", servers)
	}
	if servers[0].URL != "https://launch.example.com/mcp" {
		t.Fatalf("URL = %q, want the launch override rather than the on-disk default", servers[0].URL)
	}

	calls := fake.calls(t)
	want := append(launch.replayArgs(), "mcp", "list", "--json")
	if len(calls) != 1 || strings.Join(calls[0].Args, "\x00") != strings.Join(want, "\x00") || calls[0].Dir != project {
		t.Fatalf("probe = %+v, want args %#v dir %q", calls, want, project)
	}
}

// TestCodexMCPServersReportsStdioCommand: a stdio server's identity is its
// launch command, not its config alias — an alias can be renamed to point
// anywhere.
func TestCodexMCPServersReportsStdioCommand(t *testing.T) {
	isolateHome(t)
	project := t.TempDir()
	installFakeCodex(t, `[{"name":"local","enabled":true,"transport":{"type":"stdio","command":"npx","args":["-y","some-server"]}}]`)
	launch := parseCodexLaunchArgs([]string{"codex"}, project)
	launch.Executable = ""
	r := mcpTestRunner(t)
	r.codexLaunchContext = &launch

	servers, ok := r.CodexMCPServers(project)
	if !ok || len(servers) != 1 {
		t.Fatalf("servers = %+v ok = %v", servers, ok)
	}
	if servers[0].Command != "npx -y some-server" || servers[0].URL != "" {
		t.Fatalf("stdio server = %+v", servers[0])
	}
}

// TestCodexMCPServersDistinguishesNoSnapshotFromNoServers: callers gate
// enforcement on this. "I could not look" must not read as "there is nothing
// there", so an unreadable list reports false while a genuinely empty config
// reports true with no entries.
func TestCodexMCPServersDistinguishesNoSnapshotFromNoServers(t *testing.T) {
	isolateHome(t)
	project := t.TempDir()
	installFakeCodex(t, `[]`)
	launch := parseCodexLaunchArgs([]string{"codex"}, project)
	launch.Executable = ""

	r := mcpTestRunner(t)
	r.codexLaunchContext = &launch
	servers, ok := r.CodexMCPServers(project)
	if !ok {
		t.Fatal("an empty but readable list must report a snapshot")
	}
	if len(servers) != 0 {
		t.Fatalf("servers = %+v, want none", servers)
	}

	// A launch this process cannot replay would probe a server set the session
	// never had, so it must report no snapshot rather than a misleading one.
	unreplayable := launch
	unreplayable.Unreplayable = true
	r2 := mcpTestRunner(t)
	r2.codexLaunchContext = &unreplayable
	if _, ok := r2.CodexMCPServers(project); ok {
		t.Fatal("an unreplayable launch must not report a snapshot")
	}

	// The fallback switch is honored.
	r3 := mcpTestRunner(t, WithoutMCPListFallback())
	r3.codexLaunchContext = &launch
	if _, ok := r3.CodexMCPServers(project); ok {
		t.Fatal("WithoutMCPListFallback must suppress the probe")
	}
}
