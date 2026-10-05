package agent

import (
	"errors"
	"sync"

	clankv1 "github.com/clankhost/clank-agent/gen/clank/v1"
	"github.com/clankhost/clank-agent/internal/grpcclient"
)

// errNoLiveStream is returned when a handler sends between connections.
// Terminal deploy results are queued on this error like any other send
// failure and drained on the next connection.
var errNoLiveStream = errors.New("no live control-plane stream")

// liveStream wraps one connection's Connect stream and serializes sends on
// it: grpc-go does not allow concurrent SendMsg calls on a stream, and
// heartbeats, credential renewal, and command handlers all send at once.
type liveStream struct {
	grpcclient.ConnectStream
	sendMu sync.Mutex
}

func (s *liveStream) Send(msg *clankv1.AgentMessage) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	return s.ConnectStream.Send(msg)
}

func (s *liveStream) SendMsg(m any) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	return s.ConnectStream.SendMsg(m)
}

// streamRouter tracks the live connection so long-running handlers send on
// whichever stream is current, not the one their command arrived on.
// Cloudflare Tunnel recycles gRPC streams about every two minutes, which is
// shorter than most builds; without routing, every progress message after
// the first recycle went to a dead stream and was dropped.
type streamRouter struct {
	mu      sync.Mutex
	current *liveStream
}

// attach makes raw the live stream and returns its serialized wrapper.
func (r *streamRouter) attach(raw grpcclient.ConnectStream) *liveStream {
	ls := &liveStream{ConnectStream: raw}
	r.mu.Lock()
	r.current = ls
	r.mu.Unlock()
	return ls
}

// detach clears the live stream if it is still ls, so a late teardown of an
// old connection cannot unset a newer one.
func (r *streamRouter) detach(ls *liveStream) {
	r.mu.Lock()
	if r.current == ls {
		r.current = nil
	}
	r.mu.Unlock()
}

func (r *streamRouter) send(msg *clankv1.AgentMessage) error {
	r.mu.Lock()
	cur := r.current
	r.mu.Unlock()
	if cur == nil {
		return errNoLiveStream
	}
	return cur.Send(msg)
}

// routed returns the stream handed to command handlers for ls's connection:
// it receives from that connection but sends on the live one.
func (r *streamRouter) routed(ls *liveStream) grpcclient.ConnectStream {
	return &routedStream{liveStream: ls, router: r}
}

type routedStream struct {
	*liveStream
	router *streamRouter
}

func (s *routedStream) Send(msg *clankv1.AgentMessage) error {
	return s.router.send(msg)
}

func (s *routedStream) SendMsg(m any) error {
	if msg, ok := m.(*clankv1.AgentMessage); ok {
		return s.router.send(msg)
	}
	return s.liveStream.SendMsg(m)
}
