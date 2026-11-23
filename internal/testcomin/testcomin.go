// Package testcomin runs a fake comin gRPC server on a unix socket, for tests.
package testcomin

import (
	"context"
	"net"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/nlewo/comin/pkg/protobuf"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"
)

// Server is a running fake comin.
type Server struct {
	// Socket is the path of the unix socket it listens on.
	Socket string

	fetches atomic.Int32
}

// Fetches is the number of Fetch calls received so far.
func (s *Server) Fetches() int {
	return int(s.fetches.Load())
}

type cominServer struct {
	protobuf.UnimplementedCominServer

	owner    *Server
	fetchErr error
}

func (c *cominServer) Fetch(context.Context, *emptypb.Empty) (*emptypb.Empty, error) {
	c.owner.fetches.Add(1)
	return &emptypb.Empty{}, c.fetchErr
}

// Start serves a fake comin whose Fetch returns fetchErr, until the test ends.
func Start(tb testing.TB, fetchErr error) *Server {
	tb.Helper()
	server := &Server{Socket: filepath.Join(tb.TempDir(), "grpc.sock")}

	var listenConfig net.ListenConfig
	ln, err := listenConfig.Listen(tb.Context(), "unix", server.Socket)
	if err != nil {
		tb.Fatalf("listening on %s: %v", server.Socket, err)
	}
	grpcServer := grpc.NewServer()
	protobuf.RegisterCominServer(grpcServer, &cominServer{owner: server, fetchErr: fetchErr})

	var wg sync.WaitGroup
	wg.Go(func() { _ = grpcServer.Serve(ln) })
	tb.Cleanup(func() {
		grpcServer.Stop()
		wg.Wait()
	})
	return server
}
