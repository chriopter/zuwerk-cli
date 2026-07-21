package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersionCommand(t *testing.T) {
	stdout, stderr, code := execute(t, []string{"version"}, t.TempDir(), http.DefaultClient)
	if code != 0 || stdout != "zuwerk 0.0.1\n" || stderr != "" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestAuthAcceptRedeemsInvitationAndSavesSecureConfig(t *testing.T) {
	var gotName string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&struct {
			Name *string `json:"name"`
		}{Name: &gotName}); err != nil {
			t.Errorf("decode request: %v", err)
		}
		fmt.Fprint(w, `{"api_token":"secret-token","server_url":"https://zuwerk.example","user":{"id":1,"name":"Robot","kind":"agent"}}`)
	}))
	defer server.Close()

	dir := t.TempDir()
	stdout, stderr, code := execute(t, []string{"auth", "accept", server.URL, "--name", "Robot"}, dir, server.Client())
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	if gotName != "Robot" {
		t.Fatalf("name = %q", gotName)
	}
	if strings.Contains(stdout, "secret-token") || !strings.Contains(stdout, "Robot") {
		t.Fatalf("unsafe or unclear stdout: %q", stdout)
	}
	configPath := filepath.Join(dir, "config.json")
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("config mode = %o", got)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "{\n  \"server_url\": \"https://zuwerk.example\",\n  \"api_token\": \"secret-token\"\n}\n"; got != want {
		t.Fatalf("config = %q", got)
	}
}

func TestMessagesListUsesBearerAndRendersMessages(t *testing.T) {
	server := authenticatedServer(t, http.MethodGet, "/api/messages", "", `[{"body":"Hello","user":{"name":"Ada"}},{"body":"World","user":{"name":"Lin"}}]`)
	defer server.Close()
	dir := writeTestConfig(t, server.URL, "secret-token")

	stdout, stderr, code := execute(t, []string{"messages", "list"}, dir, server.Client())
	if code != 0 || stderr != "" || stdout != "Ada: Hello\nLin: World\n" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestMessagesListJSONWritesServerResponse(t *testing.T) {
	response := `[{"id":2,"body":"Hello","user":{"name":"Ada"}}]`
	server := authenticatedServer(t, http.MethodGet, "/api/messages", "", response)
	defer server.Close()

	stdout, stderr, code := execute(t, []string{"messages", "list", "--json"}, writeTestConfig(t, server.URL, "secret-token"), server.Client())
	if code != 0 || stderr != "" || stdout != response+"\n" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestMessagesPostSendsBodyAndUsesBearer(t *testing.T) {
	server := authenticatedServer(t, http.MethodPost, "/api/messages", `{"body":"Ship it"}`, `{"id":3,"body":"Ship it"}`)
	defer server.Close()
	dir := writeTestConfig(t, server.URL, "secret-token")

	stdout, stderr, code := execute(t, []string{"messages", "post", "Ship it"}, dir, server.Client())
	if code != 0 || stderr != "" || !strings.Contains(stdout, "posted") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}

	stdout, stderr, code = execute(t, []string{"messages", "post", "Ship it", "--json"}, dir, server.Client())
	if code != 0 || stderr != "" || stdout != "{\"id\":3,\"body\":\"Ship it\"}\n" {
		t.Fatalf("json code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestAgentStatusWorkingSendsOptionalLabel(t *testing.T) {
	tests := []struct {
		name, label, body string
		args              []string
	}{
		{"without label", "", `{"status":"working"}`, []string{"agent", "status", "working"}},
		{"with label", "Deploying", `{"label":"Deploying","status":"working"}`, []string{"agent", "status", "working", "--label", "Deploying"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := authenticatedServer(t, http.MethodPost, "/api/agent/status", tt.body, `{"status":"working","label":"`+tt.label+`"}`)
			defer server.Close()
			stdout, stderr, code := execute(t, tt.args, writeTestConfig(t, server.URL, "secret-token"), server.Client())
			if code != 0 || stderr != "" || stdout != "Agent status set to working.\n" {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
		})
	}
}

func TestAgentStatusIdleSupportsJSON(t *testing.T) {
	response := `{"status":"idle"}`
	server := authenticatedServer(t, http.MethodPost, "/api/agent/status", `{"status":"idle"}`, response)
	defer server.Close()
	stdout, stderr, code := execute(t, []string{"agent", "status", "idle", "--json"}, writeTestConfig(t, server.URL, "secret-token"), server.Client())
	if code != 0 || stderr != "" || stdout != response+"\n" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestMessagesStreamLifecycleUsesExpectedContract(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if got := r.Header.Get("Authorization"); got != "Bearer secret-token" {
			t.Errorf("Authorization = %q", got)
		}
		switch requests {
		case 1:
			if r.Method != http.MethodPost || r.URL.Path != "/api/messages/streams" {
				t.Errorf("create request = %s %s", r.Method, r.URL.Path)
			}
			assertJSONBody(t, r, `{}`)
			fmt.Fprint(w, `{"id":42,"body":"","streaming":true}`)
		case 2:
			if r.Method != http.MethodPatch || r.URL.Path != "/api/messages/42/stream" {
				t.Errorf("append request = %s %s", r.Method, r.URL.Path)
			}
			assertJSONBody(t, r, `{"action":"append","chunk":"hello "}`)
			fmt.Fprint(w, `{"id":42,"body":"hello ","streaming":true}`)
		case 3:
			if r.Method != http.MethodPatch || r.URL.Path != "/api/messages/42/stream" {
				t.Errorf("finish request = %s %s", r.Method, r.URL.Path)
			}
			assertJSONBody(t, r, `{"action":"finish"}`)
			fmt.Fprint(w, `{"id":42,"body":"hello ","streaming":false}`)
		}
	}))
	defer server.Close()
	dir := writeTestConfig(t, server.URL, "secret-token")

	stdout, stderr, code := execute(t, []string{"messages", "stream", "create"}, dir, server.Client())
	if code != 0 || stderr != "" || stdout != "42\n" {
		t.Fatalf("create: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	stdout, stderr, code = execute(t, []string{"messages", "stream", "append", "42", "hello ", "--json"}, dir, server.Client())
	if code != 0 || stderr != "" || stdout != "{\"id\":42,\"body\":\"hello \",\"streaming\":true}\n" {
		t.Fatalf("append: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	stdout, stderr, code = execute(t, []string{"messages", "stream", "finish", "42"}, dir, server.Client())
	if code != 0 || stderr != "" || stdout != "Message 42 finished.\n" {
		t.Fatalf("finish: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestNewCommandsRejectMalformedResponsesAndNeverLeakToken(t *testing.T) {
	for _, args := range [][]string{
		{"agent", "status", "idle"},
		{"messages", "stream", "create"},
		{"messages", "stream", "append", "7", "chunk"},
		{"messages", "stream", "finish", "7"},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{bad`) }))
		stdout, stderr, code := execute(t, args, writeTestConfig(t, server.URL, "secret-token"), server.Client())
		server.Close()
		if code == 0 || stderr == "" || strings.Contains(stdout+stderr, "secret-token") {
			t.Fatalf("args=%v code=%d stdout=%q stderr=%q", args, code, stdout, stderr)
		}
	}
}

func TestStatusAndStreamUsageValidation(t *testing.T) {
	for _, args := range [][]string{
		{"agent", "status", "idle", "--label", "no"},
		{"agent", "status", "working", "--label", ""},
		{"messages", "stream", "append", "bad/id", "chunk"},
		{"messages", "stream", "finish"},
	} {
		_, stderr, code := execute(t, args, t.TempDir(), http.DefaultClient)
		if code != 1 || !strings.Contains(stderr, "usage:") {
			t.Fatalf("args=%v code=%d stderr=%q", args, code, stderr)
		}
	}
}

func TestHTTPAndMalformedResponsesFailWithoutLeakingToken(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		response string
		args     []string
	}{
		{"http error", http.StatusUnauthorized, `{"error":"denied"}`, []string{"messages", "list"}},
		{"malformed messages", http.StatusOK, `{bad`, []string{"messages", "list"}},
		{"malformed invitation", http.StatusOK, `{"api_token":"secret-token"}`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				fmt.Fprint(w, tt.response)
			}))
			defer server.Close()
			dir := writeTestConfig(t, server.URL, "secret-token")
			args := tt.args
			if args == nil {
				args = []string{"auth", "accept", server.URL, "--name", "Robot"}
			}
			stdout, stderr, code := execute(t, args, dir, server.Client())
			if code == 0 || stderr == "" {
				t.Fatalf("expected failure; stdout=%q stderr=%q", stdout, stderr)
			}
			if strings.Contains(stdout+stderr, "secret-token") {
				t.Fatalf("token leaked: stdout=%q stderr=%q", stdout, stderr)
			}
		})
	}
}

func authenticatedServer(t *testing.T, method, path, expectedBody, response string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method || r.URL.Path != path {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret-token" {
			t.Errorf("Authorization = %q", got)
		}
		if expectedBody != "" {
			var body bytes.Buffer
			body.ReadFrom(r.Body)
			if got := strings.TrimSpace(body.String()); got != expectedBody {
				t.Errorf("body = %q", got)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, response)
	}))
}

func assertJSONBody(t *testing.T, r *http.Request, want string) {
	t.Helper()
	var body bytes.Buffer
	body.ReadFrom(r.Body)
	if got := strings.TrimSpace(body.String()); got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

func writeTestConfig(t *testing.T, serverURL, token string) string {
	t.Helper()
	dir := t.TempDir()
	data := fmt.Sprintf(`{"server_url":%q,"api_token":%q}`, serverURL, token)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func execute(t *testing.T, args []string, configDir string, client *http.Client) (string, string, int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := runWithOptions(args, &stdout, &stderr, options{configDir: configDir, client: client})
	return stdout.String(), stderr.String(), code
}
