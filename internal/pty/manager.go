package pty

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	clankv1 "github.com/clankhost/clank-agent/gen/clank/v1"
	"github.com/clankhost/clank-agent/internal/docker"
)

const (
	DefaultMaxSessions   = 16
	DefaultIdleTimeout   = 15 * time.Minute
	DefaultMaxInputBytes = 64 * 1024

	readChunkSize = 8 * 1024

	minCols     = 20
	maxCols     = 500
	defaultCols = 80

	minRows     = 5
	maxRows     = 200
	defaultRows = 24

	writeDeadline = 5 * time.Second
)

// DockerPTYClient abstracts Docker PTY operations for unit testability.
type DockerPTYClient interface {
	ExecAttachPTY(ctx context.Context, containerName string, command []string, cols, rows int) (*docker.PTYSession, error)
	ExecResizePTY(ctx context.Context, execID string, cols, rows int) error
	ExecInspectExitCode(ctx context.Context, execID string) (int, error)
}

type activeSession struct {
	id        string
	pty       *docker.PTYSession
	idleTimer *time.Timer

	mu       sync.Mutex
	closed   bool
	timedOut bool

	writeMu sync.Mutex
}

func (s *activeSession) resetIdle(d time.Duration) {
	if d <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed && s.idleTimer != nil {
		s.idleTimer.Reset(d)
	}
}

func (s *activeSession) close(timedOut bool) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	if timedOut {
		s.timedOut = true
	}
	timer := s.idleTimer
	s.idleTimer = nil
	s.mu.Unlock()

	if timer != nil {
		timer.Stop()
	}
	if s.pty != nil {
		_ = s.pty.Close()
	}
}

func (s *activeSession) wasTimedOut() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.timedOut
}

// Manager manages interactive container PTY sessions on the agent.
type Manager struct {
	docker        DockerPTYClient
	sendMu        sync.RWMutex
	send          func(*clankv1.PtyOutput)
	mu            sync.Mutex
	sessions      map[string]*activeSession
	pending       map[string]struct{}
	cancelled     map[string]struct{}
	maxSessions   int
	idleTimeout   time.Duration
	maxInputBytes int
}

// NewManager creates a PTY session manager with default limits.
func NewManager(dockerClient DockerPTYClient, send func(*clankv1.PtyOutput)) *Manager {
	return &Manager{
		docker:        dockerClient,
		send:          send,
		sessions:      make(map[string]*activeSession),
		pending:       make(map[string]struct{}),
		cancelled:     make(map[string]struct{}),
		maxSessions:   DefaultMaxSessions,
		idleTimeout:   DefaultIdleTimeout,
		maxInputBytes: DefaultMaxInputBytes,
	}
}

// SetSender updates the callback used to emit PtyOutput frames.
func (m *Manager) SetSender(send func(*clankv1.PtyOutput)) {
	m.sendMu.Lock()
	m.send = send
	m.sendMu.Unlock()
}

func (m *Manager) emit(out *clankv1.PtyOutput) {
	m.sendMu.RLock()
	fn := m.send
	m.sendMu.RUnlock()
	if fn != nil {
		fn(out)
	}
}

func (m *Manager) emitError(sessionID, msg string, closed bool) {
	m.emit(&clankv1.PtyOutput{
		SessionId:    sessionID,
		Closed:       closed,
		ErrorMessage: sanitizeUTF8(msg),
	})
}

// ActiveCount returns the number of currently active PTY sessions.
func (m *Manager) ActiveCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}

// Handle dispatches a PtyCommand from the control plane.
func (m *Manager) Handle(ctx context.Context, cmd *clankv1.PtyCommand) {
	if cmd == nil {
		return
	}

	switch cmd.GetAction() {
	case clankv1.PtyCommand_OPEN:
		m.handleOpen(ctx, cmd)
	case clankv1.PtyCommand_INPUT:
		m.handleInput(cmd)
	case clankv1.PtyCommand_RESIZE:
		m.handleResize(ctx, cmd)
	case clankv1.PtyCommand_CLOSE:
		m.handleClose(cmd)
	default:
		m.emitError(cmd.GetSessionId(), fmt.Sprintf("unknown PTY action: %v", cmd.GetAction()), false)
	}
}

func (m *Manager) handleOpen(ctx context.Context, cmd *clankv1.PtyCommand) {
	sessionID := strings.TrimSpace(cmd.GetSessionId())
	if sessionID == "" {
		m.emitError("", "session_id is required", true)
		return
	}
	containerName := strings.TrimSpace(cmd.GetContainerName())
	if containerName == "" {
		m.emitError(sessionID, "container_name is required", true)
		return
	}
	if m.docker == nil {
		m.emitError(sessionID, "docker client unavailable", true)
		return
	}

	m.mu.Lock()
	if _, exists := m.sessions[sessionID]; exists {
		m.mu.Unlock()
		m.emitError(sessionID, "PTY session already exists", true)
		return
	}
	if _, opening := m.pending[sessionID]; opening {
		m.mu.Unlock()
		m.emitError(sessionID, "PTY session is already opening", true)
		return
	}
	if len(m.sessions)+len(m.pending) >= m.maxSessions {
		m.mu.Unlock()
		m.emitError(sessionID, fmt.Sprintf("maximum concurrent PTY sessions (%d) reached", m.maxSessions), true)
		return
	}
	m.pending[sessionID] = struct{}{}
	m.mu.Unlock()

	cols := clampCols(int(cmd.GetCols()))
	rows := clampRows(int(cmd.GetRows()))

	ptySess, err := m.docker.ExecAttachPTY(ctx, containerName, cmd.GetCommand(), cols, rows)

	m.mu.Lock()
	delete(m.pending, sessionID)
	_, wasCancelled := m.cancelled[sessionID]
	delete(m.cancelled, sessionID)
	if err != nil {
		m.mu.Unlock()
		if !wasCancelled {
			m.emitError(sessionID, err.Error(), true)
		}
		return
	}
	if wasCancelled {
		m.mu.Unlock()
		_ = ptySess.Close()
		return
	}
	if ctx.Err() != nil {
		m.mu.Unlock()
		_ = ptySess.Close()
		m.emitError(sessionID, ctx.Err().Error(), true)
		return
	}

	sess := &activeSession{
		id:  sessionID,
		pty: ptySess,
	}
	if m.idleTimeout > 0 {
		sess.idleTimer = time.AfterFunc(m.idleTimeout, func() {
			m.closeSession(sessionID, true)
		})
	}
	m.sessions[sessionID] = sess
	m.mu.Unlock()

	m.emit(&clankv1.PtyOutput{
		SessionId: sessionID,
		Ready:     true,
	})

	go m.readLoop(sess)
}

func (m *Manager) handleInput(cmd *clankv1.PtyCommand) {
	sessionID := cmd.GetSessionId()
	data := cmd.GetData()

	m.mu.Lock()
	sess, ok := m.sessions[sessionID]
	m.mu.Unlock()
	if !ok {
		return
	}

	if len(data) > m.maxInputBytes {
		m.emitError(sessionID, fmt.Sprintf("input chunk size %d exceeds maximum %d bytes", len(data), m.maxInputBytes), false)
		return
	}

	sess.resetIdle(m.idleTimeout)
	if len(data) == 0 {
		return
	}

	sess.writeMu.Lock()
	if sess.pty.Conn != nil {
		_ = sess.pty.Conn.SetWriteDeadline(time.Now().Add(writeDeadline))
		_, err := sess.pty.Conn.Write(data)
		_ = sess.pty.Conn.SetWriteDeadline(time.Time{})
		if err != nil {
			sess.writeMu.Unlock()
			log.Printf("[pty] Write failed for session %s: %v", sessionID, err)
			m.closeSession(sessionID, false)
			return
		}
	}
	sess.writeMu.Unlock()
}

func (m *Manager) handleResize(ctx context.Context, cmd *clankv1.PtyCommand) {
	sessionID := cmd.GetSessionId()

	m.mu.Lock()
	sess, ok := m.sessions[sessionID]
	m.mu.Unlock()
	if !ok {
		return
	}

	cols := clampCols(int(cmd.GetCols()))
	rows := clampRows(int(cmd.GetRows()))
	sess.resetIdle(m.idleTimeout)

	if err := m.docker.ExecResizePTY(ctx, sess.pty.ExecID, cols, rows); err != nil {
		log.Printf("[pty] Resize failed for session %s: %v", sessionID, err)
	}
}

func (m *Manager) handleClose(cmd *clankv1.PtyCommand) {
	m.closeSession(cmd.GetSessionId(), false)
}

func (m *Manager) closeSession(sessionID string, timedOut bool) {
	m.mu.Lock()
	sess, ok := m.sessions[sessionID]
	if ok {
		delete(m.sessions, sessionID)
	} else if _, opening := m.pending[sessionID]; opening {
		m.cancelled[sessionID] = struct{}{}
	}
	m.mu.Unlock()
	if ok {
		sess.close(timedOut)
	}
}

// CloseAll closes all active PTY sessions to prevent goroutine and FD leaks.
func (m *Manager) CloseAll() {
	m.mu.Lock()
	sessions := m.sessions
	m.sessions = make(map[string]*activeSession)
	for id := range m.pending {
		m.cancelled[id] = struct{}{}
	}
	m.mu.Unlock()

	for _, sess := range sessions {
		sess.close(false)
	}
}

func (m *Manager) readLoop(sess *activeSession) {
	buf := make([]byte, readChunkSize)
	for {
		n, err := sess.pty.Reader.Read(buf)
		if n > 0 {
			sess.resetIdle(m.idleTimeout)
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			m.emit(&clankv1.PtyOutput{
				SessionId: sess.id,
				Data:      chunk,
			})
		}
		if err != nil {
			break
		}
	}

	m.mu.Lock()
	if cur, ok := m.sessions[sess.id]; ok && cur == sess {
		delete(m.sessions, sess.id)
	}
	m.mu.Unlock()
	sess.close(false)

	inspectCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	exitCode, inspectErr := m.docker.ExecInspectExitCode(inspectCtx, sess.pty.ExecID)
	cancel()
	if inspectErr != nil {
		exitCode = -1
	}

	errMsg := ""
	if sess.wasTimedOut() {
		errMsg = "PTY session closed due to inactivity"
	}

	m.emit(&clankv1.PtyOutput{
		SessionId:    sess.id,
		Closed:       true,
		ExitCode:     int32(exitCode),
		ErrorMessage: errMsg,
	})
}

func clampCols(cols int) int {
	if cols == 0 {
		return defaultCols
	}
	if cols < minCols {
		return minCols
	}
	if cols > maxCols {
		return maxCols
	}
	return cols
}

func clampRows(rows int) int {
	if rows == 0 {
		return defaultRows
	}
	if rows < minRows {
		return minRows
	}
	if rows > maxRows {
		return maxRows
	}
	return rows
}

func sanitizeUTF8(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	return strings.ToValidUTF8(s, "\uFFFD")
}
