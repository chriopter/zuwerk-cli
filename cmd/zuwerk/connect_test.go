package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestACPJSONMustBeObjectAndLimitIncludesNewline(t *testing.T) {
	for _, input := range []string{"[]\n", `"string"` + "\n", "null\n", "1\n", strings.Repeat(" ", maxACPLineBytes-2) + "{}\n"} {
		if _, err := readACPLine(bufio.NewReader(strings.NewReader(input))); err == nil {
			t.Fatalf("accepted %d-byte non-object or oversized line", len(input))
		}
	}
	valid := `{"v":"` + strings.Repeat("x", maxACPLineBytes-len(`{"v":""}`)-1) + `"}` + "\n"
	if _, err := readACPLine(bufio.NewReader(strings.NewReader(valid))); err != nil {
		t.Fatalf("rejected exact-limit line: %v", err)
	}
}

func TestConnectorRejectsWrongIdentifierAndNonObjectInboundACP(t *testing.T) {
	frames := []string{
		`{"type":"confirm_subscription","identifier":"{\"channel\":\"OtherChannel\"}"}`,
		`{"identifier":"{\"channel\":\"OtherChannel\"}","message":{"type":"acp","line":"{}"}}`,
		`{"identifier":"{\"channel\":\"AgentConnectorChannel\"}","message":{"type":"acp","line":"[]"}}`,
	}
	for _, frame := range frames {
		t.Run(frame, func(t *testing.T) {
			server := rawCableServer(t, frame)
			defer server.Close()
			child := newFakeChild()
			err := runConnector(context.Background(), config{ServerURL: server.URL, APIToken: "token"}, []string{"fake"}, connectorOptions{startChild: func(context.Context, []string) (connectorChild, error) { return child, nil }, backoff: func(int) time.Duration { return 0 }, shutdownTimeout: 20 * time.Millisecond})
			if err == nil || !errors.Is(err, errPermanent) {
				t.Fatalf("expected permanent error, got %v", err)
			}
		})
	}
}

func TestDecodeEnvelopeAcceptsSemanticConnectorIdentifier(t *testing.T) {
	for _, identifier := range []string{
		`{"channel":"AgentConnectorChannel"}`,
		`{ "channel" : "AgentConnectorChannel" }`,
	} {
		frame, err := json.Marshal(map[string]any{"type": "confirm_subscription", "identifier": identifier})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := decodeEnvelope(frame); err != nil {
			t.Fatalf("identifier %q rejected: %v", identifier, err)
		}
	}
}

func TestDecodeEnvelopeRejectsInvalidConnectorIdentifier(t *testing.T) {
	for _, identifier := range []string{
		`{"channel":"OtherChannel"}`,
		`{"channel":"AgentConnectorChannel","room":"forbidden"}`,
		`{"channel":"AgentConnectorChannel","channel":"AgentConnectorChannel"}`,
		`{"channel":1}`,
		`not-json`,
	} {
		frame, err := json.Marshal(map[string]any{"type": "confirm_subscription", "identifier": identifier})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := decodeEnvelope(frame); err == nil {
			t.Fatalf("identifier %q accepted", identifier)
		}
	}
}

func TestConnectorIgnoresActionCablePingMessage(t *testing.T) {
	server := confirmedCableServer(t, func(ctx context.Context, conn *websocket.Conn) {
		_ = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"ping","message":1234567890}`))
	})
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	child := newFakeChild()
	err := runConnector(ctx, config{ServerURL: server.URL, APIToken: "token"}, []string{"fake"}, connectorOptions{
		startChild: func(context.Context, []string) (connectorChild, error) { return child, nil },
		backoff:    func(int) time.Duration { return time.Millisecond },
	})
	if err != nil {
		t.Fatalf("Action Cable ping must be ignored: %v", err)
	}
}

func TestReconnectOnlyBeforeACPTraffic(t *testing.T) {
	for _, traffic := range []bool{false, true} {
		t.Run(fmt.Sprint(traffic), func(t *testing.T) {
			var mu sync.Mutex
			connections := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer conn.CloseNow()
				if _, _, err = conn.Read(r.Context()); err != nil {
					return
				}
				mu.Lock()
				connections++
				n := connections
				mu.Unlock()
				_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"confirm_subscription","identifier":"{\"channel\":\"AgentConnectorChannel\"}"}`))
				if n == 1 {
					if traffic {
						_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"identifier":"{\"channel\":\"AgentConnectorChannel\"}","message":{"type":"acp","line":"{}"}}`))
					}
					_ = conn.Close(websocket.StatusGoingAway, "test drop")
					return
				}
				<-r.Context().Done()
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
			defer cancel()
			child := newFakeChild()
			err := runConnector(ctx, config{ServerURL: server.URL, APIToken: "token"}, []string{"fake"}, connectorOptions{startChild: func(context.Context, []string) (connectorChild, error) { return child, nil }, backoff: func(int) time.Duration { return time.Millisecond }, shutdownTimeout: 20 * time.Millisecond})
			mu.Lock()
			got := connections
			mu.Unlock()
			if traffic && (err == nil || got != 1) {
				t.Fatalf("post-traffic drop must fail closed: err=%v connections=%d", err, got)
			}
			if !traffic && got < 2 {
				t.Fatalf("pre-traffic drop did not reconnect/resubscribe: %d", got)
			}
		})
	}
}

func TestChildExitDetectedDuringServerDowntime(t *testing.T) {
	child := newFakeChild()
	done := make(chan error, 1)
	go func() {
		done <- runConnector(context.Background(), config{ServerURL: "http://127.0.0.1:1", APIToken: "token"}, []string{"fake"}, connectorOptions{startChild: func(context.Context, []string) (connectorChild, error) { return child, nil }, backoff: func(int) time.Duration { return time.Hour }, shutdownTimeout: 20 * time.Millisecond})
	}()
	child.done <- errors.New("exit status 9")
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "adapter exited") {
			t.Fatalf("err=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("child exit was not detected while disconnected")
	}
}

func TestExecChildSupportsSignals(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	child, err := startExecChild(ctx, []string{"sh", "-c", "trap 'exit 0' TERM; while :; do sleep 1; done"})
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	_ = child.Kill()
}

func TestWriteAdapterLinePreservesSingleNDJSONDelimiter(t *testing.T) {
	for _, line := range []string{"{}", "{}\n"} {
		var output strings.Builder
		if err := writeAdapterLine(context.Background(), &output, line); err != nil {
			t.Fatal(err)
		}
		if got, want := output.String(), "{}\n"; got != want {
			t.Fatalf("input=%q output=%q want %q", line, got, want)
		}
	}
}

func TestInboundACPLineLimitIncludesExactlyOneDelimiter(t *testing.T) {
	jsonAtLimit := `{"x":"` + strings.Repeat("a", maxACPLineBytes-len(`{"x":""}`)-1) + `"}`
	for _, line := range []string{jsonAtLimit, jsonAtLimit + "\n"} {
		if !validInboundACPLine(line) {
			t.Fatalf("rejected valid %d-byte adapter line", len(line))
		}
	}
	if validInboundACPLine(jsonAtLimit + "\n\n") {
		t.Fatal("accepted multiple delimiters")
	}
	overLimit := `{"x":"` + strings.Repeat("a", maxACPLineBytes-len(`{"x":""}`)) + `"}`
	if validInboundACPLine(overLimit) || validInboundACPLine(overLimit+"\n") {
		t.Fatal("accepted oversized adapter line")
	}
}

func TestBlockedAdapterStdinIsUnblockedOnCancellation(t *testing.T) {
	r, w := io.Pipe()
	defer r.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- writeAdapterLine(ctx, w, strings.Repeat("x", 64*1024)) }()
	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked stdin writer leaked")
	}
}

func TestParseConnectArgs(t *testing.T) {
	for _, tt := range []struct {
		args []string
		want []string
		ok   bool
	}{
		{[]string{"claude"}, []string{"claude-agent-acp"}, true},
		{[]string{"codex"}, []string{"codex-acp"}, true},
		{[]string{"hermes"}, []string{"hermes", "acp"}, true},
		{[]string{"--", "adapter"}, []string{"adapter"}, true},
		{[]string{"--", "adapter", "--flag", "value"}, []string{"adapter", "--flag", "value"}, true},
		{nil, nil, false}, {[]string{"adapter"}, nil, false}, {[]string{"CLAUDE"}, nil, false},
		{[]string{"claude", "--flag"}, nil, false}, {[]string{"--"}, nil, false}, {[]string{"--", ""}, nil, false},
	} {
		got, err := parseConnectArgs(tt.args)
		if (err == nil) != tt.ok || fmt.Sprint(got) != fmt.Sprint(tt.want) {
			t.Fatalf("args=%v got=%v err=%v", tt.args, got, err)
		}
	}
}

func TestWorkingDirectoryReplacesServerPlaceholderForSessionRequests(t *testing.T) {
	for _, method := range []string{"session/new", "session/load"} {
		input := fmt.Sprintf(`{"jsonrpc":"2.0","id":7,"method":%q,"params":{"cwd":"/workspace","mcpServers":[]}}`, method) + "\n"
		got, err := withWorkingDirectory(input, "/home/agent/project")
		if err != nil {
			t.Fatal(err)
		}
		var message map[string]any
		if err := json.Unmarshal([]byte(got), &message); err != nil {
			t.Fatal(err)
		}
		params := message["params"].(map[string]any)
		if params["cwd"] != "/home/agent/project" {
			t.Fatalf("%s cwd=%v", method, params["cwd"])
		}
		if _, ok := params["mcpServers"]; !ok {
			t.Fatalf("%s lost other params", method)
		}
	}

	notification := `{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"one"}}` + "\n"
	if got, err := withWorkingDirectory(notification, "/home/agent/project"); err != nil || got != notification {
		t.Fatalf("unrelated message changed: %q, %v", got, err)
	}
}

func TestCableURL(t *testing.T) {
	for raw, want := range map[string]string{
		"http://example.test":        "ws://example.test/cable",
		"https://example.test/base/": "wss://example.test/base/cable",
	} {
		got, err := cableURL(raw)
		if err != nil || got != want {
			t.Fatalf("cableURL(%q)=%q,%v", raw, got, err)
		}
	}
	for _, raw := range []string{"ftp://example.test", "https://user:secret@example.test", "https://example.test?q=token"} {
		if _, err := cableURL(raw); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
}

func TestConnectBridgesInOrderWithBearerSubscriptionAndHeartbeat(t *testing.T) {
	var mu sync.Mutex
	var received []cableCommand
	subscribed := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/cable" {
			t.Errorf("path=%q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret-token" {
			t.Errorf("authorization=%q", got)
		}
		if got, want := r.Header.Get("Origin"), "http://"+r.Host; got != want {
			t.Errorf("origin=%q want %q", got, want)
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		ctx := r.Context()
		_, b, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var sub cableCommand
		json.Unmarshal(b, &sub)
		if sub.Command != "subscribe" || sub.Identifier != connectorIdentifier {
			t.Errorf("subscription=%s", b)
		}
		conn.Write(ctx, websocket.MessageText, []byte(`{"type":"confirm_subscription","identifier":"{\"channel\":\"AgentConnectorChannel\"}"}`))
		close(subscribed)
		conn.Write(ctx, websocket.MessageText, []byte(`{"identifier":"{\"channel\":\"AgentConnectorChannel\"}","message":{"type":"acp","line":"{\"jsonrpc\":\"2.0\",\"id\":1}"}}`))
		for {
			_, b, err = conn.Read(ctx)
			if err != nil {
				return
			}
			var cmd cableCommand
			if json.Unmarshal(b, &cmd) == nil {
				mu.Lock()
				received = append(received, cmd)
				mu.Unlock()
			}
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	child := newFakeChild()
	done := make(chan error, 1)
	go func() {
		done <- runConnector(ctx, config{ServerURL: server.URL, APIToken: "secret-token"}, []string{"fake"}, connectorOptions{
			startChild:        func(context.Context, []string) (connectorChild, error) { return child, nil },
			heartbeatInterval: 10 * time.Millisecond, backoff: func(int) time.Duration { return time.Millisecond },
		})
	}()
	<-subscribed
	line, err := child.stdinReader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if line != `{"jsonrpc":"2.0","id":1}`+"\n" {
		t.Fatalf("stdin=%q", line)
	}
	child.writeLine(`{"jsonrpc":"2.0","id":2}`)
	child.writeLine(`{"jsonrpc":"2.0","id":3}`)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		snapshot := append([]cableCommand(nil), received...)
		mu.Unlock()
		var lines []string
		heartbeat := false
		for _, c := range snapshot {
			var d connectorPayload
			json.Unmarshal([]byte(c.Data), &d)
			if d.Type == "acp" {
				lines = append(lines, d.Line)
			}
			if d.Type == "heartbeat" {
				heartbeat = true
			}
		}
		if len(lines) >= 2 && heartbeat {
			if lines[0] != `{"jsonrpc":"2.0","id":2}`+"\n" || lines[1] != `{"jsonrpc":"2.0","id":3}`+"\n" {
				t.Fatalf("lines=%v", lines)
			}
			cancel()
			<-done
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for output and heartbeat")
}

func TestReadACPLineRejectsMalformedAndOversized(t *testing.T) {
	for _, input := range []string{"not-json\n", strings.Repeat("x", maxACPLineBytes+1) + "\n"} {
		if _, err := readACPLine(bufio.NewReader(strings.NewReader(input))); err == nil {
			t.Fatalf("accepted input length %d", len(input))
		}
	}
	if got, err := readACPLine(bufio.NewReader(strings.NewReader(`{"ok":true}` + "\n"))); err != nil || got != `{"ok":true}` {
		t.Fatalf("got=%q err=%v", got, err)
	}
	undelimitedAtLimit := `{"x":"` + strings.Repeat("a", maxACPLineBytes-len(`{"x":""}`)) + `"}`
	if _, err := readACPLine(bufio.NewReader(strings.NewReader(undelimitedAtLimit))); err == nil {
		t.Fatal("accepted max-size adapter JSON that needs an additional delimiter")
	}
}

func TestRedactToken(t *testing.T) {
	token := "top-secret-token"
	got := redactToken(fmt.Errorf("dial failed for Bearer %s at https://host/?token=%s", token, token), token)
	if strings.Contains(got.Error(), token) {
		t.Fatalf("token leaked: %v", got)
	}
}

func TestConnectorStopsOnChildExitAndCancellation(t *testing.T) {
	server := confirmedCableServer(t, nil)
	defer server.Close()
	for _, cancelFirst := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		child := newFakeChild()
		done := make(chan error, 1)
		go func() {
			done <- runConnector(ctx, config{ServerURL: server.URL, APIToken: "token"}, []string{"fake"}, connectorOptions{
				startChild: func(context.Context, []string) (connectorChild, error) { return child, nil },
				backoff:    func(int) time.Duration { return time.Millisecond },
			})
		}()
		if cancelFirst {
			cancel()
		} else {
			child.done <- errors.New("exit status 7")
		}
		select {
		case err := <-done:
			if cancelFirst && err != nil {
				t.Fatalf("cancellation error: %v", err)
			}
			if !cancelFirst && (err == nil || !strings.Contains(err.Error(), "adapter exited")) {
				t.Fatalf("child exit error: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("connector did not stop")
		}
		cancel()
	}
}

func TestConnectorRejectsMalformedServerACPFrame(t *testing.T) {
	server := confirmedCableServer(t, func(ctx context.Context, conn *websocket.Conn) {
		conn.Write(ctx, websocket.MessageText, []byte(`{"identifier":"{\"channel\":\"AgentConnectorChannel\"}","message":{"type":"acp","line":"not-json"}}`))
	})
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	child := newFakeChild()
	err := runConnector(ctx, config{ServerURL: server.URL, APIToken: "token"}, []string{"fake"}, connectorOptions{
		startChild: func(context.Context, []string) (connectorChild, error) { return child, nil },
		backoff:    func(int) time.Duration { return time.Millisecond },
	})
	if err == nil || !errors.Is(err, errPermanent) {
		t.Fatalf("malformed protocol frame must be permanent: %v", err)
	}
}

func rawCableServer(t *testing.T, frame string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		if _, _, err = conn.Read(r.Context()); err == nil {
			if !strings.Contains(frame, `"type":"confirm_subscription"`) {
				_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"confirm_subscription","identifier":"{\"channel\":\"AgentConnectorChannel\"}"}`))
			}
			_ = conn.Write(r.Context(), websocket.MessageText, []byte(frame))
		}
	}))
}

func confirmedCableServer(t *testing.T, afterConfirm func(context.Context, *websocket.Conn)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		ctx := r.Context()
		if _, _, err = conn.Read(ctx); err != nil {
			return
		}
		if err = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"confirm_subscription","identifier":"{\"channel\":\"AgentConnectorChannel\"}"}`)); err != nil {
			return
		}
		if afterConfirm != nil {
			afterConfirm(ctx, conn)
		}
		<-ctx.Done()
	}))
}

type fakeChild struct {
	stdinReader  *bufio.Reader
	stdinWriter  *io.PipeWriter
	stdoutReader *io.PipeReader
	stdoutWriter *io.PipeWriter
	done         chan error
}

func newFakeChild() *fakeChild {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	return &fakeChild{stdinReader: bufio.NewReader(inR), stdinWriter: inW, stdoutReader: outR, stdoutWriter: outW, done: make(chan error, 1)}
}
func (f *fakeChild) Stdin() io.WriteCloser { return f.stdinWriter }
func (f *fakeChild) Stdout() io.ReadCloser { return f.stdoutReader }
func (f *fakeChild) Wait() error           { return <-f.done }
func (f *fakeChild) Signal(os.Signal) error {
	select {
	case f.done <- context.Canceled:
	default:
	}
	return nil
}
func (f *fakeChild) Kill() error {
	select {
	case f.done <- context.Canceled:
	default:
	}
	f.stdinWriter.Close()
	f.stdoutWriter.Close()
	return nil
}
func (f *fakeChild) writeLine(line string) { fmt.Fprintln(f.stdoutWriter, line) }
