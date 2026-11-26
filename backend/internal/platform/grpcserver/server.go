package grpcserver

import (
	"context"
	"log/slog"
	"net"
	"time"

	"google.golang.org/grpc"
)

// Serve runs a gRPC server registered by register until ctx is cancelled.
func Serve(ctx context.Context, addr string, register func(*grpc.Server), logger *slog.Logger) error {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	srv := grpc.NewServer()
	register(srv)

	errCh := make(chan error, 1)
	go func() {
		logger.Info("grpc server listening", "addr", addr)
		errCh <- srv.Serve(lis)
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		logger.Info("grpc server shutting down")
		done := make(chan struct{})
		go func() {
			srv.GracefulStop()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			srv.Stop()
		}
		return nil
	}
}
