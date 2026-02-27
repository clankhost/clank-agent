package grpcclient

import (
	"context"
	"fmt"
	"log"

	clankv1 "github.com/anaremore/clank/apps/agent/gen/clank/v1"
	"google.golang.org/grpc"
)

// StreamLogs opens a StreamLogs client-streaming RPC and sends log entries
// from the provided channel until the channel is closed or ctx is cancelled.
func StreamLogs(ctx context.Context, conn *grpc.ClientConn, entries <-chan *clankv1.LogEntry) error {
	client := clankv1.NewAgentControlServiceClient(conn)
	stream, err := client.StreamLogs(ctx)
	if err != nil {
		return fmt.Errorf("opening StreamLogs: %w", err)
	}

	count := 0
	for {
		select {
		case <-ctx.Done():
			_, _ = stream.CloseAndRecv()
			return ctx.Err()
		case entry, ok := <-entries:
			if !ok {
				// Channel closed
				_, err := stream.CloseAndRecv()
				log.Printf("[logs] StreamLogs closed after %d entries", count)
				return err
			}
			if err := stream.Send(entry); err != nil {
				return fmt.Errorf("sending log entry: %w", err)
			}
			count++
		}
	}
}
