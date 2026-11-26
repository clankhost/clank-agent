package pty

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	clankv1 "github.com/clankhost/clank-agent/gen/clank/v1"
	"github.com/clankhost/clank-agent/internal/docker"
)

type fakeResizeCall struct {
	execID string
	cols   int
	rows   int
}

type fakeAttachCall struct {
	containerName string
	command       []string
	cols          int
	rows          int
}

type fakeDockerPTY struct {
	mu           sync.Mutex
	attachCalls  []fakeAttachCall
	resizeCalls  []fakeResizeCall
	attachErr    error
	resizeErr    error
	exitCodes    map[string]int
	inspectErr   error
	peers        map[string]net.Conn
	nextExecSeq  int
}

func newFakeDockerPTY() *fakeDockerPTY {
	return &fakeDockerPTY{
		exitCodes: make(map[string]int),
		peers:     make(map[string]net.Conn),
	}
}

func (f *fakeDockerPTY) ExecAttachPTY(_ context.Context, containerName string, command []string, cols, rows int) (*docker.PTYSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attachCalls = append(f.attachCalls, fakeAttachCall{
		containerName: containerName,
		command:       append([]string(nil), command...),
		cols:          cols,
		rows:          rows,
	})
	if f.attachErr != nil {
		return nil, f.attachErr
	}
	f.nextExecSeq++
	execID := fmt.Sprintf("exec-%d", f.nextExecSeq)
	clientConn, serverConn := net.Pipe()
	f.peers[execID] = serverConn
	return &docker.PTYSession{
		ExecID: execID,
		Conn:   clientConn,
		Reader: bufio.NewReader(clientConn),
	}, nil
}

func (f *fakeDockerPTY) ExecResizePTY(_ context.Context, execID string, cols, rows int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resizeCalls = append(f.resizeCalls, fakeResizeCall{
		execID: execID,
		cols:   cols,
		rows:   rows,
	})
	return f.resizeErr
}

func (f *fakeDockerPTY) ExecInspectExitCode(_ context.Context, execID string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inspectErr != nil {
		return -1, f.inspectErr
	}
	if code, ok := f.exitCodes[execID]; ok {
		return code, nil
	}
	return 0, nil
}

func (f *fakeDockerPTY) peer(execID string) net.Conn {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.peers[execID]
}

func (f *fakeDockerPTY) setExitCode(execID string, code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.exitCodes[execID] = code
}

func waitOutput(t *testing.T, ch <-chan *clankv1.PtyOutput, pred func(*clankv1.PtyOutput) bool) *clankv1.PtyOutput {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case out := <-ch:
			if pred(out) {
				return out
			}
		case <-deadline:
			t.Fatal("timed out waiting for expected PtyOutput")
		}
	}
}

func TestClampDimensions(t *testing.T) {
	if got := clampCols(0); got != 80 {
		t.Fatalf("clampCols(0) = %d, want 80", got)
	}
	if got := clampCols(5); got != 20 {
		t.Fatalf("clampCols(5) = %d, want 20", got)
	}
	if got := clampCols(-10); got != 20 {
		t.Fatalf("clampCols(-10) = %d, want 20", got)
	}
	if got := clampCols(999); got != 500 {
		t.Fatalf("clampCols(999) = %d, want 500", got)
	}
	if got := clampCols(120); got != 120 {
		t.Fatalf("clampCols(120) = %d, want 120", got)
	}

	if got := clampRows(0); got != 24 {
		t.Fatalf("clampRows(0) = %d, want 24", got)
	}
	if got := clampRows(2); got != 5 {
		t.Fatalf("clampRows(2) = %d, want 5", got)
	}
	if got := clampRows(400); got != 200 {
		t.Fatalf("clampRows(400) = %d, want 200", got)
	}
	if got := clampRows(40); got != 40 {
		t.Fatalf("clampRows(40) = %d, want 40", got)
	}
}

func TestManager_OpenInputOutputResizeCloseExitCode(t *testing.T) {
	fake := newFakeDockerPTY()
	fake.setExitCode("exec-1", 42)
	outCh := make(chan *clankv1.PtyOutput, 32)
	mgr := NewManager(fake, func(out *clankv1.PtyOutput) {
		outCh <- out
	})
	defer mgr.CloseAll()

	// OPEN with out-of-range dims -> clamped to 20x200
	mgr.Handle(context.Background(), &clankv1.PtyCommand{
		SessionId:     "sess-1",
		Action:        clankv1.PtyCommand_OPEN,
		ContainerName: "clank-app-1",
		Command:       []string{"/bin/bash"},
		Cols:          10,
		Rows:          999,
	})

	ready := waitOutput(t, outCh, func(o *clankv1.PtyOutput) bool { return o.GetReady() })
	if ready.GetSessionId() != "sess-1" {
		t.Fatalf("ready session_id = %q, want sess-1", ready.GetSessionId())
	}
	if mgr.ActiveCount() != 1 {
		t.Fatalf("ActiveCount = %d, want 1", mgr.ActiveCount())
	}

	fake.mu.Lock()
	if len(fake.attachCalls) != 1 || fake.attachCalls[0].cols != 20 || fake.attachCalls[0].rows != 200 {
		t.Fatalf("unexpected attach calls: %+v", fake.attachCalls)
	}
	fake.mu.Unlock()

	peer := fake.peer("exec-1")
	if peer == nil {
		t.Fatal("expected peer pipe for exec-1")
	}

	// Container writes output -> client receives Data
	if _, err := peer.Write([]byte("welcome$ ")); err != nil {
		t.Fatalf("peer write: %v", err)
	}
	dataOut := waitOutput(t, outCh, func(o *clankv1.PtyOutput) bool { return len(o.GetData()) > 0 })
	if string(dataOut.GetData()) != "welcome$ " {
		t.Fatalf("output data = %q, want %q", string(dataOut.GetData()), "welcome$ ")
	}

	// Client sends INPUT -> container reads it
	readDone := make(chan string, 1)
	go func() {
		buf := make([]byte, 32)
		n, _ := peer.Read(buf)
		readDone <- string(buf[:n])
	}()

	mgr.Handle(context.Background(), &clankv1.PtyCommand{
		SessionId: "sess-1",
		Action:    clankv1.PtyCommand_INPUT,
		Data:      []byte("ls -la\n"),
	})

	select {
	case got := <-readDone:
		if got != "ls -la\n" {
			t.Fatalf("container read %q, want %q", got, "ls -la\n")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for container to read PTY input")
	}

	// Client sends RESIZE -> clamped to 500x5
	mgr.Handle(context.Background(), &clankv1.PtyCommand{
		SessionId: "sess-1",
		Action:    clankv1.PtyCommand_RESIZE,
		Cols:      900,
		Rows:      2,
	})

	fake.mu.Lock()
	if len(fake.resizeCalls) != 1 || fake.resizeCalls[0].execID != "exec-1" || fake.resizeCalls[0].cols != 500 || fake.resizeCalls[0].rows != 5 {
		t.Fatalf("unexpected resize calls: %+v", fake.resizeCalls)
	}
	fake.mu.Unlock()

	// Process exits -> peer closes -> manager inspects exit code and sends Closed: true
	_ = peer.Close()

	closed := waitOutput(t, outCh, func(o *clankv1.PtyOutput) bool { return o.GetClosed() })
	if closed.GetExitCode() != 42 {
		t.Fatalf("ExitCode = %d, want 42", closed.GetExitCode())
	}
	if closed.GetErrorMessage() != "" {
		t.Fatalf("unexpected ErrorMessage on clean exit: %q", closed.GetErrorMessage())
	}
	if mgr.ActiveCount() != 0 {
		t.Fatalf("ActiveCount after exit = %d, want 0", mgr.ActiveCount())
	}
}

func TestManager_ChunkingLargeOutput(t *testing.T) {
	fake := newFakeDockerPTY()
	outCh := make(chan *clankv1.PtyOutput, 64)
	mgr := NewManager(fake, func(out *clankv1.PtyOutput) {
		outCh <- out
	})
	defer mgr.CloseAll()

	mgr.Handle(context.Background(), &clankv1.PtyCommand{
		SessionId:     "sess-chunk",
		Action:        clankv1.PtyCommand_OPEN,
		ContainerName: "clank-app-1",
	})
	waitOutput(t, outCh, func(o *clankv1.PtyOutput) bool { return o.GetReady() })

	payload := bytes.Repeat([]byte("A"), 20*1024+123)
	peer := fake.peer("exec-1")
	go func() {
		_, _ = peer.Write(payload)
		_ = peer.Close()
	}()

	var combined bytes.Buffer
	for {
		select {
		case out := <-outCh:
			if len(out.GetData()) > readChunkSize {
				t.Fatalf("chunk size %d exceeded readChunkSize %d", len(out.GetData()), readChunkSize)
			}
			combined.Write(out.GetData())
			if out.GetClosed() {
				if !bytes.Equal(combined.Bytes(), payload) {
					t.Fatalf("reassembled len = %d, want %d", combined.Len(), len(payload))
				}
				return
			}
		case <-time.After(2 * time.Second):
			t.Fatal("timed out reading chunked output")
		}
	}
}

func TestManager_RejectsOversizedInput(t *testing.T) {
	fake := newFakeDockerPTY()
	outCh := make(chan *clankv1.PtyOutput, 16)
	mgr := NewManager(fake, func(out *clankv1.PtyOutput) {
		outCh <- out
	})
	defer mgr.CloseAll()

	mgr.Handle(context.Background(), &clankv1.PtyCommand{
		SessionId:     "sess-big",
		Action:        clankv1.PtyCommand_OPEN,
		ContainerName: "clank-app-1",
	})
	waitOutput(t, outCh, func(o *clankv1.PtyOutput) bool { return o.GetReady() })

	oversized := make([]byte, DefaultMaxInputBytes+1)
	mgr.Handle(context.Background(), &clankv1.PtyCommand{
		SessionId: "sess-big",
		Action:    clankv1.PtyCommand_INPUT,
		Data:      oversized,
	})

	errOut := waitOutput(t, outCh, func(o *clankv1.PtyOutput) bool { return o.GetErrorMessage() != "" })
	if errOut.GetClosed() {
		t.Fatal("oversized input should reject chunk without closing session")
	}
	if !strings.Contains(errOut.GetErrorMessage(), "exceeds maximum") {
		t.Fatalf("unexpected error message: %q", errOut.GetErrorMessage())
	}
}

func TestManager_MaxSessionsCap(t *testing.T) {
	fake := newFakeDockerPTY()
	outCh := make(chan *clankv1.PtyOutput, 32)
	mgr := NewManager(fake, func(out *clankv1.PtyOutput) {
		outCh <- out
	})
	mgr.maxSessions = 2
	defer mgr.CloseAll()

	for i := 1; i <= 2; i++ {
		id := fmt.Sprintf("sess-%d", i)
		mgr.Handle(context.Background(), &clankv1.PtyCommand{
			SessionId:     id,
			Action:        clankv1.PtyCommand_OPEN,
			ContainerName: "clank-app-1",
		})
		waitOutput(t, outCh, func(o *clankv1.PtyOutput) bool {
			return o.GetSessionId() == id && o.GetReady()
		})
	}

	// 3rd session must be rejected
	mgr.Handle(context.Background(), &clankv1.PtyCommand{
		SessionId:     "sess-3",
		Action:        clankv1.PtyCommand_OPEN,
		ContainerName: "clank-app-1",
	})
	rejected := waitOutput(t, outCh, func(o *clankv1.PtyOutput) bool {
		return o.GetSessionId() == "sess-3" && o.GetClosed()
	})
	if !strings.Contains(rejected.GetErrorMessage(), "maximum concurrent PTY sessions") {
		t.Fatalf("unexpected cap error: %q", rejected.GetErrorMessage())
	}
	if mgr.ActiveCount() != 2 {
		t.Fatalf("ActiveCount = %d, want 2", mgr.ActiveCount())
	}
}

func TestManager_IdleTimeout(t *testing.T) {
	fake := newFakeDockerPTY()
	outCh := make(chan *clankv1.PtyOutput, 16)
	mgr := NewManager(fake, func(out *clankv1.PtyOutput) {
		outCh <- out
	})
	mgr.idleTimeout = 80 * time.Millisecond
	defer mgr.CloseAll()

	mgr.Handle(context.Background(), &clankv1.PtyCommand{
		SessionId:     "sess-idle",
		Action:        clankv1.PtyCommand_OPEN,
		ContainerName: "clank-app-1",
	})
	waitOutput(t, outCh, func(o *clankv1.PtyOutput) bool { return o.GetReady() })

	closed := waitOutput(t, outCh, func(o *clankv1.PtyOutput) bool { return o.GetClosed() })
	if !strings.Contains(strings.ToLower(closed.GetErrorMessage()), "inactivity") {
		t.Fatalf("expected inactivity error message, got %q", closed.GetErrorMessage())
	}
	if mgr.ActiveCount() != 0 {
		t.Fatalf("ActiveCount after idle timeout = %d, want 0", mgr.ActiveCount())
	}
}

func TestManager_CloseCommandAndCloseAll(t *testing.T) {
	fake := newFakeDockerPTY()
	outCh := make(chan *clankv1.PtyOutput, 32)
	mgr := NewManager(fake, func(out *clankv1.PtyOutput) {
		outCh <- out
	})

	for _, id := range []string{"s1", "s2", "s3"} {
		mgr.Handle(context.Background(), &clankv1.PtyCommand{
			SessionId:     id,
			Action:        clankv1.PtyCommand_OPEN,
			ContainerName: "clank-app-1",
		})
		waitOutput(t, outCh, func(o *clankv1.PtyOutput) bool {
			return o.GetSessionId() == id && o.GetReady()
		})
	}

	// Explicit CLOSE on s1
	mgr.Handle(context.Background(), &clankv1.PtyCommand{
		SessionId: "s1",
		Action:    clankv1.PtyCommand_CLOSE,
	})
	waitOutput(t, outCh, func(o *clankv1.PtyOutput) bool {
		return o.GetSessionId() == "s1" && o.GetClosed()
	})
	if mgr.ActiveCount() != 2 {
		t.Fatalf("ActiveCount after CLOSE s1 = %d, want 2", mgr.ActiveCount())
	}

	// CloseAll closes s2 and s3
	mgr.CloseAll()
	if mgr.ActiveCount() != 0 {
		t.Fatalf("ActiveCount after CloseAll = %d, want 0", mgr.ActiveCount())
	}

	closedIDs := map[string]bool{}
	deadline := time.After(2 * time.Second)
	for len(closedIDs) < 2 {
		select {
		case out := <-outCh:
			if out.GetClosed() {
				closedIDs[out.GetSessionId()] = true
			}
		case <-deadline:
			t.Fatalf("timed out waiting for s2/s3 close events, got %v", closedIDs)
		}
	}

	// Verify peer pipes are closed (Read returns EOF/ErrClosedPipe)
	for _, execID := range []string{"exec-2", "exec-3"} {
		buf := make([]byte, 1)
		_, err := fake.peer(execID).Read(buf)
		if err == nil || (!errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe)) {
			// Any non-nil error confirms the pipe was torn down.
			if err == nil {
				t.Fatalf("expected peer %s to be closed", execID)
			}
		}
	}
}

func TestManager_ErrorPropagationAndValidation(t *testing.T) {
	fake := newFakeDockerPTY()
	outCh := make(chan *clankv1.PtyOutput, 16)
	mgr := NewManager(fake, func(out *clankv1.PtyOutput) {
		outCh <- out
	})
	defer mgr.CloseAll()

	// Missing session_id
	mgr.Handle(context.Background(), &clankv1.PtyCommand{
		Action:        clankv1.PtyCommand_OPEN,
		ContainerName: "clank-app-1",
	})
	out := waitOutput(t, outCh, func(o *clankv1.PtyOutput) bool { return o.GetClosed() })
	if !strings.Contains(out.GetErrorMessage(), "session_id") {
		t.Fatalf("expected session_id error, got %q", out.GetErrorMessage())
	}

	// Missing container_name
	mgr.Handle(context.Background(), &clankv1.PtyCommand{
		SessionId: "s-no-container",
		Action:    clankv1.PtyCommand_OPEN,
	})
	out = waitOutput(t, outCh, func(o *clankv1.PtyOutput) bool { return o.GetClosed() })
	if !strings.Contains(out.GetErrorMessage(), "container_name") {
		t.Fatalf("expected container_name error, got %q", out.GetErrorMessage())
	}

	// Docker attach error propagation
	fake.attachErr = errors.New("refusing to attach PTY to unmanaged container foreign")
	mgr.Handle(context.Background(), &clankv1.PtyCommand{
		SessionId:     "s-unmanaged",
		Action:        clankv1.PtyCommand_OPEN,
		ContainerName: "foreign",
	})
	out = waitOutput(t, outCh, func(o *clankv1.PtyOutput) bool {
		return o.GetSessionId() == "s-unmanaged" && o.GetClosed()
	})
	if !strings.Contains(out.GetErrorMessage(), "unmanaged") {
		t.Fatalf("expected unmanaged error propagated, got %q", out.GetErrorMessage())
	}
	if mgr.ActiveCount() != 0 {
		t.Fatalf("ActiveCount after failed open = %d, want 0", mgr.ActiveCount())
	}
}
