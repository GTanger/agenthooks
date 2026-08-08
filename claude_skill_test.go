package agenthooks

import (
	"crypto/sha256"
	"encoding/hex"
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
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	originalManagedRoot := claudeManagedSkillsRoot
	claudeManagedSkillsRoot = func() string { return "" }
	t.Cleanup(func() { claudeManagedSkillsRoot = originalManagedRoot })
	return home
}

func writeClaudeSkillManifest(t *testing.T, dir, content string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestResolveClaudeSkillPersonalBeatsProject(t *testing.T) {
	home := isolateClaudeSkillRoots(t)
	repo := filepath.Join(t.TempDir(), "repo")
	cwd := filepath.Join(repo, "nested")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	personalPath := writeClaudeSkillManifest(t, filepath.Join(home, ".claude", "skills", "shared"), "personal")
	writeClaudeSkillManifest(t, filepath.Join(repo, ".claude", "skills", "shared"), "project")

	resolved := ResolveClaudeSkill("shared", cwd)

	if !resolved.CaptureReady || resolved.SourceLevel != "personal" || resolved.SourcePath != personalPath || resolved.Content != "personal" {
		t.Fatalf("unexpected resolution: %+v", resolved)
	}
	sum := sha256.Sum256([]byte("personal"))
	if resolved.RawSHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("RawSHA256 = %q", resolved.RawSHA256)
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
	projectPath := writeClaudeSkillManifest(t, filepath.Join(projectInstall, "skills", "review"), "project plugin")
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

	resolved := ResolveClaudeSkill("quality:review", cwd)

	if !resolved.CaptureReady || resolved.SourceLevel != "plugin" || resolved.SourcePath != projectPath || resolved.Content != "project plugin" {
		t.Fatalf("unexpected plugin resolution: %+v", resolved)
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

	resolved := ResolveClaudeSkill("linked", repo)

	if !resolved.CaptureReady || resolved.SourceLevel != "project" || resolved.Content != "linked body" {
		t.Fatalf("unexpected symlink resolution: %+v", resolved)
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

	resolved := ResolveClaudeSkill("escape", repo)

	if resolved.CaptureReady || resolved.Content != "" || resolved.RawSHA256 != "" || resolved.SourcePath != "" {
		t.Fatalf("escaped skill was captured: %+v", resolved)
	}
}

func TestResolveClaudeSkillHashesOversizedManifestWithoutCapturing(t *testing.T) {
	isolateClaudeSkillRoots(t)
	repo := t.TempDir()
	content := strings.Repeat("a", maxSkillContentBytes+8192) + "tail"
	writeClaudeSkillManifest(t, filepath.Join(repo, ".claude", "skills", "oversized"), content)

	resolved := ResolveClaudeSkill("oversized", repo)

	sum := sha256.Sum256([]byte(content))
	if resolved.CaptureReady || resolved.Content != "" || resolved.RawSHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("unexpected oversized resolution: %+v", resolved)
	}
}
