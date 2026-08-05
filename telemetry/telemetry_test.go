package telemetry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewValidatesConfig(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Errorf("empty endpoint must fail at construction")
	}
	if _, err := New(Config{Endpoint: "not a url", SpoolDir: t.TempDir()}); err == nil {
		t.Errorf("malformed endpoint must fail at construction")
	}
	if _, err := New(Config{Endpoint: "grpc://example.com/v1/logs", SpoolDir: t.TempDir()}); err == nil {
		t.Errorf("non-http endpoint must fail at construction")
	}
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{Endpoint: testEndpoint, SpoolDir: filepath.Join(blocker, "spool")}); err == nil {
		t.Errorf("uncreatable spool dir must fail at construction")
	}
}

func TestCaptureContentLevel(t *testing.T) {
	rec := newTestRecorder(t, func(cfg *Config) { cfg.Capture = CaptureContent })
	hr := toolPreRecord()
	if err := rec.RecordHook(hr); err != nil {
		t.Fatalf("RecordHook: %v", err)
	}
	prompt := toolPreRecord()
	prompt.Kind, prompt.NativeName = "prompt.submitted", "UserPromptSubmit"
	prompt.Tool = nil
	prompt.Prompt = "deploy with API_TOKEN=supersecret please"
	if err := rec.RecordHook(prompt); err != nil {
		t.Fatalf("RecordHook: %v", err)
	}

	_, records := readSpool(t, rec.spoolDir)
	if len(records) != 2 {
		t.Fatalf("records = %d, want 2", len(records))
	}
	toolAttrs := attrMap(records[0])
	args, ok := toolAttrs["gen_ai.tool.call.arguments"].(string)
	if !ok {
		t.Fatalf("content level must carry tool arguments")
	}
	if !strings.Contains(args, `"title":"hi"`) {
		t.Errorf("arguments lost benign content: %s", args)
	}
	if strings.Contains(args, "sk-abcdef1234567890") {
		t.Errorf("built-in redaction must scrub token-shaped values from content: %s", args)
	}
	if toolAttrs["agenthooks.session.cwd"] != "/work/repo" {
		t.Errorf("cwd rides at content level, got %v", toolAttrs["agenthooks.session.cwd"])
	}

	body := records[1].GetBody().GetStringValue()
	if !strings.Contains(body, "deploy with") {
		t.Errorf("prompt text must ride the body at content level: %q", body)
	}
	if strings.Contains(body, "supersecret") {
		t.Errorf("secret-named env assignment must be scrubbed from the body: %q", body)
	}
}

func TestUserRedactorRunsAfterBuiltinRedaction(t *testing.T) {
	var sawKeys []string
	rec := newTestRecorder(t, func(cfg *Config) {
		cfg.Capture = CaptureContent
		cfg.Redactor = func(key, value string) string {
			sawKeys = append(sawKeys, key)
			if key == "session.id" {
				return "REDACTED-SESSION"
			}
			return strings.ReplaceAll(value, "hi", "**")
		}
	})
	if err := rec.RecordHook(toolPreRecord()); err != nil {
		t.Fatalf("RecordHook: %v", err)
	}
	_, records := readSpool(t, rec.spoolDir)
	attrs := attrMap(records[0])
	if attrs["session.id"] != "REDACTED-SESSION" {
		t.Errorf("Redactor must rewrite attribute values: %v", attrs["session.id"])
	}
	if args, _ := attrs["gen_ai.tool.call.arguments"].(string); strings.Contains(args, "hi") {
		t.Errorf("Redactor must see content values: %s", args)
	}
	joined := strings.Join(sawKeys, ",")
	if !strings.Contains(joined, "body") {
		t.Errorf("Redactor must see the body (key \"body\"); saw %s", joined)
	}
}

func TestPromptDigestAtDefaultLevel(t *testing.T) {
	rec := newTestRecorder(t, nil)
	hr := toolPreRecord()
	hr.Kind, hr.NativeName = "prompt.submitted", "UserPromptSubmit"
	hr.Tool = nil
	hr.Prompt = "refactor the auth middleware"
	if err := rec.RecordHook(hr); err != nil {
		t.Fatalf("RecordHook: %v", err)
	}
	_, records := readSpool(t, rec.spoolDir)
	attrs := attrMap(records[0])
	if attrs["agenthooks.prompt.length"] != int64(len(hr.Prompt)) {
		t.Errorf("prompt length = %v", attrs["agenthooks.prompt.length"])
	}
	if attrs["agenthooks.prompt.sha256"] != "61d18f121f92c32678dc7bdf69b23794a67d6247aaf7b33b68459d0dfe061660" {
		t.Errorf("prompt sha256 = %v", attrs["agenthooks.prompt.sha256"])
	}
	if body := records[0].GetBody().GetStringValue(); strings.Contains(body, "refactor") {
		t.Errorf("prompt text must not spool at the default level: %q", body)
	}
	// Non-tool events trace per session (gram rule 3).
	if got := records[0].GetTraceId(); len(got) != 16 {
		t.Fatalf("trace id missing")
	}
}

func TestRedactURL(t *testing.T) {
	tests := []struct{ in, want string }{
		{"https://user:pass@host.example.com/path", "https://host.example.com/path"},
		// url.Values.Encode percent-encodes the mask, matching the relay
		// implementation this is ported from.
		{"https://host.example.com/sse?api_key=abc&x=1", "https://host.example.com/sse?api_key=%2A%2A%2A&x=1"},
		{"https://host.example.com/p?signature=zzz", "https://host.example.com/p?signature=%2A%2A%2A"},
		{"https://host.example.com/p#frag", "https://host.example.com/p"},
		// Unparseable URLs could hide credentials anywhere: dropped whole.
		{"https://u:p@host/%zz", "***"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := redactURL(tt.in); got != tt.want {
			t.Errorf("redactURL(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestRedactCommand(t *testing.T) {
	tests := []struct{ in, want string }{
		{"npx server --api-key=abc123", "npx server --api-key=***"},
		{"npx server --token abc123", "npx server --token ***"},
		{"OPENAI_API_KEY=sk-123 npx server", "OPENAI_API_KEY=*** npx server"},
		{`curl -H "Authorization: Bearer abc.def" https://api.example.com`, "curl -H Authorization: Bearer *** https://api.example.com"},
		{"npx mcp-remote https://u:p@srv.example.com/mcp", "npx mcp-remote https://srv.example.com/mcp"},
		{"npx server ghp_0123456789abcdef", "npx server ***"},
	}
	for _, tt := range tests {
		if got := redactCommand(tt.in); got != tt.want {
			t.Errorf("redactCommand(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestRedactContent(t *testing.T) {
	tests := []struct{ in, want string }{
		{"use sk-abcdef1234567890 for auth", "use *** for auth"},
		{"push with ghp_0123456789abcdef now", "push with *** now"},
		{"MY_API_KEY=hunter2 ./run", "MY_API_KEY=*** ./run"},
		{"see https://user:pw@host.example.com/x", "see https://***@host.example.com/x"},
		{"Authorization: Bearer abc12345678", "Authorization: Bearer ***"},
		{"plain text stays intact", "plain text stays intact"},
	}
	for _, tt := range tests {
		if got := redactContent(tt.in); got != tt.want {
			t.Errorf("redactContent(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
