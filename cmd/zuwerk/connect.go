package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/coder/websocket"
)

const (
	maxACPLineBytes     = 10 << 20
	connectorIdentifier = `{"channel":"AgentConnectorChannel"}`
)

type cableCommand struct {
	Command    string `json:"command"`
	Identifier string `json:"identifier"`
	Data       string `json:"data,omitempty"`
}
type connectorPayload struct {
	Type string `json:"type"`
	Line string `json:"line,omitempty"`
}
type cableEnvelope struct {
	Type       string          `json:"type,omitempty"`
	Identifier string          `json:"identifier,omitempty"`
	Message    json.RawMessage `json:"message,omitempty"`
}

type connectorChild interface {
	Stdin() io.WriteCloser
	Stdout() io.ReadCloser
	Wait() error
	Signal(os.Signal) error
	Kill() error
}
type connectorOptions struct {
	startChild        func(context.Context, []string) (connectorChild, error)
	heartbeatInterval time.Duration
	backoff           func(int) time.Duration
	shutdownTimeout   time.Duration
}

type execChild struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
}

func (c *execChild) Stdin() io.WriteCloser { return c.stdin }
func (c *execChild) Stdout() io.ReadCloser { return c.stdout }
func (c *execChild) Wait() error           { return c.cmd.Wait() }
func (c *execChild) Signal(signal os.Signal) error {
	if c.cmd.Process == nil {
		return nil
	}
	return c.cmd.Process.Signal(signal)
}
func (c *execChild) Kill() error {
	if c.cmd.Process == nil {
		return nil
	}
	return c.cmd.Process.Kill()
}

func startExecChild(ctx context.Context, argv []string) (connectorChild, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &execChild{cmd: cmd, stdin: stdin, stdout: stdout}, nil
}

func parseConnectArgs(args []string) ([]string, error) {
	if len(args) < 2 || args[0] != "--" || strings.TrimSpace(args[1]) == "" {
		return nil, usageError("connect -- <adapter> [args...]")
	}
	return append([]string(nil), args[1:]...), nil
}

func cableURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("server URL must be an absolute HTTP(S) URL")
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	default:
		return "", errors.New("server URL must use HTTP(S)")
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/cable"
	return u.String(), nil
}

func validJSONObject(data []byte) bool {
	var object map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&object); err != nil || object == nil {
		return false
	}
	return decoder.Decode(&struct{}{}) == io.EOF
}

func validConnectorIdentifier(identifier string) bool {
	decoder := json.NewDecoder(strings.NewReader(identifier))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') || !decoder.More() {
		return false
	}
	key, err := decoder.Token()
	if err != nil || key != "channel" {
		return false
	}
	var channel string
	if err := decoder.Decode(&channel); err != nil || channel != "AgentConnectorChannel" || decoder.More() {
		return false
	}
	token, err = decoder.Token()
	if err != nil || token != json.Delim('}') {
		return false
	}
	return decoder.Decode(&struct{}{}) == io.EOF
}

func readACPLine(r *bufio.Reader) (string, error) {
	line := make([]byte, 0, 4096)
	for {
		part, err := r.ReadSlice('\n')
		if len(line)+len(part) > maxACPLineBytes {
			return "", errors.New("ACP line is too large")
		}
		line = append(line, part...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil && !(errors.Is(err, io.EOF) && len(line) > 0) {
			return "", err
		}
		break
	}
	line = bytes.TrimSuffix(line, []byte{'\n'})
	line = bytes.TrimSuffix(line, []byte{'\r'})
	if len(line) == 0 || !validJSONObject(line) {
		return "", errors.New("ACP line is malformed")
	}
	return string(line), nil
}

func redactToken(err error, token string) error {
	if err == nil {
		return nil
	}
	message := err.Error()
	if token != "" {
		message = strings.ReplaceAll(message, token, "[REDACTED]")
	}
	if errors.Is(err, errPermanent) {
		return fmt.Errorf("%w: %s", errPermanent, strings.TrimPrefix(message, errPermanent.Error()+": "))
	}
	if errors.Is(err, errChildExited) {
		return fmt.Errorf("%w: %s", errChildExited, strings.TrimPrefix(message, errChildExited.Error()+": "))
	}
	return errors.New(message)
}

var errChildExited = errors.New("adapter exited")
var errPermanent = errors.New("permanent connector error")

type connectionResult struct {
	err     error
	traffic bool
}

func runConnector(ctx context.Context, cfg config, argv []string, opts connectorOptions) (result error) {
	if opts.startChild == nil {
		opts.startChild = startExecChild
	}
	if opts.heartbeatInterval <= 0 {
		opts.heartbeatInterval = 30 * time.Second
	}
	if opts.backoff == nil {
		opts.backoff = reconnectBackoff
	}
	if opts.shutdownTimeout <= 0 {
		opts.shutdownTimeout = 2 * time.Second
	}
	wsURL, err := cableURL(cfg.ServerURL)
	if err != nil {
		return err
	}
	child, err := opts.startChild(ctx, argv)
	if err != nil {
		return fmt.Errorf("start adapter: %w", err)
	}
	workerCtx, stopWorkers := context.WithCancel(context.Background())
	lines := make(chan string, 64)
	stdoutErr := make(chan error, 1)
	childDone := make(chan struct{})
	var childWaitErr error
	go func() {
		r := bufio.NewReaderSize(child.Stdout(), 64*1024)
		for {
			line, readErr := readACPLine(r)
			if readErr != nil {
				if !errors.Is(readErr, io.EOF) {
					select {
					case stdoutErr <- readErr:
					default:
					}
				}
				return
			}
			select {
			case lines <- line:
			case <-workerCtx.Done():
				return
			}
		}
	}()
	go func() {
		childWaitErr = child.Wait()
		close(childDone)
	}()
	defer func() {
		stopWorkers()
		_ = child.Stdin().Close()
		_ = child.Stdout().Close()
		_ = child.Signal(syscall.SIGTERM)
		timer := time.NewTimer(opts.shutdownTimeout)
		defer timer.Stop()
		select {
		case <-childDone:
		case <-timer.C:
			_ = child.Kill()
			select {
			case <-childDone:
			case <-time.After(opts.shutdownTimeout):
			}
		}
	}()

	traffic := false
	for attempt := 0; ; attempt++ {
		outcome := connectOnce(ctx, wsURL, cfg.APIToken, child.Stdin(), lines, stdoutErr, childDone, opts.heartbeatInterval)
		traffic = traffic || outcome.traffic
		if errors.Is(outcome.err, errChildExited) {
			return redactToken(childExitError(childWaitErr), cfg.APIToken)
		}
		if errors.Is(outcome.err, errPermanent) {
			return redactToken(outcome.err, cfg.APIToken)
		}
		if traffic {
			return redactToken(fmt.Errorf("%w: connection lost after ACP traffic", errPermanent), cfg.APIToken)
		}
		if ctx.Err() != nil {
			return nil
		}
		timer := time.NewTimer(opts.backoff(attempt))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-childDone:
			timer.Stop()
			return redactToken(childExitError(childWaitErr), cfg.APIToken)
		case readErr := <-stdoutErr:
			timer.Stop()
			return redactToken(fmt.Errorf("%w: %v", errPermanent, readErr), cfg.APIToken)
		case <-timer.C:
		}
	}
}

func childExitError(err error) error {
	if err == nil {
		return errChildExited
	}
	return fmt.Errorf("%w: %v", errChildExited, err)
}

func connectOnce(ctx context.Context, wsURL, token string, stdin io.Writer, lines <-chan string, stdoutErr <-chan error, childDone <-chan struct{}, heartbeat time.Duration) (out connectionResult) {
	connectionCtx, cancelConnection := context.WithCancel(ctx)
	defer cancelConnection()
	go func() {
		select {
		case <-childDone:
			cancelConnection()
		case <-connectionCtx.Done():
		}
	}()
	defer func() {
		select {
		case <-childDone:
			out.err = errChildExited
		default:
		}
	}()
	headers := http.Header{"Authorization": []string{"Bearer " + token}}
	if parsed, parseErr := url.Parse(wsURL); parseErr == nil {
		originScheme := "https"
		if parsed.Scheme == "ws" {
			originScheme = "http"
		}
		headers.Set("Origin", originScheme+"://"+parsed.Host)
	}
	conn, resp, err := websocket.Dial(connectionCtx, wsURL, &websocket.DialOptions{HTTPHeader: headers})
	if err != nil {
		if resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
			out.err = fmt.Errorf("%w: authentication rejected", errPermanent)
		} else {
			out.err = errors.New("connection failed")
		}
		return out
	}
	defer gracefulWebsocketClose(conn, 100*time.Millisecond)
	conn.SetReadLimit(maxACPLineBytes + 4096)
	if err := writeWS(connectionCtx, conn, cableCommand{Command: "subscribe", Identifier: connectorIdentifier}); err != nil {
		out.err = err
		return out
	}
	for {
		_, data, err := conn.Read(connectionCtx)
		if err != nil {
			out.err = err
			return out
		}
		env, protocolErr := decodeEnvelope(data)
		if protocolErr != nil {
			out.err = protocolErr
			return out
		}
		if (env.Type == "confirm_subscription" || env.Type == "reject_subscription") && !validConnectorIdentifier(env.Identifier) {
			out.err = fmt.Errorf("%w: unexpected Action Cable identifier", errPermanent)
			return out
		}
		if env.Type == "reject_subscription" {
			out.err = fmt.Errorf("%w: subscription rejected", errPermanent)
			return out
		}
		if env.Type == "confirm_subscription" {
			break
		}
	}

	readErr := make(chan error, 1)
	incoming := make(chan string)
	readCtx, cancelRead := context.WithCancel(connectionCtx)
	defer cancelRead()
	go func() {
		for {
			_, data, err := conn.Read(readCtx)
			if err != nil {
				readErr <- err
				return
			}
			env, err := decodeEnvelope(data)
			if err != nil {
				readErr <- err
				return
			}
			if env.Type != "" || len(env.Message) == 0 {
				continue
			}
			if !validConnectorIdentifier(env.Identifier) {
				readErr <- fmt.Errorf("%w: unexpected Action Cable identifier", errPermanent)
				return
			}
			var payload connectorPayload
			if json.Unmarshal(env.Message, &payload) != nil || payload.Type != "acp" || !validInboundACPLine(payload.Line) {
				readErr <- fmt.Errorf("%w: malformed ACP frame", errPermanent)
				return
			}
			select {
			case incoming <- payload.Line:
			case <-readCtx.Done():
				return
			}
		}
	}()

	ticker := time.NewTicker(heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return out
		case <-childDone:
			out.err = errChildExited
			return out
		case err := <-stdoutErr:
			out.err = fmt.Errorf("%w: %v", errPermanent, err)
			return out
		case err := <-readErr:
			out.err = err
			return out
		case line := <-incoming:
			out.traffic = true
			if err := writeAdapterLine(ctx, stdin, line); err != nil {
				out.err = fmt.Errorf("%w: write adapter stdin: %v", errPermanent, err)
				return out
			}
		case line := <-lines:
			out.traffic = true
			if err := perform(ctx, conn, connectorPayload{Type: "acp", Line: line}); err != nil {
				out.err = err
				return out
			}
		case <-ticker.C:
			if err := perform(ctx, conn, connectorPayload{Type: "heartbeat"}); err != nil {
				out.err = err
				return out
			}
		}
	}
}

func validInboundACPLine(line string) bool {
	lineBytes := len(line)
	if strings.HasSuffix(line, "\n") {
		if strings.Count(line, "\n") != 1 {
			return false
		}
	} else {
		if strings.Contains(line, "\n") {
			return false
		}
		lineBytes++
	}
	return lineBytes <= maxACPLineBytes && validJSONObject([]byte(line))
}

func writeAdapterLine(ctx context.Context, stdin io.Writer, line string) error {
	if !strings.HasSuffix(line, "\n") {
		line += "\n"
	}
	done := make(chan error, 1)
	go func() {
		_, err := io.WriteString(stdin, line)
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		if closer, ok := stdin.(io.Closer); ok {
			_ = closer.Close()
		}
		<-done
		return ctx.Err()
	}
}

func gracefulWebsocketClose(conn *websocket.Conn, timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		_ = conn.Close(websocket.StatusNormalClosure, "connector shutdown")
		close(done)
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		_ = conn.CloseNow()
	}
}

func decodeEnvelope(data []byte) (cableEnvelope, error) {
	var env cableEnvelope
	if !validJSONObject(data) || json.Unmarshal(data, &env) != nil {
		return env, fmt.Errorf("%w: malformed Action Cable frame", errPermanent)
	}
	if env.Identifier != "" && !validConnectorIdentifier(env.Identifier) {
		return env, fmt.Errorf("%w: unexpected Action Cable identifier", errPermanent)
	}
	return env, nil
}

func perform(ctx context.Context, conn *websocket.Conn, payload connectorPayload) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return writeWS(ctx, conn, cableCommand{Command: "message", Identifier: connectorIdentifier, Data: string(data)})
}
func writeWS(ctx context.Context, conn *websocket.Conn, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, data)
}

func reconnectBackoff(attempt int) time.Duration {
	if attempt > 5 {
		attempt = 5
	}
	base := 250 * time.Millisecond * time.Duration(1<<attempt)
	return base + time.Duration(rand.Int64N(int64(base/2)+1))
}
