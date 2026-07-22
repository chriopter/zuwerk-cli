package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const version = "0.0.1"
const maxInputBytes = 1 << 20
const maxResponseBytes = 10 << 20
const requestTimeout = 30 * time.Second

var defaultHTTPClient = &http.Client{Timeout: requestTimeout}

type options struct {
	configDir string
	stdin     io.Reader
	client    *http.Client
}
type config struct {
	ServerURL string `json:"server_url"`
	APIToken  string `json:"api_token"`
}
type invitationResponse struct {
	APIToken  string `json:"api_token"`
	ServerURL string `json:"server_url"`
	User      struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
		Kind string `json:"kind"`
	} `json:"user"`
}

func main() {
	dir := os.Getenv("ZUWERK_CONFIG_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintln(os.Stderr, "error: cannot determine configuration directory")
			os.Exit(1)
		}
		dir = filepath.Join(home, ".config", "zuwerk")
	}
	os.Exit(runWithOptions(os.Args[1:], os.Stdout, os.Stderr, options{configDir: dir, stdin: os.Stdin, client: defaultHTTPClient}))
}

func run(args []string, stdout, stderr io.Writer) int {
	dir := os.Getenv("ZUWERK_CONFIG_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintln(stderr, "error: cannot determine configuration directory")
			return 1
		}
		dir = filepath.Join(home, ".config", "zuwerk")
	}
	return runWithOptions(args, stdout, stderr, options{configDir: dir, stdin: os.Stdin, client: defaultHTTPClient})
}

func runWithOptions(args []string, stdout, stderr io.Writer, opts options) int {
	if opts.client == nil {
		opts.client = defaultHTTPClient
	}
	if opts.stdin == nil {
		opts.stdin = strings.NewReader("")
	}
	var data []byte
	var err error
	switch {
	case len(args) == 1 && args[0] == "version":
		data, _ = json.Marshal(map[string]string{"version": version})
	case len(args) >= 2 && args[0] == "auth" && args[1] == "accept":
		data, err = acceptInvitation(args[2:], opts)
	case len(args) >= 2 && args[0] == "projects" && args[1] == "list":
		data, err = simpleResource(args[2:], opts, http.MethodGet, "/api/projects", "projects list")
	case len(args) >= 2 && args[0] == "projects" && args[1] == "show":
		data, err = showResource(args[2:], opts, "/api/projects/", "projects show")
	case len(args) >= 2 && args[0] == "messages" && args[1] == "list":
		data, err = projectList(args[2:], opts, "messages")
	case len(args) >= 2 && args[0] == "messages" && args[1] == "create":
		data, err = createMessage(args[2:], opts)
	case len(args) >= 2 && args[0] == "todos" && args[1] == "list":
		data, err = projectList(args[2:], opts, "todos")
	case len(args) >= 2 && args[0] == "todos" && args[1] == "show":
		data, err = showTodo(args[2:], opts)
	case len(args) >= 2 && args[0] == "todos" && args[1] == "create":
		data, err = createTodo(args[2:], opts)
	case len(args) >= 2 && args[0] == "todos" && args[1] == "update":
		data, err = updateTodo(args[2:], opts)
	case len(args) >= 3 && args[0] == "agent" && args[1] == "status":
		data, err = setAgentStatus(args[2:], opts)
	default:
		fmt.Fprintln(stderr, "usage: zuwerk <agent status|auth accept|messages|projects|todos|version>")
		return 2
	}
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	writeJSON(stdout, data)
	return 0
}

func simpleResource(args []string, opts options, method, path, usage string) ([]byte, error) {
	if len(args) != 0 {
		return nil, usageError(usage)
	}
	return authenticatedRequest(opts, method, path, nil)
}
func showResource(args []string, opts options, base, usage string) ([]byte, error) {
	if len(args) != 1 || !positiveID(args[0]) {
		return nil, usageError(usage + " <id>")
	}
	return authenticatedRequest(opts, http.MethodGet, base+args[0], nil)
}
func projectList(args []string, opts options, resource string) ([]byte, error) {
	f, err := parseFlags(args, map[string]bool{"--project": true})
	if err != nil || !positiveID(f["--project"]) {
		return nil, usageError(resource + " list --project <id>")
	}
	return authenticatedRequest(opts, http.MethodGet, "/api/projects/"+f["--project"]+"/"+resource, nil)
}

func createMessage(args []string, opts options) ([]byte, error) {
	f, err := parseFlags(args, map[string]bool{"--project": true, "--body": true, "--event": true})
	if err != nil || !positiveID(f["--project"]) || f["--body"] == "" {
		return nil, usageError("messages create --project <id> --body <text|-> [--event <id>]")
	}
	body, err := textValue(f["--body"], opts.stdin)
	if err != nil {
		return nil, err
	}
	payloadFields := map[string]string{"body": body}
	if eventID, ok := f["--event"]; ok {
		if strings.TrimSpace(eventID) == "" || len(eventID) > 200 {
			return nil, usageError("messages create --project <id> --body <text|-> [--event <id>]")
		}
		payloadFields["event_id"] = eventID
	}
	payload, _ := json.Marshal(payloadFields)
	return authenticatedRequest(opts, http.MethodPost, "/api/projects/"+f["--project"]+"/messages", payload)
}
func createTodo(args []string, opts options) ([]byte, error) {
	f, err := parseFlags(args, map[string]bool{"--project": true, "--title": true, "--description": true})
	if err != nil || !positiveID(f["--project"]) || strings.TrimSpace(f["--title"]) == "" {
		return nil, usageError("todos create --project <id> --title <title> [--description <text|->]")
	}
	if len(f["--title"]) > maxInputBytes {
		return nil, errors.New("input is too large")
	}
	p := map[string]string{"title": f["--title"]}
	if v, ok := f["--description"]; ok {
		v, err = textValue(v, opts.stdin)
		if err != nil {
			return nil, err
		}
		p["description"] = v
	}
	payload, _ := json.Marshal(p)
	return authenticatedRequest(opts, http.MethodPost, "/api/projects/"+f["--project"]+"/todos", payload)
}
func showTodo(args []string, opts options) ([]byte, error) {
	if len(args) < 1 || !positiveID(args[0]) {
		return nil, usageError("todos show <id> --project <id>")
	}
	f, err := parseFlags(args[1:], map[string]bool{"--project": true})
	if err != nil || !positiveID(f["--project"]) {
		return nil, usageError("todos show <id> --project <id>")
	}
	return authenticatedRequest(opts, http.MethodGet, "/api/projects/"+f["--project"]+"/todos/"+args[0], nil)
}
func updateTodo(args []string, opts options) ([]byte, error) {
	if len(args) < 1 || !positiveID(args[0]) {
		return nil, usageError("todos update <id> --project <id> [--title ...] [--description ...] [--status open|completed]")
	}
	f, err := parseFlags(args[1:], map[string]bool{"--project": true, "--title": true, "--description": true, "--status": true})
	projectID := f["--project"]
	if err != nil || !positiveID(projectID) || len(f) == 1 {
		return nil, usageError("todos update <id> --project <id> [--title ...] [--description ...] [--status open|completed]")
	}
	if s, ok := f["--status"]; ok && s != "open" && s != "completed" {
		return nil, usageError("todos update <id> --project <id> [--title ...] [--description ...] [--status open|completed]")
	}
	if t, ok := f["--title"]; ok && (strings.TrimSpace(t) == "" || len(t) > maxInputBytes) {
		return nil, usageError("todos update <id> --project <id> [--title ...] [--description ...] [--status open|completed]")
	}
	p := map[string]string{}
	for k, v := range f {
		key := strings.TrimPrefix(k, "--")
		if key == "project" {
			continue
		}
		if key == "description" {
			v, err = textValue(v, opts.stdin)
			if err != nil {
				return nil, err
			}
		}
		p[key] = v
	}
	payload, _ := json.Marshal(p)
	return authenticatedRequest(opts, http.MethodPatch, "/api/projects/"+projectID+"/todos/"+args[0], payload)
}
func setAgentStatus(args []string, opts options) ([]byte, error) {
	if len(args) < 1 {
		return nil, usageError("agent status <working|idle>")
	}
	status := args[0]
	allowed := map[string]bool{}
	if status == "working" {
		allowed["--label"] = true
	}
	f, err := parseFlags(args[1:], allowed)
	if err != nil || (status != "working" && status != "idle") {
		return nil, statusUsage(status)
	}
	p := map[string]string{"status": status}
	if label, ok := f["--label"]; ok {
		if strings.TrimSpace(label) == "" || len(label) > maxInputBytes {
			return nil, statusUsage(status)
		}
		p["label"] = label
	}
	payload, _ := json.Marshal(p)
	return authenticatedRequest(opts, http.MethodPost, "/api/agent/status", payload)
}
func statusUsage(status string) error {
	if status == "working" {
		return usageError("agent status working [--label <text>]")
	}
	return usageError("agent status idle")
}

func acceptInvitation(args []string, opts options) ([]byte, error) {
	if len(args) != 3 || args[1] != "--name" || strings.TrimSpace(args[0]) == "" || strings.TrimSpace(args[2]) == "" || len(args[2]) > maxInputBytes {
		return nil, usageError("auth accept <invitation-url> --name <name>")
	}
	payload, _ := json.Marshal(map[string]string{"name": args[2]})
	response, err := doRequest(opts.client, http.MethodPost, args[0], "", payload)
	if err != nil {
		return nil, fmt.Errorf("invitation request failed: %w", err)
	}
	var accepted invitationResponse
	if json.Unmarshal(response, &accepted) != nil || accepted.APIToken == "" || accepted.ServerURL == "" || accepted.User.ID <= 0 || accepted.User.Name == "" || accepted.User.Kind == "" {
		return nil, errors.New("invitation response is malformed or incomplete")
	}
	accepted.ServerURL = strings.TrimRight(accepted.ServerURL, "/")
	if !sameOrigin(args[0], accepted.ServerURL) || !safeInvitationServer(accepted.ServerURL) {
		return nil, errors.New("invitation response server URL must be an absolute HTTP(S) URL on the invitation origin")
	}
	if err := saveConfig(opts.configDir, config{accepted.ServerURL, accepted.APIToken}); err != nil {
		return nil, fmt.Errorf("cannot save configuration: %w", err)
	}
	return json.Marshal(struct {
		ServerURL string `json:"server_url"`
		User      any    `json:"user"`
	}{accepted.ServerURL, accepted.User})
}

func sameOrigin(invitationURL, serverURL string) bool {
	invitation, invitationErr := url.Parse(invitationURL)
	server, serverErr := url.Parse(serverURL)
	if invitationErr != nil || serverErr != nil || invitation.User != nil || server.User != nil {
		return false
	}
	if (invitation.Scheme != "http" && invitation.Scheme != "https") || (server.Scheme != "http" && server.Scheme != "https") {
		return false
	}
	return strings.EqualFold(invitation.Scheme, server.Scheme) && strings.EqualFold(invitation.Host, server.Host)
}

func safeInvitationServer(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	return parsed.Scheme == "https" || parsed.Scheme == "http"
}

func parseFlags(args []string, allowed map[string]bool) (map[string]string, error) {
	r := map[string]string{}
	for i := 0; i < len(args); i++ {
		k := args[i]
		if !allowed[k] || i+1 >= len(args) {
			return nil, errors.New("invalid arguments")
		}
		if _, exists := r[k]; exists {
			return nil, errors.New("duplicate flag")
		}
		i++
		if strings.HasPrefix(args[i], "--") {
			return nil, errors.New("missing flag value")
		}
		r[k] = args[i]
	}
	return r, nil
}
func positiveID(s string) bool { n, err := strconv.ParseUint(s, 10, 64); return err == nil && n > 0 }
func textValue(value string, stdin io.Reader) (string, error) {
	if value != "-" {
		if len(value) > maxInputBytes {
			return "", errors.New("input is too large")
		}
		return value, nil
	}
	data, err := io.ReadAll(io.LimitReader(stdin, maxInputBytes+1))
	if err != nil {
		return "", errors.New("cannot read stdin")
	}
	if len(data) > maxInputBytes {
		return "", errors.New("stdin input is too large")
	}
	return string(data), nil
}
func usageError(s string) error { return fmt.Errorf("usage: zuwerk %s", s) }

func authenticatedRequest(opts options, method, path string, payload []byte) ([]byte, error) {
	cfg, err := loadConfig(opts.configDir)
	if err != nil {
		return nil, err
	}
	return doRequest(opts.client, method, cfg.ServerURL+path, cfg.APIToken, payload)
}
func doRequest(client *http.Client, method, url, token string, payload []byte) ([]byte, error) {
	if len(payload) > maxInputBytes+1024 {
		return nil, errors.New("request is too large")
	}
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, errors.New("invalid request URL")
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("could not contact server")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, errors.New("could not read server response")
	}
	if len(data) > maxResponseBytes {
		return nil, errors.New("server response is too large")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("server returned HTTP %d", resp.StatusCode)
	}
	if !json.Valid(data) {
		return nil, errors.New("server response is malformed")
	}
	return data, nil
}
func saveConfig(dir string, cfg config) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return errors.New("cannot create configuration directory")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return errors.New("cannot secure configuration directory")
	}
	data, _ := json.MarshalIndent(cfg, "", "  ")
	data = append(data, '\n')
	tmp, err := os.CreateTemp(dir, ".config-*")
	if err != nil {
		return errors.New("cannot create configuration file")
	}
	name := tmp.Name()
	defer os.Remove(name)
	if tmp.Chmod(0o600) != nil {
		return errors.New("cannot secure configuration file")
	}
	if _, err = tmp.Write(data); err != nil {
		tmp.Close()
		return errors.New("cannot write configuration file")
	}
	if tmp.Close() != nil {
		return errors.New("cannot write configuration file")
	}
	if os.Rename(name, filepath.Join(dir, "config.json")) != nil {
		return errors.New("cannot replace configuration file")
	}
	return os.Chmod(filepath.Join(dir, "config.json"), 0o600)
}
func loadConfig(dir string) (config, error) {
	var cfg config
	data, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		return cfg, errors.New("configuration not found; accept an invitation first")
	}
	if json.Unmarshal(data, &cfg) != nil || cfg.ServerURL == "" || cfg.APIToken == "" {
		return cfg, errors.New("configuration is malformed; accept an invitation again")
	}
	cfg.ServerURL = strings.TrimRight(cfg.ServerURL, "/")
	return cfg, nil
}
func writeJSON(w io.Writer, data []byte) {
	w.Write(data)
	if len(data) == 0 || data[len(data)-1] != '\n' {
		fmt.Fprintln(w)
	}
}
