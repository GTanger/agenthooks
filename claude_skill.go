package agenthooks

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

const (
	maxClaudeSkillBytes          = 1 << 20
	maxClaudePluginRegistryBytes = 1 << 20
)

var claudeSkillTokenRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// Claude reports only the skill name in PostToolUse, although the model receives
// the resolved manifest. Resolve the same local skill so Output reflects the
// model-visible content; Event.Raw retains Claude's response verbatim.
func backfillClaudeSkillOutput(event *ToolPostEvent) {
	if event.Failed || !strings.EqualFold(event.Tool.Name, "Skill") {
		return
	}
	var input struct {
		Skill string `json:"skill"`
	}
	if json.Unmarshal(event.Tool.Input, &input) != nil {
		return
	}
	path := resolveClaudeSkillPath(strings.TrimSpace(input.Skill), event.Session.CWD)
	if path == "" {
		return
	}
	file, err := os.Open(path)
	if err != nil {
		return
	}
	content, readErr := io.ReadAll(io.LimitReader(file, maxClaudeSkillBytes+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || len(content) > maxClaudeSkillBytes {
		return
	}
	if output, err := json.Marshal(string(content)); err == nil {
		event.Output = output
	}
}

func resolveClaudeSkillPath(name, cwd string) string {
	if plugin, skill, ok := strings.Cut(name, ":"); ok {
		if !claudeSkillTokenRE.MatchString(plugin) || !claudeSkillTokenRE.MatchString(skill) || strings.Contains(skill, ":") {
			return ""
		}
		return resolveClaudePluginSkillPath(plugin, skill, cwd)
	}
	if !claudeSkillTokenRE.MatchString(name) {
		return ""
	}

	managedRoot := ""
	switch runtime.GOOS {
	case "darwin":
		managedRoot = "/Library/Application Support/ClaudeCode/.claude/skills"
	case "linux":
		managedRoot = "/etc/claude-code/.claude/skills"
	case "windows":
		managedRoot = `C:\Program Files\ClaudeCode\.claude\skills`
	}
	if path := existingClaudeSkillManifest(managedRoot, name); path != "" {
		return path
	}

	if configRoot := claudeConfigRoot(); configRoot != "" {
		if path := existingClaudeSkillManifest(filepath.Join(configRoot, "skills"), name); path != "" {
			return path
		}
	}
	if filepath.IsAbs(cwd) {
		for dir := cwd; ; dir = filepath.Dir(dir) {
			if path := existingClaudeSkillManifest(filepath.Join(dir, ".claude", "skills"), name); path != "" {
				return path
			}
			if pathExists(filepath.Join(dir, ".git")) || filepath.Dir(dir) == dir {
				break
			}
		}
	}
	return ""
}

func resolveClaudePluginSkillPath(plugin, skill, cwd string) string {
	configRoot := claudeConfigRoot()
	if configRoot == "" {
		return ""
	}
	file, err := os.Open(filepath.Join(configRoot, "plugins", "installed_plugins.json"))
	if err != nil {
		return ""
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxClaudePluginRegistryBytes+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || len(data) > maxClaudePluginRegistryBytes {
		return ""
	}
	var registry struct {
		Version int `json:"version"`
		Plugins map[string][]struct {
			Scope       string `json:"scope"`
			ProjectPath string `json:"projectPath"`
			InstallPath string `json:"installPath"`
		} `json:"plugins"`
	}
	if json.Unmarshal(data, &registry) != nil || registry.Version != 2 {
		return ""
	}

	type candidate struct {
		installPath string
		projectPath string
	}
	var projectCandidates, userCandidates []candidate
	for key, records := range registry.Plugins {
		name, _, _ := strings.Cut(key, "@")
		if name != plugin {
			continue
		}
		for _, record := range records {
			switch record.Scope {
			case "project", "local":
				if record.InstallPath != "" && pathWithin(cwd, record.ProjectPath) {
					projectCandidates = append(projectCandidates, candidate{installPath: record.InstallPath, projectPath: record.ProjectPath})
				}
			case "user":
				if record.InstallPath != "" {
					userCandidates = append(userCandidates, candidate{installPath: record.InstallPath})
				}
			}
		}
	}

	candidates := userCandidates
	if len(projectCandidates) > 0 {
		longest := 0
		for _, candidate := range projectCandidates {
			longest = max(longest, len(filepath.Clean(candidate.projectPath)))
		}
		candidates = nil
		for _, candidate := range projectCandidates {
			if len(filepath.Clean(candidate.projectPath)) == longest {
				candidates = append(candidates, candidate)
			}
		}
	}
	if len(candidates) != 1 {
		return ""
	}
	for _, root := range []string{filepath.Join(candidates[0].installPath, "skills"), candidates[0].installPath} {
		if path := existingClaudeSkillManifest(root, skill); path != "" {
			return path
		}
	}
	return ""
}

func claudeConfigRoot() string {
	if root := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); root != "" {
		return root
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude")
}

func existingClaudeSkillManifest(root, name string) string {
	if root == "" {
		return ""
	}
	path := filepath.Join(root, name, "SKILL.md")
	if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
		return path
	}
	return ""
}

func pathWithin(path, root string) bool {
	if path == "" || root == "" {
		return false
	}
	pathAbs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(rootAbs, pathAbs)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
