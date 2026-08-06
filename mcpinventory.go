package agenthooks

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func (r *Runner) reportMCPInventory(ctx context.Context, base *Event) error {
	if r.mcpResolveOff || len(r.hMCPInventory) == 0 || base.Session.ID == "" {
		return nil
	}

	entries, available := r.effectiveMCPInventory(base)
	if !available {
		return fmt.Errorf("resolve MCP inventory: no authoritative snapshot")
	}
	dir := filepath.Join(r.stateDir(), "agenthooks-mcp-reported")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create MCP inventory report directory: %w", err)
	}
	cleanupMCPListCache(dir, time.Now())
	keyParts := make([]string, 0, len(entries))
	for _, entry := range entries {
		keyParts = append(keyParts, entry.Name+"\x00"+entry.URL+"\x00"+entry.Command)
	}
	sort.Strings(keyParts)
	sum := sha256.Sum256([]byte(string(base.Provider) + "\x00" + base.Session.ID + "\x00" + strings.Join(keyParts, "\x00")))
	path := filepath.Join(dir, hex.EncodeToString(sum[:])[:32])

	deadline := time.Now().Add(mcpListWaitTimeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		unlock, ok, err := tryMCPListLock(path + ".lock")
		if err != nil {
			return fmt.Errorf("lock MCP inventory report: %w", err)
		}
		if !ok {
			if !time.Now().Before(deadline) {
				return fmt.Errorf("wait for MCP inventory report: %w", context.DeadlineExceeded)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(25 * time.Millisecond):
			}
			continue
		}

		if _, err := os.Stat(path); err == nil {
			unlock()
			return nil
		}
		servers := make([]MCPServer, 0, len(entries))
		for _, entry := range entries {
			servers = append(servers, MCPServer{Name: entry.Name, URL: entry.URL, Command: entry.Command})
		}
		event := &MCPInventoryEvent{
			Event: Event{
				Provider:            base.Provider,
				Variant:             base.Variant,
				NativeName:          "MCPInventory",
				Kind:                KindMCPInventory,
				Time:                r.now(),
				Session:             base.Session,
				Agent:               base.Agent,
				DetectionConfidence: base.DetectionConfidence,
				Backfilled:          true,
				Raw:                 nil,
			},
			Servers: servers,
		}
		_, dispatchErr := r.dispatch(ctx, event)
		if dispatchErr == nil {
			file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err == nil {
				err = file.Close()
			}
			if err != nil && !os.IsExist(err) {
				dispatchErr = fmt.Errorf("mark MCP inventory reported: %w", err)
			}
		}
		unlock()
		return dispatchErr
	}
}

func (r *Runner) effectiveMCPInventory(base *Event) ([]mcpConfigEntry, bool) {
	switch base.Provider {
	case ProviderClaudeCode:
		launch := currentClaudeLaunchContext(base.Session.CWD)
		if launch.SafeMode {
			return nil, true
		}
		explicit := launch.explicitMCPEntries()
		if launch.StrictMCP {
			return explicit, true
		}
		if launch.Bare {
			if r.mcpListOff || len(launch.PluginDirs) == 0 {
				return explicit, true
			}
			entries, available := r.claudeMCPListSnapshot(launch)
			return firstMCPEntries(explicit, launch.barePluginEntries(entries)), available
		}
		if r.mcpListOff {
			return firstMCPEntries(explicit, loadMCPConfigEntries(ProviderClaudeCode, base.Session.CWD)), true
		}
		entries, available := r.claudeMCPListSnapshot(launch)
		if len(launch.ReplayArgs) > 0 {
			return firstMCPEntries(explicit, entries), available
		}
		return firstMCPEntries(explicit, loadMCPConfigEntries(ProviderClaudeCode, base.Session.CWD), entries), available
	case ProviderCodex:
		launch, ok := r.currentCodexLaunchContext(base.Session.CWD)
		if ok && !launch.Unreplayable && !r.mcpListOff {
			if entries, authoritative := r.codexMCPListEntries(launch); authoritative {
				return entries, true
			}
			return nil, false
		}
		return loadMCPConfigEntries(ProviderCodex, base.Session.CWD), true
	default:
		return loadMCPConfigEntries(base.Provider, base.Session.CWD), true
	}
}
