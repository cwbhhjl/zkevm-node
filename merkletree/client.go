package merkletree

import (
	"context"
	"time"

	"github.com/0xPolygonHermez/zkevm-node/log"
	"github.com/0xPolygonHermez/zkevm-node/merkletree/pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// NewMTDBServiceClient creates a new MTDB client.
func NewMTDBServiceClient(ctx context.Context, c Config) (pb.StateDBServiceClient, *grpc.ClientConn, context.CancelFunc) {
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	}
	const maxWaitSeconds = 120
	ctx, cancel := context.WithTimeout(ctx, maxWaitSeconds*time.Second)

	mtDBConn, err := grpc.DialContext(ctx, c.URI, opts...)
	if err != nil {
		log.Fatalf("fail to dial: %v", err)
	}

	mtDBClient := pb.NewStateDBServiceClient(mtDBConn)
	return mtDBClient, mtDBConn, cancel
}