package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const version = "0.0.1"

const (
	agentStatusPath       = "/api/agent/status"
	messageStreamsPath    = "/api/messages/streams"
	messageStreamPathBase = "/api/messages/"
)

type options struct {
	configDir string
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

type message struct {
	Body string `json:"body"`
	User struct {
		Name string `json:"name"`
	} `json:"user"`
}

func main() {
	dir := os.Getenv("ZUWERK_CONFIG_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintln(os.Stderr, "error: cannot determine the configuration directory")
			os.Exit(1)
		}
		dir = filepath.Join(home, ".config", "zuwerk")
	}
	os.Exit(runWithOptions(os.Args[1:], os.Stdout, os.Stderr, options{configDir: dir, client: http.DefaultClient}))
}

func run(args []string, stdout, stderr io.Writer) int {
	dir := os.Getenv("ZUWERK_CONFIG_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintln(stderr, "error: cannot determine the configuration directory")
			return 1
		}
		dir = filepath.Join(home, ".config", "zuwerk")
	}
	return runWithOptions(args, stdout, stderr, options{configDir: dir, client: http.DefaultClient})
}

func runWithOptions(args []string, stdout, stderr io.Writer, opts options) int {
	if opts.client == nil {
		opts.client = http.DefaultClient
	}
	var err error
	switch {
	case len(args) == 1 && args[0] == "version":
		fmt.Fprintf(stdout, "zuwerk %s\n", version)
		return 0
	case len(args) >= 2 && args[0] == "auth" && args[1] == "accept":
		err = acceptInvitation(args[2:], stdout, opts)
	case len(args) >= 2 && args[0] == "messages" && args[1] == "list":
		err = listMessages(args[2:], stdout, opts)
	case len(args) >= 2 && args[0] == "messages" && args[1] == "post":
		err = postMessage(args[2:], stdout, opts)
	case len(args) >= 3 && args[0] == "agent" && args[1] == "status":
		err = setAgentStatus(args[2], args[3:], stdout, opts)
	case len(args) >= 3 && args[0] == "messages" && args[1] == "stream":
		err = streamMessage(args[2], args[3:], stdout, opts)
	default:
		fmt.Fprintln(stderr, "usage: zuwerk <agent status|auth accept|messages list|messages post|messages stream|version>")
		return 2
	}
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	return 0
}

func setAgentStatus(status string, args []string, stdout io.Writer, opts options) error {
	jsonOutput := false
	label := ""
	for len(args) > 0 {
		switch {
		case args[0] == "--json" && !jsonOutput:
			jsonOutput, args = true, args[1:]
		case args[0] == "--label" && len(args) >= 2 && label == "" && strings.TrimSpace(args[1]) != "":
			label, args = args[1], args[2:]
		default:
			return statusUsage(status)
		}
	}
	if (status != "working" && status != "idle") || (status == "idle" && label != "") {
		return statusUsage(status)
	}
	payload := map[string]string{"status": status}
	if label != "" {
		payload["label"] = label
	}
	data, _ := json.Marshal(payload)
	response, err := authenticatedRequest(opts, http.MethodPost, agentStatusPath, data)
	if err != nil {
		return fmt.Errorf("agent status request failed: %w", err)
	}
	if !json.Valid(response) {
		return errors.New("agent status response is malformed")
	}
	if jsonOutput {
		writeJSON(stdout, response)
	} else {
		fmt.Fprintf(stdout, "Agent status set to %s.\n", status)
	}
	return nil
}

func statusUsage(status string) error {
	if status == "working" {
		return errors.New("usage: zuwerk agent status working [--label TEXT] [--json]")
	}
	return errors.New("usage: zuwerk agent status idle [--json]")
}

func streamMessage(action string, args []string, stdout io.Writer, opts options) error {
	positional := map[string]int{"create": 0, "append": 2, "finish": 1}
	want, ok := positional[action]
	if !ok {
		return streamUsage(action)
	}
	jsonOutput, err := parseJSONFlag("messages stream "+action, args, want)
	if err != nil {
		return streamUsage(action)
	}
	values := make([]string, 0, want)
	for _, arg := range args {
		if arg != "--json" {
			values = append(values, arg)
		}
	}
	var path string
	var payload []byte
	if action == "create" {
		path, payload = messageStreamsPath, []byte(`{}`)
	} else {
		id := values[0]
		parsedID, parseErr := strconv.ParseUint(id, 10, 64)
		if parseErr != nil || parsedID == 0 {
			return streamUsage(action)
		}
		path = messageStreamPathBase + id + "/stream"
		request := map[string]string{"action": action}
		if action == "append" {
			request["chunk"] = values[1]
		}
		payload, _ = json.Marshal(request)
	}
	method := http.MethodPatch
	if action == "create" {
		method = http.MethodPost
	}
	response, err := authenticatedRequest(opts, method, path, payload)
	if err != nil {
		return fmt.Errorf("message stream %s request failed: %w", action, err)
	}
	var result struct {
		ID uint64 `json:"id"`
	}
	if err := json.Unmarshal(response, &result); err != nil || result.ID == 0 {
		return fmt.Errorf("message stream %s response is malformed or incomplete", action)
	}
	if jsonOutput {
		writeJSON(stdout, response)
	} else if action == "create" {
		fmt.Fprintln(stdout, result.ID)
	} else if action == "append" {
		fmt.Fprintf(stdout, "Chunk appended to message %d.\n", result.ID)
	} else {
		fmt.Fprintf(stdout, "Message %d finished.\n", result.ID)
	}
	return nil
}

func streamUsage(action string) error {
	switch action {
	case "create":
		return errors.New("usage: zuwerk messages stream create [--json]")
	case "append":
		return errors.New("usage: zuwerk messages stream append <message-id> <chunk> [--json]")
	default:
		return errors.New("usage: zuwerk messages stream finish <message-id> [--json]")
	}
}

func authenticatedRequest(opts options, method, path string, payload []byte) ([]byte, error) {
	cfg, err := loadConfig(opts.configDir)
	if err != nil {
		return nil, err
	}
	return doRequest(opts.client, method, cfg.ServerURL+path, cfg.APIToken, payload)
}

func acceptInvitation(args []string, stdout io.Writer, opts options) error {
	var invitationURL, name string
	if len(args) == 3 && args[1] == "--name" {
		invitationURL, name = args[0], args[2]
	} else if len(args) == 3 && args[0] == "--name" {
		name, invitationURL = args[1], args[2]
	}
	if invitationURL == "" || strings.TrimSpace(name) == "" {
		return errors.New("usage: zuwerk auth accept <invitation-url> --name <agent-name>")
	}
	payload, _ := json.Marshal(struct {
		Name string `json:"name"`
	}{Name: name})
	response, err := doRequest(opts.client, http.MethodPost, invitationURL, "", payload)
	if err != nil {
		return fmt.Errorf("invitation request failed: %w", err)
	}
	var accepted invitationResponse
	if err := json.Unmarshal(response, &accepted); err != nil || accepted.APIToken == "" || accepted.ServerURL == "" || accepted.User.ID == 0 || accepted.User.Name == "" || accepted.User.Kind == "" {
		return errors.New("invitation response is malformed or incomplete")
	}
	if err := saveConfig(opts.configDir, config{ServerURL: strings.TrimRight(accepted.ServerURL, "/"), APIToken: accepted.APIToken}); err != nil {
		return fmt.Errorf("cannot save configuration: %w", err)
	}
	fmt.Fprintf(stdout, "Invitation accepted for %s.\n", accepted.User.Name)
	return nil
}

func listMessages(args []string, stdout io.Writer, opts options) error {
	jsonOutput, err := parseJSONFlag("messages list", args, 0)
	if err != nil {
		return err
	}
	cfg, err := loadConfig(opts.configDir)
	if err != nil {
		return err
	}
	response, err := doRequest(opts.client, http.MethodGet, cfg.ServerURL+"/api/messages", cfg.APIToken, nil)
	if err != nil {
		return fmt.Errorf("message list request failed: %w", err)
	}
	var messages []message
	if err := json.Unmarshal(response, &messages); err != nil {
		return errors.New("message list response is malformed")
	}
	if jsonOutput {
		writeJSON(stdout, response)
		return nil
	}
	for _, item := range messages {
		if item.User.Name == "" {
			return errors.New("message list response is incomplete")
		}
		fmt.Fprintf(stdout, "%s: %s\n", item.User.Name, item.Body)
	}
	return nil
}

func postMessage(args []string, stdout io.Writer, opts options) error {
	jsonOutput, err := parseJSONFlag("messages post", args, 1)
	if err != nil {
		return err
	}
	body := ""
	for _, arg := range args {
		if arg != "--json" {
			body = arg
		}
	}
	cfg, err := loadConfig(opts.configDir)
	if err != nil {
		return err
	}
	payload, _ := json.Marshal(struct {
		Body string `json:"body"`
	}{Body: body})
	response, err := doRequest(opts.client, http.MethodPost, cfg.ServerURL+"/api/messages", cfg.APIToken, payload)
	if err != nil {
		return fmt.Errorf("message post request failed: %w", err)
	}
	if !json.Valid(response) {
		return errors.New("message post response is malformed")
	}
	if jsonOutput {
		writeJSON(stdout, response)
	} else {
		fmt.Fprintln(stdout, "Message posted.")
	}
	return nil
}

func parseJSONFlag(command string, args []string, positional int) (bool, error) {
	jsonOutput := false
	count := 0
	for _, arg := range args {
		if arg == "--json" {
			if jsonOutput {
				return false, fmt.Errorf("usage: zuwerk %s", command)
			}
			jsonOutput = true
		} else {
			count++
		}
	}
	if count != positional {
		return false, fmt.Errorf("usage: zuwerk %s", command)
	}
	return jsonOutput, nil
}

func doRequest(client *http.Client, method, url, token string, payload []byte) ([]byte, error) {
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
	response, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return nil, errors.New("could not read server response")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("server returned HTTP %d", resp.StatusCode)
	}
	return response, nil
}

func saveConfig(dir string, cfg config) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return errors.New("cannot create configuration directory")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return errors.New("cannot secure configuration directory")
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return errors.New("cannot encode configuration")
	}
	data = append(data, '\n')
	path := filepath.Join(dir, "config.json")
	tmp, err := os.CreateTemp(dir, ".config-*")
	if err != nil {
		return errors.New("cannot create configuration file")
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return errors.New("cannot secure configuration file")
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return errors.New("cannot write configuration file")
	}
	if err := tmp.Close(); err != nil {
		return errors.New("cannot write configuration file")
	}
	if err := os.Rename(tmpName, path); err != nil {
		return errors.New("cannot replace configuration file")
	}
	return os.Chmod(path, 0o600)
}

func loadConfig(dir string) (config, error) {
	var cfg config
	data, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		return cfg, errors.New("configuration not found; accept an invitation first")
	}
	if err := json.Unmarshal(data, &cfg); err != nil || cfg.ServerURL == "" || cfg.APIToken == "" {
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
