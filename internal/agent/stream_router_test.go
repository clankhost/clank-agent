package agent

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	clankv1 "github.com/clankhost/clank-agent/gen/clank/v1"
	"github.com/clankhost/clank-agent/internal/grpcclient"
	"google.golang.org/grpc/metadata"
)

// fakeConnectStream records sends and the peak number of concurrent Send
// calls, which grpc-go requires to be one.
type fakeConnectStream struct {
	mu      sync.Mutex
	sent    []*clankv1.AgentMessage
	inSend  atomic.Int32
	maxSend atomic.Int32
	recv    chan *clankv1.ControlMessage
}

func newFakeConnectStream() *fakeConnectStream {
	return &fakeConnectStream{recv: make(chan *clankv1.ControlMessage, 4)}
}

func (f *fakeConnectStream) Send(m *clankv1.AgentMessage) error {
	n := f.inSend.Add(1)
	defer f.inSend.Add(-1)
	for {
		peak := f.maxSend.Load()
		if n <= peak || f.maxSend.CompareAndSwap(peak, n) {
			break
		}
	}
	time.Sleep(100 * time.Microsecond) // widen the window for overlapping sends
	f.mu.Lock()
	f.sent = append(f.sent, m)
	f.mu.Unlock()
	return nil
}

func (f *fakeConnectStream) SendMsg(m any) error { return f.Send(m.(*clankv1.AgentMessage)) }

func (f *fakeConnectStream) Recv() (*clankv1.ControlMessage, error) {
	m, ok := <-f.recv
	if !ok {
		return nil, io.EOF
	}
	return m, nil
}

func (f *fakeConnectStream) RecvMsg(any) error            { return nil }
func (f *fakeConnectStream) Header() (metadata.MD, error) { return nil, nil }
func (f *fakeConnectStream) Trailer() metadata.MD         { return nil }
func (f *fakeConnectStream) CloseSend() error             { return nil }
func (f *fakeConnectStream) Context() context.Context     { return context.Background() }

func (f *fakeConnectStream) statuses() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.sent))
	for _, m := range f.sent {
		out = append(out, m.GetDeployProgress().GetStatus())
	}
	return out
}

func progress(status string) *clankv1.AgentMessage {
	return &clankv1.AgentMessage{Payload: &clankv1.AgentMessage_DeployProgress{
		DeployProgress: &clankv1.DeployProgress{DeploymentId: "d1", Status: status},
	}}
}

func TestRoutedStreamSendsOnLiveConnection(t *testing.T) {
	var r streamRouter
	fakeA, fakeB := newFakeConnectStream(), newFakeConnectStream()

	a := r.attach(fakeA)
	handlerStream := r.routed(a) // a command arrived on connection A
	b := r.attach(fakeB)         // Cloudflare recycled the stream
	r.detach(a)                  // late teardown of A must not unset B

	if err := handlerStream.Send(progress("built")); err != nil {
		t.Fatalf("send: %v", err)
	}
	if got := fakeA.statuses(); len(got) != 0 {
		t.Fatalf("dead connection received %v", got)
	}
	if got := fakeB.statuses(); len(got) != 1 || got[0] != "built" {
		t.Fatalf("live connection received %v, want [built]", got)
	}
	r.detach(b)
}

func TestRoutedStreamFailsBetweenConnections(t *testing.T) {
	var r streamRouter
	fake := newFakeConnectStream()
	a := r.attach(fake)
	handlerStream := r.routed(a)
	r.detach(a)

	if err := handlerStream.Send(progress("active")); !errors.Is(err, errNoLiveStream) {
		t.Fatalf("err = %v, want errNoLiveStream", err)
	}
	if got := fake.statuses(); len(got) != 0 {
		t.Fatalf("detached connection received %v", got)
	}
}

func TestRoutedStreamReceivesFromItsOwnConnection(t *testing.T) {
	var r streamRouter
	fakeA, fakeB := newFakeConnectStream(), newFakeConnectStream()
	handlerStream := r.routed(r.attach(fakeA))
	r.attach(fakeB)

	want := &clankv1.ControlMessage{Payload: &clankv1.ControlMessage_Ping{}}
	fakeA.recv <- want
	got, err := handlerStream.Recv()
	if err != nil || got != want {
		t.Fatalf("Recv = %v, %v; want the message from connection A", got, err)
	}
}

func TestSendsOnAConnectionAreSerialized(t *testing.T) {
	var r streamRouter
	fake := newFakeConnectStream()
	own := r.attach(fake)
	handlerStream := r.routed(own)

	const n = 64
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Heartbeats use the connection's own stream; handlers the routed one.
			s := grpcclient.ConnectStream(own)
			if i%2 == 0 {
				s = handlerStream
			}
			if err := s.Send(progress("building")); err != nil {
				t.Errorf("send: %v", err)
			}
		}(i)
	}
	wg.Wait()

	if peak := fake.maxSend.Load(); peak != 1 {
		t.Fatalf("peak concurrent Send calls = %d, want 1", peak)
	}
	if got := len(fake.statuses()); got != n {
		t.Fatalf("sent %d messages, want %d", got, n)
	}
}

// Reproduces the production failure: a deploy dispatched on one connection
// keeps reporting after Cloudflare recycles the stream mid-build.
func TestDeployProgressFollowsReconnect(t *testing.T) {
	var r streamRouter
	fakeA, fakeB := newFakeConnectStream(), newFakeConnectStream()
	a := r.attach(fakeA)

	reconnected := make(chan struct{})
	done := make(chan error, 1)
	handlers := grpcclient.CommandHandlers{
		OnDeploy: func(_ context.Context, stream grpcclient.ConnectStream, _ *clankv1.DeployCommand) {
			if err := stream.Send(progress("building")); err != nil {
				done <- err
				return
			}
			<-reconnected
			if err := stream.Send(progress("built")); err != nil {
				done <- err
				return
			}
			done <- stream.Send(progress("active"))
		},
	}

	recvDone := make(chan error, 1)
	go func() { recvDone <- grpcclient.ReceiveCommands(context.Background(), r.routed(a), handlers) }()
	fakeA.recv <- &clankv1.ControlMessage{Payload: &clankv1.ControlMessage_Deploy{
		Deploy: &clankv1.DeployCommand{DeploymentId: "d1"},
	}}

	deadline := time.After(5 * time.Second)
	for len(fakeA.statuses()) == 0 {
		select {
		case <-deadline:
			t.Fatal("deploy handler never reported building")
		case <-time.After(5 * time.Millisecond):
		}
	}

	close(fakeA.recv) // connection A ends
	if err := <-recvDone; err != nil {
		t.Fatalf("ReceiveCommands: %v", err)
	}
	r.detach(a)
	r.attach(fakeB)
	close(reconnected)

	if err := <-done; err != nil {
		t.Fatalf("deploy handler send: %v", err)
	}
	if got := fakeA.statuses(); len(got) != 1 || got[0] != "building" {
		t.Fatalf("connection A received %v, want [building]", got)
	}
	if got := fakeB.statuses(); len(got) != 2 || got[0] != "built" || got[1] != "active" {
		t.Fatalf("connection B received %v, want [built active]", got)
	}
}
