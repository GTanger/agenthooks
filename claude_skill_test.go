package agenthooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func isolateClaudeSkillRoots(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	originalManagedRoot := claudeManagedSkillsRoot
	claudeManagedSkillsRoot = func() string { return "" }
	t.Cleanup(func() { claudeManagedSkillsRoot = originalManagedRoot })
	return home
}

func writeClaudeSkillManifest(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestResolveClaudeSkillPersonalBeatsProject(t *testing.T) {
	home := isolateClaudeSkillRoots(t)
	repo := filepath.Join(t.TempDir(), "repo")
	cwd := filepath.Join(repo, "nested")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeClaudeSkillManifest(t, filepath.Join(home, ".claude", "skills", "shared"), "personal")
	writeClaudeSkillManifest(t, filepath.Join(repo, ".claude", "skills", "shared"), "project")

	content, ok := readClaudeSkillContent("shared", cwd)

	if !ok || content != "personal" {
		t.Fatalf("readClaudeSkillContent() = %q, %t", content, ok)
	}
}

func TestResolveClaudeSkillUsesApplicablePluginRecord(t *testing.T) {
	isolateClaudeSkillRoots(t)
	configRoot := filepath.Join(t.TempDir(), "claude")
	t.Setenv("CLAUDE_CONFIG_DIR", configRoot)
	repo := filepath.Join(t.TempDir(), "repo")
	cwd := filepath.Join(repo, "nested")
	projectInstall := filepath.Join(t.TempDir(), "project-plugin")
	userInstall := filepath.Join(t.TempDir(), "user-plugin")
	writeClaudeSkillManifest(t, filepath.Join(projectInstall, "skills", "review"), "project plugin")
	writeClaudeSkillManifest(t, filepath.Join(userInstall, "skills", "review"), "user plugin")
	registry := map[string]any{
		"version": 2,
		"plugins": map[string]any{
			"quality@marketplace": []map[string]string{
				{"scope": "user", "installPath": userInstall},
				{"scope": "project", "projectPath": repo, "installPath": projectInstall},
			},
		},
	}
	data, err := json.Marshal(registry)
	if err != nil {
		t.Fatal(err)
	}
	registryPath := filepath.Join(configRoot, "plugins", "installed_plugins.json")
	if err := os.MkdirAll(filepath.Dir(registryPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(registryPath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	content, ok := readClaudeSkillContent("quality:review", cwd)

	if !ok || content != "project plugin" {
		t.Fatalf("readClaudeSkillContent() = %q, %t", content, ok)
	}
}

func TestResolveClaudeSkillFollowsSiblingSkillTreeSymlink(t *testing.T) {
	isolateClaudeSkillRoots(t)
	repo := filepath.Join(t.TempDir(), "repo")
	targetDir := filepath.Join(repo, ".agents", "skills", "linked")
	writeClaudeSkillManifest(t, targetDir, "linked body")
	claudeRoot := filepath.Join(repo, ".claude", "skills")
	if err := os.MkdirAll(claudeRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "..", ".agents", "skills", "linked"), filepath.Join(claudeRoot, "linked")); err != nil {
		t.Fatal(err)
	}

	content, ok := readClaudeSkillContent("linked", repo)

	if !ok || content != "linked body" {
		t.Fatalf("readClaudeSkillContent() = %q, %t", content, ok)
	}
}

func TestResolveClaudeSkillRejectsSymlinkEscape(t *testing.T) {
	isolateClaudeSkillRoots(t)
	repo := t.TempDir()
	external := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(external, []byte("private material"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(repo, ".claude", "skills", "escape", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, path); err != nil {
		t.Fatal(err)
	}

	if content, ok := readClaudeSkillContent("escape", repo); ok || content != "" {
		t.Fatalf("escaped skill was captured: %q", content)
	}
}

func TestResolveClaudeSkillRejectsOversizedManifest(t *testing.T) {
	isolateClaudeSkillRoots(t)
	repo := t.TempDir()
	content := strings.Repeat("a", maxSkillContentBytes+8192) + "tail"
	writeClaudeSkillManifest(t, filepath.Join(repo, ".claude", "skills", "oversized"), content)

	if captured, ok := readClaudeSkillContent("oversized", repo); ok || captured != "" {
		t.Fatalf("oversized skill was captured: %q", captured)
	}
}

func TestSkillActivationOfFileRead(t *testing.T) {
	event := &ToolPostEvent{
		Event: Event{Provider: ProviderCursor, Kind: KindToolPost},
		Tool: ToolCall{
			Name:      "read_file",
			Canonical: ToolFileRead,
			Input:     json.RawMessage(`{"file_path":"C:\\repo\\.cursor\\skills\\review\\SKILL.md"}`),
		},
		Output: json.RawMessage(`"review content"`),
	}

	activation := SkillActivationOf(event)
	if activation == nil || activation.Name != "review" || activation.Content != "review content" || !activation.ContentAvailable || activation.Explicit {
		t.Fatalf("SkillActivationOf() = %+v", activation)
	}
}

func TestSkillActivationOfShellRead(t *testing.T) {
	event := &ToolPreEvent{
		Event: Event{Provider: ProviderCodex, Kind: KindToolPre},
		Tool: ToolCall{
			Name:      "Bash",
			Canonical: ToolShell,
			Input:     json.RawMessage(`{"command":"sed -n 1,200p '/repo/.agents/skills/review/SKILL.md'"}`),
		},
	}

	activation := SkillActivationOf(event)
	if activation == nil || activation.Name != "review" || activation.ContentAvailable {
		t.Fatalf("SkillActivationOf() = %+v", activation)
	}
}

func TestSkillActivationOfIgnoresPermissionPreview(t *testing.T) {
	event := &PermissionEvent{
		Event: Event{Provider: ProviderCodex, Kind: KindPermission},
		Tool: ToolCall{
			Name:      "Bash",
			Canonical: ToolShell,
			Input:     json.RawMessage(`{"command":"cat /repo/.agents/skills/review/SKILL.md"}`),
		},
	}

	if activation := SkillActivationOf(event); activation != nil {
		t.Fatalf("SkillActivationOf() = %+v", activation)
	}
}
