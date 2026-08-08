package agenthooks

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"unicode/utf8"
)

const (
	maxSkillContentBytes        = 65_536
	maxClaudePluginRegistrySize = 1 << 20
)

var (
	claudeSkillTokenRE      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	claudeManagedSkillsRoot = platformClaudeManagedSkillsRoot
)

// SkillActivation is a normalized Skill tool invocation.
type SkillActivation struct {
	// Name is the activated skill name.
	Name string

	// Content is the exact model-visible skill manifest when the tool completed.
	Content string
}

type skillAuthorization uint8

const (
	skillAuthorizationExact skillAuthorization = iota
	skillAuthorizationProject
	skillAuthorizationPersonal
)

type skillLocation struct {
	path          string
	level         string
	root          string
	authorization skillAuthorization
	owner         string
}

// SkillActivationOf projects a normalized skill-loading tool event into its
// activated name and, after completion, the exact content shown to the model.
// It returns nil for other tools and failed activations.
func SkillActivationOf(typed any) *SkillActivation {
	base := EventOf(typed)
	tool := toolOf(typed)
	if base == nil || tool == nil {
		return nil
	}
	if event, ok := typed.(*ToolPostEvent); ok && event.Failed {
		return nil
	}
	name := skillNameFromTool(base.Provider, tool)
	if name == "" {
		return nil
	}
	activation := &SkillActivation{Name: name, Content: ""}
	if event, ok := typed.(*ToolPostEvent); ok {
		_ = json.Unmarshal(event.Output, &activation.Content)
	}
	return activation
}

func skillNameFromTool(provider Provider, tool *ToolCall) string {
	if provider != ProviderClaudeCode || !strings.EqualFold(tool.Name, "Skill") {
		if tool.Canonical != ToolFileRead {
			return ""
		}
		var input struct {
			FilePath string `json:"file_path"`
			Path     string `json:"path"`
		}
		if json.Unmarshal(tool.Input, &input) != nil {
			return ""
		}
		path := input.FilePath
		if path == "" {
			path = input.Path
		}
		parts := strings.Split(strings.ReplaceAll(path, `\`, "/"), "/")
		if len(parts) < 3 || parts[len(parts)-1] != "SKILL.md" {
			return ""
		}
		nameIndex := len(parts) - 2
		skillsIndex := nameIndex - 1
		if parts[skillsIndex] == ".system" {
			skillsIndex--
		}
		if skillsIndex < 0 || parts[skillsIndex] != "skills" || !claudeSkillTokenRE.MatchString(parts[nameIndex]) {
			return ""
		}
		return parts[nameIndex]
	}
	var input struct {
		Skill string `json:"skill"`
		Name  string `json:"name"`
	}
	if json.Unmarshal(tool.Input, &input) != nil {
		return ""
	}
	name := strings.TrimSpace(input.Skill)
	if name == "" {
		name = strings.TrimSpace(input.Name)
	}
	if name == "" {
		return ""
	}
	return name
}

func readClaudeSkillContent(name, cwd string) (string, bool) {
	location := resolveClaudeSkill(name, cwd)
	if location.path == "" {
		return "", false
	}
	file, _, ok := openValidatedSkill(location)
	if !ok {
		return "", false
	}
	content, readErr := io.ReadAll(io.LimitReader(file, maxSkillContentBytes+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || len(content) > maxSkillContentBytes || !utf8.Valid(content) {
		return "", false
	}
	return string(content), true
}

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
	content, ok := readClaudeSkillContent(strings.TrimSpace(input.Skill), event.Session.CWD)
	if !ok {
		return
	}
	if output, err := json.Marshal(content); err == nil {
		event.Output = output
	}
}

func resolveClaudeSkill(name, cwd string) skillLocation {
	if plugin, skill, ok := strings.Cut(name, ":"); ok {
		if !claudeSkillTokenRE.MatchString(plugin) || !claudeSkillTokenRE.MatchString(skill) || strings.Contains(skill, ":") {
			return skillLocation{}
		}
		return resolveClaudePluginSkill(plugin, skill, cwd)
	}
	if !claudeSkillTokenRE.MatchString(name) {
		return skillLocation{}
	}
	if root := claudeManagedSkillsRoot(); root != "" {
		path := filepath.Join(root, name, "SKILL.md")
		info, err := os.Stat(path)
		switch {
		case err == nil && info.Mode().IsRegular():
			if readableRegularFile(path) {
				return exactSkillLocation(path, "admin", root)
			}
			return skillLocation{}
		case err != nil && !os.IsNotExist(err):
			return skillLocation{}
		}
	}

	home, _ := os.UserHomeDir()
	configRoot := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR"))
	if configRoot == "" && home != "" {
		configRoot = filepath.Join(home, ".claude")
	}
	if configRoot != "" {
		root := filepath.Join(configRoot, "skills")
		if path := existingSkillManifest(filepath.Join(root, name)); path != "" {
			if home != "" && filepath.Clean(configRoot) == filepath.Join(filepath.Clean(home), ".claude") {
				return personalSkillLocation(path, "personal", root, home)
			}
			return exactSkillLocation(path, "personal", root)
		}
	}
	if filepath.IsAbs(cwd) {
		for dir := cwd; ; dir = filepath.Dir(dir) {
			root := filepath.Join(dir, ".claude", "skills")
			if path := existingSkillManifest(filepath.Join(root, name)); path != "" {
				return projectSkillLocation(path, "project", root, dir)
			}
			if pathExists(filepath.Join(dir, ".git")) {
				break
			}
			if parent := filepath.Dir(dir); parent == dir {
				break
			}
		}
	}
	return skillLocation{}
}

func platformClaudeManagedSkillsRoot() string {
	switch runtime.GOOS {
	case "darwin":
		return "/Library/Application Support/ClaudeCode/.claude/skills"
	case "linux":
		return "/etc/claude-code/.claude/skills"
	case "windows":
		return `C:\Program Files\ClaudeCode\.claude\skills`
	default:
		return ""
	}
}

func resolveClaudePluginSkill(plugin, skill, cwd string) skillLocation {
	home, _ := os.UserHomeDir()
	configRoot := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR"))
	if configRoot == "" && home != "" {
		configRoot = filepath.Join(home, ".claude")
	}
	if configRoot == "" {
		return skillLocation{}
	}
	registryFile, err := os.Open(filepath.Join(configRoot, "plugins", "installed_plugins.json"))
	if err != nil {
		return skillLocation{}
	}
	data, readErr := io.ReadAll(io.LimitReader(registryFile, maxClaudePluginRegistrySize+1))
	closeErr := registryFile.Close()
	if readErr != nil || closeErr != nil || len(data) > maxClaudePluginRegistrySize {
		return skillLocation{}
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
		return skillLocation{}
	}

	type candidate struct {
		installPath string
		projectPath string
	}
	var projectCandidates, userCandidates []candidate
	for key, records := range registry.Plugins {
		prefix, _, _ := strings.Cut(key, "@")
		if prefix != plugin {
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
			if len(filepath.Clean(candidate.projectPath)) > longest {
				longest = len(filepath.Clean(candidate.projectPath))
			}
		}
		candidates = nil
		for _, candidate := range projectCandidates {
			if len(filepath.Clean(candidate.projectPath)) == longest {
				candidates = append(candidates, candidate)
			}
		}
	}
	if len(candidates) != 1 {
		return skillLocation{}
	}
	for _, dir := range []string{filepath.Join(candidates[0].installPath, "skills", skill), candidates[0].installPath} {
		if path := existingSkillManifest(dir); path != "" {
			return exactSkillLocation(path, "plugin", candidates[0].installPath)
		}
	}
	return skillLocation{}
}

func exactSkillLocation(path, level, root string) skillLocation {
	return skillLocation{path: path, level: level, root: root, authorization: skillAuthorizationExact}
}

func projectSkillLocation(path, level, root, owner string) skillLocation {
	return skillLocation{path: path, level: level, root: root, authorization: skillAuthorizationProject, owner: owner}
}

func personalSkillLocation(path, level, root, owner string) skillLocation {
	return skillLocation{path: path, level: level, root: root, authorization: skillAuthorizationPersonal, owner: owner}
}

func existingSkillManifest(dir string) string {
	path := filepath.Join(dir, "SKILL.md")
	info, err := os.Stat(path)
	if err == nil && info.Mode().IsRegular() {
		return path
	}
	return ""
}

func readableRegularFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	openedInfo, statErr := file.Stat()
	closeErr := file.Close()
	return statErr == nil && openedInfo.Mode().IsRegular() && closeErr == nil
}

func openValidatedSkill(location skillLocation) (*os.File, string, bool) {
	if !filepath.IsAbs(location.path) || !filepath.IsAbs(location.root) {
		return nil, "", false
	}
	resolved, err := filepath.EvalSymlinks(location.path)
	if err != nil {
		return nil, "", false
	}
	resolvedRoot, err := filepath.EvalSymlinks(location.root)
	if err != nil {
		return nil, "", false
	}
	authorizedRoot, ok := authorizedSkillRoot(location, resolvedRoot, resolved)
	if !ok {
		return nil, "", false
	}
	rootDir, err := os.OpenRoot(filepath.Dir(resolved))
	if err != nil {
		return nil, "", false
	}
	name := filepath.Base(resolved)
	info, err := rootDir.Stat(name)
	if err != nil || !info.Mode().IsRegular() {
		_ = rootDir.Close()
		return nil, "", false
	}
	file, err := rootDir.Open(name)
	closeRootErr := rootDir.Close()
	if err != nil || closeRootErr != nil {
		if file != nil {
			_ = file.Close()
		}
		return nil, "", false
	}
	openedInfo, err := file.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() {
		_ = file.Close()
		return nil, "", false
	}
	return file, authorizedRoot, true
}

func authorizedSkillRoot(location skillLocation, resolvedRoot, resolvedPath string) (string, bool) {
	switch location.authorization {
	case skillAuthorizationProject, skillAuthorizationPersonal:
		if !filepath.IsAbs(location.owner) {
			return "", false
		}
		resolvedOwner, err := filepath.EvalSymlinks(location.owner)
		if err != nil {
			return "", false
		}
		allowedRoots := providerSkillRoots(resolvedOwner)
		if !pathWithinAny(resolvedRoot, allowedRoots) {
			return "", false
		}
		sourceRoots := providerSkillRoots(location.owner)
		for i, root := range allowedRoots {
			if pathWithin(resolvedPath, root) {
				return filepath.Clean(sourceRoots[i]), true
			}
		}
		return "", false
	case skillAuthorizationExact:
		resolvedParent, err := filepath.EvalSymlinks(filepath.Dir(location.root))
		if err == nil && pathWithin(resolvedRoot, resolvedParent) && pathWithin(resolvedPath, resolvedRoot) {
			return filepath.Clean(location.root), true
		}
		return "", false
	default:
		return "", false
	}
}

func providerSkillRoots(owner string) []string {
	roots := make([]string, 0, 4)
	for _, provider := range []string{".agents", ".claude", ".codex", ".cursor"} {
		roots = append(roots, filepath.Join(owner, provider, "skills"))
	}
	return roots
}

func pathWithinAny(path string, roots []string) bool {
	for _, root := range roots {
		if pathWithin(path, root) {
			return true
		}
	}
	return false
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
