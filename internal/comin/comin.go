// Package comin makes a running comin agent fetch, over its gRPC unix socket.
package comin

import (
	"context"
	"fmt"
	"time"

	"github.com/nlewo/comin/pkg/protobuf"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/emptypb"
)

// Client calls comin's gRPC API.
type Client struct {
	conn    *grpc.ClientConn
	api     protobuf.CominClient
	timeout time.Duration
}

// New returns a client for the comin socket at the absolute path socket. It
// connects on the first call, so comin may start after the client does. Each
// call is bounded by timeout.
func New(socket string, timeout time.Duration) (*Client, error) {
	conn, err := grpc.NewClient("unix://"+socket, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("creating comin client for %s: %w", socket, err)
	}
	return &Client{conn: conn, api: protobuf.NewCominClient(conn), timeout: timeout}, nil
}

// Fetch makes comin fetch its repository now.
// It returns the error that comin or the connection gives.
// The client package of comin drops that error.
func (c *Client) Fetch(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	if _, err := c.api.Fetch(ctx, &emptypb.Empty{}); err != nil {
		return fmt.Errorf("comin fetch: %w", err)
	}
	return nil
}

// Close releases the connection.
func (c *Client) Close() error {
	if err := c.conn.Close(); err != nil {
		return fmt.Errorf("closing comin client: %w", err)
	}
	return nil
}
