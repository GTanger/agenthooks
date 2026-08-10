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
	writeClaudeSkillManifest(t, filepath.Join(userInstall, "review"), "user plugin")
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

	content, ok = readClaudeSkillContent("quality:review", t.TempDir())
	if !ok || content != "user plugin" {
		t.Fatalf("readClaudeSkillContent() direct plugin layout = %q, %t", content, ok)
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

func TestSkillActivationOfShellCatRead(t *testing.T) {
	event := &ToolPreEvent{
		Event: Event{Provider: ProviderCodex, Kind: KindToolPre},
		Tool: ToolCall{
			Name:      "Bash",
			Canonical: ToolShell,
			Input:     json.RawMessage(`{"command":"cat /repo/.agents/skills/review/SKILL.md"}`),
		},
	}

	activation := SkillActivationOf(event)
	if activation == nil || activation.Name != "review" || activation.ContentAvailable {
		t.Fatalf("SkillActivationOf() = %+v", activation)
	}
}

func TestSkillActivationOfRejectsNonReadingShellCommands(t *testing.T) {
	commands := []string{
		`# cat /repo/.agents/skills/review/SKILL.md`,
		`cat /repo/.agents/skills/review/SKILL.md # activate review`,
		`echo /repo/.agents/skills/review/SKILL.md`,
		`printf '%s\n' /repo/.agents/skills/review/SKILL.md`,
		`test -f /repo/.agents/skills/review/SKILL.md`,
		`cat /repo/.agents/skills/review/SKILL.md > /tmp/review`,
		`cat --help /repo/.agents/skills/review/SKILL.md`,
		`cat "$(printf /repo)/.agents/skills/review/SKILL.md"`,
		`printf replacement > /repo/.agents/skills/review/SKILL.md`,
		`sed -i 1d /repo/.agents/skills/review/SKILL.md`,
		`cat /repo/.agents/skills/review/SKILL.md /repo/.agents/skills/deploy/SKILL.md`,
		`cat /not-a-skill\;/repo/.agents/skills/review/SKILL.md`,
		`cat /not-a-skill\#/repo/.agents/skills/review/SKILL.md`,
		`sed -n 1,200p /not-a-skill\|/repo/.agents/skills/review/SKILL.md`,
	}

	for _, command := range commands {
		input, err := json.Marshal(map[string]string{"command": command})
		if err != nil {
			t.Fatal(err)
		}
		event := &ToolPreEvent{
			Event: Event{Provider: ProviderCodex, Kind: KindToolPre},
			Tool:  ToolCall{Name: "Bash", Canonical: ToolShell, Input: input},
		}
		if activation := SkillActivationOf(event); activation != nil {
			t.Errorf("SkillActivationOf(%q) = %+v", command, activation)
		}
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

func TestBackfillSkillOutputImplicitFileRead(t *testing.T) {
	content := "---\nname: review\n---\n\nInspect the change.\n"
	repo := t.TempDir()
	writeClaudeSkillManifest(t, filepath.Join(repo, ".agents", "skills", "review"), content)
	manifest := filepath.Join(repo, ".agents", "skills", "review", "SKILL.md")
	event := &ToolPostEvent{
		Event: Event{Provider: ProviderCursor, Kind: KindToolPost, Session: SessionInfo{CWD: repo}},
		Tool: ToolCall{
			Name:      "read_file",
			Canonical: ToolFileRead,
			Input:     json.RawMessage(`{"file_path":` + mustJSON(t, manifest) + `}`),
		},
		Output: json.RawMessage(`{"success":true,"linesRead":5}`),
	}

	backfillSkillOutput(event)

	activation := SkillActivationOf(event)
	if activation == nil || activation.Content != content || !activation.ContentAvailable || activation.Explicit {
		t.Fatalf("SkillActivationOf() = %+v", activation)
	}
}

func TestBackfillSkillOutputImplicitShellCatRelativePath(t *testing.T) {
	content := "---\nname: review\n---\n\nInspect the change.\n"
	repo := t.TempDir()
	writeClaudeSkillManifest(t, filepath.Join(repo, ".agents", "skills", "review"), content)
	event := &ToolPostEvent{
		Event: Event{Provider: ProviderCodex, Kind: KindToolPost, Session: SessionInfo{CWD: repo}},
		Tool: ToolCall{
			Name:      "Bash",
			Canonical: ToolShell,
			Input:     json.RawMessage(`{"command":"cat .agents/skills/review/SKILL.md"}`),
		},
		Output: json.RawMessage(`{"output":"---\nname: review\n---","metadata":{"exit_code":0}}`),
	}

	backfillSkillOutput(event)

	activation := SkillActivationOf(event)
	if activation == nil || activation.Content != content || !activation.ContentAvailable {
		t.Fatalf("SkillActivationOf() = %+v", activation)
	}
}

func TestBackfillSkillOutputKeepsStringOutput(t *testing.T) {
	event := &ToolPostEvent{
		Event: Event{Provider: ProviderCursor, Kind: KindToolPost},
		Tool: ToolCall{
			Name:      "read_file",
			Canonical: ToolFileRead,
			Input:     json.RawMessage(`{"file_path":"/repo/.agents/skills/review/SKILL.md"}`),
		},
		Output: json.RawMessage(`"model-visible content"`),
	}

	backfillSkillOutput(event)

	if string(event.Output) != `"model-visible content"` {
		t.Fatalf("Output = %s, want provider string untouched", event.Output)
	}
}

func TestBackfillSkillOutputIgnoresNonSkillReads(t *testing.T) {
	original := json.RawMessage(`{"success":true}`)
	event := &ToolPostEvent{
		Event: Event{Provider: ProviderCursor, Kind: KindToolPost},
		Tool: ToolCall{
			Name:      "read_file",
			Canonical: ToolFileRead,
			Input:     json.RawMessage(`{"file_path":"/repo/README.md"}`),
		},
		Output: original,
	}

	backfillSkillOutput(event)

	if string(event.Output) != string(original) {
		t.Fatalf("Output = %s, want untouched", event.Output)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
