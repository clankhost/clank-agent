package grpcclient

import (
	"context"
	"fmt"
	"log"

	clankv1 "github.com/clankhost/clank-agent/gen/clank/v1"
	"google.golang.org/grpc"
)

// StreamMetrics opens a StreamMetrics client-streaming RPC and sends batches
// from the provided channel until the channel is closed or ctx is cancelled.
func StreamMetrics(ctx context.Context, conn *grpc.ClientConn, batches <-chan *clankv1.MetricBatch) error {
	client := clankv1.NewAgentControlServiceClient(conn)
	stream, err := client.StreamMetrics(ctx)
	if err != nil {
		return fmt.Errorf("opening StreamMetrics: %w", err)
	}

	count := 0
	for {
		select {
		case <-ctx.Done():
			_, _ = stream.CloseAndRecv()
			return ctx.Err()
		case batch, ok := <-batches:
			if !ok {
				// Channel closed
				_, err := stream.CloseAndRecv()
				log.Printf("[metrics] StreamMetrics closed after %d batches", count)
				return err
			}
			if err := stream.Send(batch); err != nil {
				return fmt.Errorf("sending metric batch: %w", err)
			}
			count++
		}
	}
}
