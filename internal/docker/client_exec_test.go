package docker

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

func writeHijackedExecStream(w http.ResponseWriter, r *http.Request, stdout, stderr []byte, hold <-chan struct{}) {
	_, _ = io.Copy(io.Discard, r.Body)
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
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
	if hold != nil {
		<-hold
		return
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
		_, _ = io.Copy(io.Discard, conn)
	}
}

func newTestDockerManager(t *testing.T, handler http.Handler) *Manager {
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
	return NewManagerWithClient(cli)
}

func TestFindAllContainersByLabel(t *testing.T) {
	var sawAll string
	var sawFilters string

	mgr := newTestDockerManager(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/containers/json") {
			sawAll = r.URL.Query().Get("all")
			sawFilters = r.URL.Query().Get("filters")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[
				{"Id":"cid-111111111111","Names":["/clank-api-1"],"State":"running"},
				{"Id":"cid-222222222222","Names":["/clank-api-2"],"State":"exited"}
			]`))
			return
		}
		http.NotFound(w, r)
	}))

	refs, err := mgr.FindAllContainersByLabel(context.Background(), "clank.service_slug", "api")
	if err != nil {
		t.Fatalf("FindAllContainersByLabel: %v", err)
	}
	if sawAll != "1" && sawAll != "true" {
		t.Fatalf("expected all=1 in ContainerList query, got %q", sawAll)
	}
	if !strings.Contains(sawFilters, "clank.service_slug=api") {
		t.Fatalf("expected label filter in query, got %q", sawFilters)
	}
	if len(refs) != 2 {
		t.Fatalf("expected 2 containers, got %d", len(refs))
	}
	if refs[0].ID != "cid-111111111111" || refs[0].Name != "clank-api-1" || refs[0].State != "running" {
		t.Fatalf("unexpected ref[0]: %+v", refs[0])
	}
	if refs[1].ID != "cid-222222222222" || refs[1].Name != "clank-api-2" || refs[1].State != "exited" {
		t.Fatalf("unexpected ref[1]: %+v", refs[1])
	}
}

func TestExecInContainer_SuccessAndDemux(t *testing.T) {
	var receivedCmd []string

	mgr := newTestDockerManager(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/containers/clank-app-1/json"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Id":"cid-managed-1","Config":{"Labels":{"clank.managed":"true"}}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/containers/cid-managed-1/exec"):
			var body struct {
				Cmd          []string `json:"Cmd"`
				AttachStdout bool     `json:"AttachStdout"`
				AttachStderr bool     `json:"AttachStderr"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode exec create: %v", err)
			}
			receivedCmd = body.Cmd
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Id":"exec-1"}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/exec/exec-1/start"):
			writeHijackedExecStream(w, r, []byte("hello stdout\n"), []byte("hello stderr\n"), nil)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/exec/exec-1/json"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ID":"exec-1","Running":false,"ExitCode":7}`))
		default:
			http.NotFound(w, r)
		}
	}))

	res, err := mgr.ExecInContainer(context.Background(), "clank-app-1", []string{"sh", "-c", "echo hi"}, 1024)
	if err != nil {
		t.Fatalf("ExecInContainer: %v", err)
	}
	if len(receivedCmd) != 3 || receivedCmd[0] != "sh" || receivedCmd[2] != "echo hi" {
		t.Fatalf("unexpected exec cmd: %v", receivedCmd)
	}
	if res.ExitCode != 7 {
		t.Fatalf("ExitCode = %d, want 7", res.ExitCode)
	}
	if res.Stdout != "hello stdout\n" {
		t.Fatalf("Stdout = %q, want %q", res.Stdout, "hello stdout\n")
	}
	if res.Stderr != "hello stderr\n" {
		t.Fatalf("Stderr = %q, want %q", res.Stderr, "hello stderr\n")
	}
	if res.Truncated {
		t.Fatal("expected Truncated=false")
	}
}

func TestExecInContainer_TruncatesOutput(t *testing.T) {
	mgr := newTestDockerManager(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/containers/clank-app-1/json"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Id":"cid-managed-1","Config":{"Labels":{"clank.managed":"true"}}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/containers/cid-managed-1/exec"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Id":"exec-trunc"}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/exec/exec-trunc/start"):
			writeHijackedExecStream(w, r, []byte("0123456789ABCDEF"), []byte("err"), nil)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/exec/exec-trunc/json"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ID":"exec-trunc","Running":false,"ExitCode":0}`))
		default:
			http.NotFound(w, r)
		}
	}))

	res, err := mgr.ExecInContainer(context.Background(), "clank-app-1", []string{"cat"}, 8)
	if err != nil {
		t.Fatalf("ExecInContainer: %v", err)
	}
	if res.Stdout != "01234567" {
		t.Fatalf("Stdout = %q, want %q", res.Stdout, "01234567")
	}
	if res.Stderr != "err" {
		t.Fatalf("Stderr = %q, want %q", res.Stderr, "err")
	}
	if !res.Truncated {
		t.Fatal("expected Truncated=true")
	}
}

func TestExecInContainer_RejectsUnmanagedContainer(t *testing.T) {
	var execCreated atomic.Bool

	mgr := newTestDockerManager(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/containers/foreign-1/json"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Id":"cid-foreign","Config":{"Labels":{"app":"other"}}}`))
		case strings.Contains(r.URL.Path, "/exec"):
			execCreated.Store(true)
			http.Error(w, "should not be called", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))

	_, err := mgr.ExecInContainer(context.Background(), "foreign-1", []string{"id"}, 1024)
	if err == nil || !strings.Contains(err.Error(), "unmanaged") {
		t.Fatalf("expected unmanaged container error, got: %v", err)
	}
	if execCreated.Load() {
		t.Fatal("exec must not be created on unmanaged container")
	}
}

func TestExecInContainer_RespectsContextCancel(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	mgr := newTestDockerManager(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/containers/clank-app-1/json"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Id":"cid-managed-1","Config":{"Labels":{"clank.managed":"true"}}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/containers/cid-managed-1/exec"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Id":"exec-hang"}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/exec/exec-hang/start"):
			writeHijackedExecStream(w, r, nil, nil, release)
		default:
			http.NotFound(w, r)
		}
	}))

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := mgr.ExecInContainer(ctx, "clank-app-1", []string{"sleep", "60"}, 1024)
	if err == nil {
		t.Fatal("expected timeout error on hanging exec")
	}
}

func writeHijackedPTYStream(w http.ResponseWriter, r *http.Request, output []byte) {
	_, _ = io.Copy(io.Discard, r.Body)
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	conn, bufrw, err := hj.Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	_, _ = bufrw.WriteString("HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
	if len(output) > 0 {
		_, _ = bufrw.Write(output)
	}
	_ = bufrw.Flush()
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
		_, _ = io.Copy(io.Discard, conn)
	}
}

func TestExecAttachPTY_SuccessResizeAndExitCode(t *testing.T) {
	var created struct {
		Tty          bool     `json:"Tty"`
		AttachStdin  bool     `json:"AttachStdin"`
		AttachStdout bool     `json:"AttachStdout"`
		AttachStderr bool     `json:"AttachStderr"`
		Cmd          []string `json:"Cmd"`
		Env          []string `json:"Env"`
	}
	var resizeCalls []string

	mgr := newTestDockerManager(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/containers/clank-app-1/json"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Id":"cid-pty-1","State":{"Running":true},"Config":{"Labels":{"clank.managed":"true"}}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/containers/cid-pty-1/exec"):
			if err := json.NewDecoder(r.Body).Decode(&created); err != nil {
				t.Errorf("decode exec create: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Id":"exec-pty-1"}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/exec/exec-pty-1/start"):
			writeHijackedPTYStream(w, r, []byte("sh-5.2# "))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/exec/exec-pty-1/resize"):
			resizeCalls = append(resizeCalls, r.URL.Query().Get("w")+"x"+r.URL.Query().Get("h"))
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/exec/exec-pty-1/json"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ID":"exec-pty-1","Running":false,"ExitCode":130}`))
		default:
			http.NotFound(w, r)
		}
	}))

	sess, err := mgr.ExecAttachPTY(context.Background(), "clank-app-1", nil, 100, 30)
	if err != nil {
		t.Fatalf("ExecAttachPTY: %v", err)
	}
	defer sess.Close()

	if sess.ExecID != "exec-pty-1" {
		t.Fatalf("ExecID = %q, want exec-pty-1", sess.ExecID)
	}
	if !created.Tty || !created.AttachStdin || !created.AttachStdout || !created.AttachStderr {
		t.Fatalf("unexpected PTY exec flags: %+v", created)
	}
	if len(created.Cmd) != 1 || created.Cmd[0] != "/bin/sh" {
		t.Fatalf("default Cmd = %v, want [/bin/sh]", created.Cmd)
	}
	if len(created.Env) != 1 || created.Env[0] != "TERM=xterm-256color" {
		t.Fatalf("Env = %v, want [TERM=xterm-256color]", created.Env)
	}

	out, err := io.ReadAll(sess.Reader)
	if err != nil {
		t.Fatalf("reading PTY output: %v", err)
	}
	if string(out) != "sh-5.2# " {
		t.Fatalf("PTY output = %q, want %q", string(out), "sh-5.2# ")
	}

	if err := mgr.ExecResizePTY(context.Background(), sess.ExecID, 120, 40); err != nil {
		t.Fatalf("ExecResizePTY: %v", err)
	}
	if len(resizeCalls) != 2 || resizeCalls[0] != "100x30" || resizeCalls[1] != "120x40" {
		t.Fatalf("resize calls = %v, want [100x30 120x40]", resizeCalls)
	}

	exitCode, err := mgr.ExecInspectExitCode(context.Background(), sess.ExecID)
	if err != nil {
		t.Fatalf("ExecInspectExitCode: %v", err)
	}
	if exitCode != 130 {
		t.Fatalf("exitCode = %d, want 130", exitCode)
	}
}

func TestExecAttachPTY_RejectsUnmanagedAndStoppedContainers(t *testing.T) {
	var execCreated atomic.Bool

	mgr := newTestDockerManager(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/containers/foreign-box/json"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Id":"cid-foreign","State":{"Running":true},"Config":{"Labels":{"app":"other"}}}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/containers/stopped-box/json"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Id":"cid-stopped","State":{"Running":false},"Config":{"Labels":{"clank.managed":"true"}}}`))
		case strings.Contains(r.URL.Path, "/exec"):
			execCreated.Store(true)
			http.Error(w, "should not be called", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))

	_, err := mgr.ExecAttachPTY(context.Background(), "foreign-box", nil, 80, 24)
	if err == nil || !strings.Contains(err.Error(), "unmanaged") {
		t.Fatalf("expected unmanaged container error, got: %v", err)
	}

	_, err = mgr.ExecAttachPTY(context.Background(), "stopped-box", nil, 80, 24)
	if err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("expected not running error, got: %v", err)
	}

	if execCreated.Load() {
		t.Fatal("exec must not be created on unmanaged or stopped container")
	}
}
