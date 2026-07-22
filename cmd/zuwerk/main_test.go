package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersionIsJSON(t *testing.T) {
	out, errOut, code := execute(t, []string{"version"}, "", nil, http.DefaultClient)
	assertSuccess(t, out, errOut, code)
	assertJSONEqual(t, out, `{"version":"0.0.1"}`)
}

func TestAuthAcceptCanonicalFormSavesConfigWithoutLeakingToken(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method=%s", r.Method)
		}
		assertJSONRequest(t, r, `{"name":"Robot"}`)
		fmt.Fprintf(w, `{"api_token":"secret-token","server_url":%q,"user":{"id":1,"name":"Robot","kind":"agent"}}`, server.URL+"/")
	}))
	defer server.Close()
	dir := t.TempDir()
	out, errOut, code := execute(t, []string{"auth", "accept", server.URL, "--name", "Robot"}, dir, nil, server.Client())
	assertSuccess(t, out, errOut, code)
	if strings.Contains(out+errOut, "secret-token") {
		t.Fatalf("token leaked: %q %q", out, errOut)
	}
	assertJSONEqual(t, out, fmt.Sprintf(`{"server_url":%q,"user":{"id":1,"name":"Robot","kind":"agent"}}`, server.URL))
	info, err := os.Stat(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%o", info.Mode().Perm())
	}
}

func TestAuthAcceptRejectsAConfigurationServerOnAnotherOrigin(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"api_token":"secret-token","server_url":"https://attacker.example","user":{"id":1,"name":"Robot","kind":"agent"}}`)
	}))
	defer server.Close()
	dir := t.TempDir()
	out, errOut, code := execute(t, []string{"auth", "accept", server.URL, "--name", "Robot"}, dir, nil, server.Client())
	if code == 0 || out != "" || !strings.Contains(errOut, "origin") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errOut)
	}
	if _, err := os.Stat(filepath.Join(dir, "config.json")); !os.IsNotExist(err) {
		t.Fatalf("configuration should not exist: %v", err)
	}
}

func TestCanonicalResourceCommands(t *testing.T) {
	tests := []struct {
		name                         string
		args                         []string
		method, path, body, response string
	}{
		{"projects list", []string{"projects", "list"}, "GET", "/api/projects", "", `[{"id":7,"name":"One"}]`},
		{"projects show", []string{"projects", "show", "7"}, "GET", "/api/projects/7", "", `{"id":7,"name":"One"}`},
		{"messages list", []string{"messages", "list", "--project", "7"}, "GET", "/api/projects/7/messages", "", `[{"id":2,"body":"Hi"}]`},
		{"messages create", []string{"messages", "create", "--project", "7", "--body", "Ship it"}, "POST", "/api/projects/7/messages", `{"body":"Ship it"}`, `{"id":3,"body":"Ship it"}`},
		{"messages create for event", []string{"messages", "create", "--project", "7", "--body", "Ship it", "--event", "event-123"}, "POST", "/api/projects/7/messages", `{"body":"Ship it","event_id":"event-123"}`, `{"id":3,"body":"Ship it"}`},
		{"todos list", []string{"todos", "list", "--project", "7"}, "GET", "/api/projects/7/todos", "", `[{"id":4,"title":"Test"}]`},
		{"todos show", []string{"todos", "show", "4", "--project", "7"}, "GET", "/api/projects/7/todos/4", "", `{"id":4,"title":"Test"}`},
		{"todos create", []string{"todos", "create", "--project", "7", "--title", "Test", "--description", "Details"}, "POST", "/api/projects/7/todos", `{"description":"Details","title":"Test"}`, `{"id":4,"title":"Test"}`},
		{"todos update", []string{"todos", "update", "4", "--project", "7", "--title", "Done", "--description", "All done", "--status", "completed"}, "PATCH", "/api/projects/7/todos/4", `{"description":"All done","status":"completed","title":"Done"}`, `{"id":4,"status":"completed"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := apiServer(t, tt.method, tt.path, tt.body, tt.response)
			defer server.Close()
			out, errOut, code := execute(t, tt.args, writeTestConfig(t, server.URL, "secret-token"), nil, server.Client())
			assertSuccess(t, out, errOut, code)
			assertJSONEqual(t, out, tt.response)
		})
	}
}

func TestDashReadsBoundedStdin(t *testing.T) {
	tests := []struct {
		args []string
		body string
	}{
		{[]string{"messages", "create", "--project", "2", "--body", "-"}, `{"body":"from stdin"}`},
		{[]string{"todos", "create", "--project", "2", "--title", "T", "--description", "-"}, `{"description":"from stdin","title":"T"}`},
		{[]string{"todos", "update", "9", "--project", "2", "--description", "-"}, `{"description":"from stdin"}`},
	}
	for _, tt := range tests {
		path := "/api/projects/2/messages"
		method := "POST"
		if tt.args[0] == "todos" && tt.args[1] == "create" {
			path = "/api/projects/2/todos"
		}
		if tt.args[1] == "update" {
			path = "/api/projects/2/todos/9"
			method = "PATCH"
		}
		server := apiServer(t, method, path, tt.body, `{"ok":true}`)
		out, errOut, code := execute(t, tt.args, writeTestConfig(t, server.URL, "secret-token"), strings.NewReader("from stdin"), server.Client())
		server.Close()
		assertSuccess(t, out, errOut, code)
	}
	tooLarge := strings.NewReader(strings.Repeat("x", maxInputBytes+1))
	_, errOut, code := execute(t, []string{"messages", "create", "--project", "2", "--body", "-"}, t.TempDir(), tooLarge, http.DefaultClient)
	if code == 0 || !strings.Contains(errOut, "too large") {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
}

func TestAgentStatusIsJSONOnly(t *testing.T) {
	for _, tt := range []struct {
		args []string
		body string
	}{
		{[]string{"agent", "status", "working", "--label", "Reviewing"}, `{"label":"Reviewing","status":"working"}`},
		{[]string{"agent", "status", "idle"}, `{"status":"idle"}`},
	} {
		server := apiServer(t, "POST", "/api/agent/status", tt.body, tt.body)
		defer server.Close()
		out, errOut, code := execute(t, tt.args, writeTestConfig(t, server.URL, "secret-token"), nil, server.Client())
		assertSuccess(t, out, errOut, code)
		assertJSONEqual(t, out, tt.body)
	}
}

func TestStrictArgumentsRejectLegacyUnknownDuplicateAndInvalidIDs(t *testing.T) {
	cases := [][]string{
		{"auth", "accept", "--name", "Robot", "https://example.test/i"},
		{"messages", "post", "old"}, {"messages", "stream", "create"}, {"messages", "list"},
		{"messages", "list", "--project", "0"}, {"projects", "show", "-1"}, {"todos", "show", "abc", "--project", "1"}, {"todos", "show", "1"},
		{"messages", "list", "--project", "1", "--project", "2"},
		{"messages", "list", "--project", "1", "--json"},
		{"todos", "create", "--project", "1", "--title", "a", "--title", "b"},
		{"todos", "update", "1"}, {"todos", "update", "1", "--project", "1", "--status", "closed"},
		{"agent", "status", "idle", "--label", "x"}, {"agent", "status", "working", "--label", "a", "--label", "b"},
	}
	for _, args := range cases {
		_, errOut, code := execute(t, args, t.TempDir(), nil, http.DefaultClient)
		if code == 0 || !strings.Contains(errOut, "usage:") {
			t.Fatalf("args=%v code=%d stderr=%q", args, code, errOut)
		}
	}
}

func TestResponseLimitMalformedJSONAndHTTPError(t *testing.T) {
	responses := []struct {
		status int
		body   string
	}{{200, strings.Repeat("x", maxResponseBytes+1)}, {200, `{bad`}, {401, `{"token":"secret-token"}`}}
	for _, rr := range responses {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(rr.status); io.WriteString(w, rr.body) }))
		out, errOut, code := execute(t, []string{"projects", "list"}, writeTestConfig(t, server.URL, "secret-token"), nil, server.Client())
		server.Close()
		if code == 0 || strings.Contains(out+errOut, "secret-token") {
			t.Fatalf("code=%d out=%q err=%q", code, out, errOut)
		}
	}
}

func apiServer(t *testing.T, method, path, expectedBody, response string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method || r.URL.Path != path || r.URL.RawQuery != "" {
			t.Errorf("request=%s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
		if r.Header.Get("Authorization") != "Bearer secret-token" {
			t.Errorf("authorization=%q", r.Header.Get("Authorization"))
		}
		if expectedBody != "" {
			assertJSONRequest(t, r, expectedBody)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, response)
	}))
}
func assertJSONRequest(t *testing.T, r *http.Request, want string) {
	t.Helper()
	data, _ := io.ReadAll(r.Body)
	assertJSONEqual(t, string(data), want)
}
func assertJSONEqual(t *testing.T, got, want string) {
	t.Helper()
	var g, w any
	if json.Unmarshal([]byte(got), &g) != nil || json.Unmarshal([]byte(want), &w) != nil || !objectsEqual(g, w) {
		t.Fatalf("JSON got=%q want=%q", got, want)
	}
}
func objectsEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}
func assertSuccess(t *testing.T, out, errOut string, code int) {
	t.Helper()
	if code != 0 || errOut != "" || !json.Valid([]byte(out)) {
		t.Fatalf("code=%d out=%q err=%q", code, out, errOut)
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
func execute(t *testing.T, args []string, configDir string, stdin io.Reader, client *http.Client) (string, string, int) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := runWithOptions(args, &out, &errOut, options{configDir: configDir, stdin: stdin, client: client})
	return out.String(), errOut.String(), code
}
