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
	"strings"
)

const version = "0.0.1"

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
	default:
		fmt.Fprintln(stderr, "usage: zuwerk <auth accept|messages list|messages post|version>")
		return 2
	}
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	return 0
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
