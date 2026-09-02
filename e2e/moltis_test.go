package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/speakeasy-api/agenthooks"
	"github.com/speakeasy-api/agenthooks/install"
)

// TestMoltisEventsAndDecisions drives the real Moltis Gateway against an
// explicitly configured local OpenAI-compatible model. One isolated Gateway
// proves all three mutable surfaces end to end: prompt context reaches stored
// history, Deny prevents exec, and updated input replaces the command Moltis
// executes. The user's live config, data directory, and Gateway are untouched.
func TestMoltisEventsAndDecisions(t *testing.T) {
	bin := requireE2E(t, "moltis")
	baseURL := strings.TrimRight(os.Getenv("AGENTHOOKS_MOLTIS_BASE_URL"), "/")
	rawModel := os.Getenv("AGENTHOOKS_MOLTIS_MODEL")
	if baseURL == "" || rawModel == "" {
		t.Skip("set AGENTHOOKS_MOLTIS_BASE_URL and AGENTHOOKS_MOLTIS_MODEL for the isolated Moltis E2E")
	}
	if strings.ContainsAny(baseURL+rawModel, "\n\r\"") {
		t.Fatal("Moltis E2E URL/model contains a character unsafe for the generated TOML fixture")
	}

	proj := t.TempDir()
	configDir := t.TempDir()
	dataDir := t.TempDir()
	rec := newRecorderWithConfig(t, recorderConfig{
		PromptContext: "Portable context marker: AGENTHOOKS_MOLTIS_CONTEXT_OK",
	})
	installHooks(t, rec, agenthooks.ProviderMoltis, install.ScopeUser, dataDir)

	port := unusedLocalPort(t)
	writeMoltisE2EConfig(t, configDir, port, baseURL, rawModel)
	logPath, stop := startMoltisGateway(t, bin, proj, configDir, dataDir, port)
	defer stop()
	client := &http.Client{Timeout: 10 * time.Second}
	waitMoltis(t, 30*time.Second, func() bool {
		resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/health", port))
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, "Gateway health", logPath)

	model := "lmstudio::" + rawModel
	allowed := filepath.Join(proj, "allowed-marker.txt")
	postGraphQL(t, client, port,
		"mutation($message:String!,$session:String!,$model:String){chat{send(message:$message,sessionKey:$session,model:$model){ok}}}",
		map[string]any{
			"message": "Use the exec tool once to run exactly `touch " + allowed + "`, then stop. Include any portable context marker appended to this message in the final answer.",
			"session": "agenthooks-e2e-allow",
			"model":   model,
		})
	waitMoltis(t, 2*time.Minute, func() bool {
		return fileExists(allowed) && fileContains(rec.Events, `"native":"AfterToolCall"`)
	}, "allowed tool.post", logPath)
	waitMoltis(t, 2*time.Minute, func() bool {
		return moltisHistoryContains(client, port, "agenthooks-e2e-allow", "assistant", "AGENTHOOKS_MOLTIS_CONTEXT_OK")
	}, "prompt context in the model response", logPath)
	evs := rec.events(t)
	requireKinds(t, evs, agenthooks.KindPromptSubmitted, agenthooks.KindToolPre, agenthooks.KindToolPost)
	if !hasCanonicalTool(evs, agenthooks.ToolShell) {
		t.Fatalf("Moltis tool did not normalize as shell:\n%s", summarize(evs))
	}

	denyRec := recorder{Bin: rec.Bin, Events: filepath.Join(filepath.Dir(rec.Events), "deny-events.jsonl")}
	setRecorderConfig(t, denyRec, recorderConfig{Deny: string(agenthooks.ToolShell)})
	denied := filepath.Join(proj, "denied-marker.txt")
	postGraphQL(t, client, port,
		"mutation($message:String!,$session:String!,$model:String){chat{send(message:$message,sessionKey:$session,model:$model){ok}}}",
		map[string]any{
			"message": "Use the exec tool once to run exactly `touch " + denied + "`. If blocked, stop without another method.",
			"session": "agenthooks-e2e-deny",
			"model":   model,
		})
	waitMoltis(t, 2*time.Minute, func() bool {
		history, err := moltisHistory(client, port, "agenthooks-e2e-deny")
		return err == nil && fileContains(denyRec.Events, `"denied":true`) &&
			bytes.Contains(history, []byte("blocked by hook: blocked by agenthooks e2e"))
	}, "denied tool result", logPath)
	if fileExists(denied) {
		t.Fatal("Moltis created the marker despite the portable Deny decision")
	}

	rewriteRec := recorder{Bin: rec.Bin, Events: filepath.Join(filepath.Dir(rec.Events), "rewrite-events.jsonl")}
	rewritten := filepath.Join(proj, "rewritten-marker.txt")
	setRecorderConfig(t, rewriteRec, recorderConfig{RewriteCommand: "touch " + rewritten})
	original := filepath.Join(proj, "original-marker.txt")
	postGraphQL(t, client, port,
		"mutation($message:String!,$session:String!,$model:String){chat{send(message:$message,sessionKey:$session,model:$model){ok}}}",
		map[string]any{
			"message": "Use the exec tool once to run exactly `touch " + original + "`, then stop.",
			"session": "agenthooks-e2e-rewrite",
			"model":   model,
		})
	waitMoltis(t, 2*time.Minute, func() bool {
		return fileExists(rewritten) && fileContains(rewriteRec.Events, `"rewritten":true`)
	}, "rewritten tool input", logPath)
	if fileExists(original) {
		t.Fatal("Moltis ran the original command instead of the portable input rewrite")
	}
}

func unusedLocalPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

func writeMoltisE2EConfig(t *testing.T, dir string, port int, baseURL, model string) {
	t.Helper()
	content := fmt.Sprintf(`[server]
bind = "127.0.0.1"
port = %d
terminal_enabled = false

[auth]
disabled = true
vault_enabled = false

[graphql]
enabled = true

[tls]
enabled = false

[providers]
offered = ["lmstudio"]

[providers.lmstudio]
enabled = true
base_url = %s
models = [%s]
fetch_models = false
tool_mode = "native"

[chat]
priority_models = [%s]
auto_title = false

[memory]
style = "off"
agent_write_mode = "off"
disable_rag = true
session_export = "off"

[tools]
agent_timeout_secs = 180
agent_max_iterations = 5
agent_max_auto_continues = 0

[tools.exec]
approval_mode = "never"
security_level = "permissive"

[tools.exec.sandbox]
mode = "off"
`, port, strconv.Quote(baseURL), strconv.Quote(model), strconv.Quote("lmstudio::"+model))
	if err := os.WriteFile(filepath.Join(dir, "moltis.toml"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func startMoltisGateway(t *testing.T, bin, proj, configDir, dataDir string, port int) (string, func()) {
	t.Helper()
	logPath := filepath.Join(dataDir, "gateway.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin,
		"--log-level", "warn",
		"--config-dir", configDir,
		"--data-dir", dataDir,
		"--bind", "127.0.0.1",
		"--port", strconv.Itoa(port),
		"gateway")
	cmd.Dir = proj
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		t.Fatal(err)
	}
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = logFile.Close()
	}
	t.Cleanup(stop)
	return logPath, stop
}

func postGraphQL(t *testing.T, client *http.Client, port int, query string, variables map[string]any) []byte {
	t.Helper()
	result, err := requestGraphQL(client, port, query, variables)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func requestGraphQL(client *http.Client, port int, query string, variables map[string]any) ([]byte, error) {
	body, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		return nil, err
	}
	resp, err := client.Post(fmt.Sprintf("http://127.0.0.1:%d/graphql", port), "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	result, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK || bytes.Contains(result, []byte(`"errors"`)) {
		return nil, fmt.Errorf("moltis GraphQL failed (%s): %s", resp.Status, result)
	}
	return result, nil
}

func moltisHistory(client *http.Client, port int, session string) ([]byte, error) {
	return requestGraphQL(client, port,
		"query($session:String!){chat{history(sessionKey:$session)}}",
		map[string]any{"session": session})
}

func moltisHistoryContains(client *http.Client, port int, session, role, substring string) bool {
	var response struct {
		Data struct {
			Chat struct {
				History []struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"history"`
			} `json:"chat"`
		} `json:"data"`
	}
	history, err := moltisHistory(client, port, session)
	if err != nil {
		return false
	}
	if err := json.Unmarshal(history, &response); err != nil {
		return false
	}
	for _, message := range response.Data.Chat.History {
		if message.Role == role && strings.Contains(message.Content, substring) {
			return true
		}
	}
	return false
}

func waitMoltis(t *testing.T, timeout time.Duration, ready func() bool, what, logPath string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	logs, _ := os.ReadFile(logPath)
	t.Fatalf("timed out waiting for Moltis %s\nGateway logs:\n%s", what, tail(string(logs), 6000))
}

func fileContains(path, needle string) bool {
	data, err := os.ReadFile(path)
	return err == nil && bytes.Contains(data, []byte(needle))
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func hasCanonicalTool(evs []event, canonical agenthooks.CanonicalTool) bool {
	for _, event := range typedToolPres(evs) {
		if event.Canonical == string(canonical) {
			return true
		}
	}
	return false
}
