package agent

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	clankv1 "github.com/clankhost/clank-agent/gen/clank/v1"
	"github.com/clankhost/clank-agent/internal/docker"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

func writeMockExecStream(w http.ResponseWriter, r *http.Request, stdout, stderr []byte) {
	_, _ = io.Copy(io.Discard, r.Body)
	hj, _ := w.(http.Hijacker)
	conn, bufrw, err := hj.Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	_, _ = bufrw.WriteString("HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
	if len(stdout) > 0 {
		stdoutW := stdcopy.NewStdWriter(bufrw, stdcopy.Stdout)
		_, _ = stdoutW.Write(stdout)
	}
	if len(stderr) > 0 {
		stderrW := stdcopy.NewStdWriter(bufrw, stdcopy.Stderr)
		_, _ = stderrW.Write(stderr)
	}
	_ = bufrw.Flush()
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
		_, _ = io.Copy(io.Discard, conn)
	}
}

func newMockDockerManagerForHandler(t *testing.T, handler http.Handler) *docker.Manager {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	cli, err := client.NewClientWithOpts(
		client.WithHost("tcp://"+srv.Listener.Addr().String()),
		client.WithVersion("1.45"),
	)
	if err != nil {
		t.Fatalf("NewClientWithOpts: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return docker.NewManagerWithClient(cli)
}

func TestHandleContainerCommandExec_SuccessAndJSONPayload(t *testing.T) {
	var gotCmd []string

	dm := newMockDockerManagerForHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/containers/clank-api-abc12345/json"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Id":"cid-api-1","Config":{"Labels":{"clank.managed":"true"}}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/containers/cid-api-1/exec"):
			var body struct {
				Cmd []string `json:"Cmd"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			gotCmd = body.Cmd
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Id":"exec-ok"}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/exec/exec-ok/start"):
			writeMockExecStream(w, r, []byte("ok-\xff-out"), []byte("warn"))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/exec/exec-ok/json"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ID":"exec-ok","Running":false,"ExitCode":0}`))
		default:
			http.NotFound(w, r)
		}
	}))

	h := &CommandHandler{docker: dm}
	stream := newFakeConnectStream()

	h.HandleContainerCommand(context.Background(), stream, &clankv1.ContainerCommand{
		CommandId:     "cmd-exec-1",
		ContainerName: "clank-api-abc12345",
		Action:        clankv1.ContainerCommand_Action(4),
		VolumeNames:   []string{"echo", "hello"},
	})

	if len(stream.sent) != 1 {
		t.Fatalf("expected 1 sent message, got %d", len(stream.sent))
	}
	res := stream.sent[0].GetCommandResult()
	if res == nil {
		t.Fatalf("expected CommandResult payload, got %+v", stream.sent[0])
	}
	if res.GetCommandId() != "cmd-exec-1" {
		t.Fatalf("CommandId = %q, want cmd-exec-1", res.GetCommandId())
	}
	if !res.GetSuccess() {
		t.Fatalf("expected Success=true, got false (output=%q)", res.GetOutput())
	}
	if len(gotCmd) != 2 || gotCmd[0] != "echo" || gotCmd[1] != "hello" {
		t.Fatalf("unexpected argv passed to docker: %v", gotCmd)
	}

	var parsed struct {
		ExitCode  int    `json:"exit_code"`
		Stdout    string `json:"stdout"`
		Stderr    string `json:"stderr"`
		Truncated bool   `json:"truncated"`
	}
	if err := json.Unmarshal([]byte(res.GetOutput()), &parsed); err != nil {
		t.Fatalf("unmarshal output JSON (%q): %v", res.GetOutput(), err)
	}
	if parsed.ExitCode != 0 || parsed.Stdout != "ok-\uFFFD-out" || parsed.Stderr != "warn" || parsed.Truncated {
		t.Fatalf("unexpected parsed exec result: %+v", parsed)
	}
}

func TestHandleContainerCommandExec_ResolvesByServiceSlug(t *testing.T) {
	dm := newMockDockerManagerForHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/containers/json"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"Id":"cid-slug-1","Names":["/clank-worker-1"],"State":"running"}]`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/containers/cid-slug-1/json"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Id":"cid-slug-1","Config":{"Labels":{"clank.managed":"true"}}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/containers/cid-slug-1/exec"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Id":"exec-slug"}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/exec/exec-slug/start"):
			writeMockExecStream(w, r, []byte("pong"), nil)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/exec/exec-slug/json"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ID":"exec-slug","Running":false,"ExitCode":0}`))
		default:
			http.NotFound(w, r)
		}
	}))

	h := &CommandHandler{docker: dm}
	stream := newFakeConnectStream()

	h.HandleContainerCommand(context.Background(), stream, &clankv1.ContainerCommand{
		CommandId:   "cmd-exec-slug",
		ServiceSlug: "worker",
		Action:      clankv1.ContainerCommand_Action(4),
		VolumeNames: []string{"ping"},
	})

	if len(stream.sent) != 1 {
		t.Fatalf("expected 1 sent message, got %d", len(stream.sent))
	}
	res := stream.sent[0].GetCommandResult()
	if !res.GetSuccess() {
		t.Fatalf("expected Success=true, got false: %q", res.GetOutput())
	}
	var parsed struct {
		ExitCode int    `json:"exit_code"`
		Stdout   string `json:"stdout"`
	}
	if err := json.Unmarshal([]byte(res.GetOutput()), &parsed); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if parsed.ExitCode != 0 || parsed.Stdout != "pong" {
		t.Fatalf("unexpected parsed result: %+v", parsed)
	}
}

func TestHandleContainerCommandExec_RejectsEmptyArgv(t *testing.T) {
	h := &CommandHandler{}
	stream := newFakeConnectStream()

	h.HandleContainerCommand(context.Background(), stream, &clankv1.ContainerCommand{
		CommandId:     "cmd-exec-empty",
		ContainerName: "clank-api-1",
		Action:        clankv1.ContainerCommand_Action(4),
		VolumeNames:   nil,
	})

	if len(stream.sent) != 1 {
		t.Fatalf("expected 1 sent message, got %d", len(stream.sent))
	}
	res := stream.sent[0].GetCommandResult()
	if res.GetSuccess() {
		t.Fatal("expected Success=false for empty argv")
	}
	if !strings.Contains(strings.ToLower(res.GetOutput()), "argv") {
		t.Fatalf("expected error mentioning argv, got %q", res.GetOutput())
	}
}

func TestHandleContainerCommandExec_RejectsUnmanagedContainer(t *testing.T) {
	dm := newMockDockerManagerForHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/containers/unmanaged-box/json") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Id":"cid-unmanaged","Config":{"Labels":{"owner":"someone-else"}}}`))
			return
		}
		http.NotFound(w, r)
	}))

	h := &CommandHandler{docker: dm}
	stream := newFakeConnectStream()

	h.HandleContainerCommand(context.Background(), stream, &clankv1.ContainerCommand{
		CommandId:     "cmd-exec-unmanaged",
		ContainerName: "unmanaged-box",
		Action:        clankv1.ContainerCommand_Action(4),
		VolumeNames:   []string{"id"},
	})

	if len(stream.sent) != 1 {
		t.Fatalf("expected 1 sent message, got %d", len(stream.sent))
	}
	res := stream.sent[0].GetCommandResult()
	if res.GetSuccess() {
		t.Fatal("expected Success=false for unmanaged container")
	}
	if !strings.Contains(strings.ToLower(res.GetOutput()), "unmanaged") {
		t.Fatalf("expected error mentioning unmanaged container, got %q", res.GetOutput())
	}
}
