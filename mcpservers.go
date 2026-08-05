package agenthooks

// MCPServer is one entry of a provider's effective MCP server list: the servers
// a session will actually reach, after every config layer and launch override
// has been applied.
type MCPServer struct {
	// Name is the server's configured name.
	Name string
	// URL is set for remote (HTTP/SSE) servers.
	URL string
	// Command is set for stdio servers: the launch command with its args
	// space-joined. It identifies where a call really routes, which a
	// renameable config alias does not.
	Command string
}

// CodexMCPServers returns the MCP servers the Codex session running at cwd will
// actually reach.
//
// Callers must not shell out to `codex mcp list` themselves. The effective
// server set depends on how the session was launched — `--profile` selects a
// different set, `-c mcp_servers.<name>.url=…` retargets one — and a plain
// invocation reports the default config instead. Both directions mislead
// anything that trusts the result: a profile-only server looks absent, and an
// overridden server reports the target it replaced. This resolves the launch
// context from the session's own process ancestry and replays those flags, the
// same way MCP tool-call resolution does, and shares its cache so a session
// pays for at most one probe.
//
// The bool reports whether a snapshot was obtained at all, and callers must
// distinguish it from an empty list: false means the list could not be read —
// no codex binary, a failed probe, a launch this process cannot replay — while
// true with no entries means the session genuinely has no MCP servers. Treating
// the two alike turns "I could not look" into "there is nothing there".
func (r *Runner) CodexMCPServers(cwd string) ([]MCPServer, bool) {
	if r.mcpListOff {
		return nil, false
	}
	launch, found := r.currentCodexLaunchContext(cwd)
	// An unreplayable launch is worse than no answer: the probe would report a
	// server set the session never had.
	if !found || launch.Unreplayable {
		return nil, false
	}
	entries, snapshot := r.codexMCPListEntries(launch)
	if !snapshot {
		return nil, false
	}
	servers := make([]MCPServer, 0, len(entries))
	for _, entry := range entries {
		servers = append(servers, MCPServer{
			Name:    entry.Name,
			URL:     entry.URL,
			Command: entry.Command,
		})
	}
	return servers, true
}
