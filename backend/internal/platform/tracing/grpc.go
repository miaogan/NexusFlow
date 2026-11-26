package tracing

import (
	"context"

	"google.golang.org/grpc/metadata"
)

const traceIDMetadataKey = "x-trace-id"

// FromGRPC extracts a trace id carried in incoming gRPC metadata, if any.
func FromGRPC(ctx context.Context) string {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if vals := md.Get(traceIDMetadataKey); len(vals) > 0 && vals[0] != "" {
			return vals[0]
		}
	}
	return ""
}

// ToGRPC injects a trace id into outgoing gRPC metadata so downstream services
// continue the same trace.
func ToGRPC(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return metadata.AppendToOutgoingContext(ctx, traceIDMetadataKey, id)
}
