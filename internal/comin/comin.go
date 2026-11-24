// Package comin makes a running comin agent fetch, over its gRPC unix socket.
//
// It makes the one unary call with net/http over unencrypted HTTP/2.
// That avoids linking a gRPC client.
// The call is the Fetch method of comin.
// Its definition is in pkg/protobuf/services.proto of comin:
// package protobuf, service Comin, and a Fetch method that takes and returns
// an empty message.
package comin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"time"
)

// fetchURL is the gRPC method as an HTTP/2 request target. The host is not
// used: the transport dials the unix socket whatever it is.
const fetchURL = "http://comin/protobuf.Comin/Fetch"

// maxResponseBytes bounds how much of a response body the client reads. The
// reply to Fetch is an empty message, so any real reply is far below it.
const maxResponseBytes = 1 << 20

// emptyMessage is an empty protobuf message in a gRPC frame.
// The frame is a zero compression flag and a four byte length of zero.
var emptyMessage = []byte{0, 0, 0, 0, 0}

// StatusError is a call that comin answered with a non-zero grpc-status.
type StatusError struct {
	// Code is the grpc-status value.
	Code string
	// Message is the decoded grpc-message value.
	Message string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("grpc-status %s: %s", e.Code, e.Message)
}

// Client calls comin's gRPC API.
type Client struct {
	http    *http.Client
	timeout time.Duration
}

// New returns a client for the comin socket at the absolute path socket. It
// connects on the first call, so comin may start after the client does. Each
// call is bounded by timeout.
func New(socket string, timeout time.Duration) (*Client, error) {
	if !path.IsAbs(socket) {
		return nil, fmt.Errorf("the comin socket must be an absolute path, got %q", socket)
	}
	var protocols http.Protocols
	protocols.SetUnencryptedHTTP2(true)
	transport := &http.Transport{
		Protocols: &protocols,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", socket)
		},
	}
	return &Client{http: &http.Client{Transport: transport}, timeout: timeout}, nil
}

// Fetch makes comin fetch its repository now.
// It returns an error wrapping a *StatusError when comin answers with a
// non-zero grpc-status, and the transport error when no answer arrives.
func (c *Client) Fetch(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fetchURL, bytes.NewReader(emptyMessage))
	if err != nil {
		return fmt.Errorf("comin fetch: %w", err)
	}
	req.Header.Set("Content-Type", "application/grpc")
	req.Header.Set("Te", "trailers")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("comin fetch: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("comin fetch: HTTP status %d", resp.StatusCode)
	}

	// A reply with no message has grpc-status in its headers.
	// Otherwise the status comes in the trailers, which are complete only once
	// the body has ended.
	status := resp.Header
	if status.Get("Grpc-Status") == "" {
		read, err := io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes+1))
		if err != nil {
			return fmt.Errorf("comin fetch: reading the reply: %w", err)
		}
		if read > maxResponseBytes {
			return fmt.Errorf("comin fetch: the reply exceeds %d bytes", maxResponseBytes)
		}
		status = resp.Trailer
	}
	code := status.Get("Grpc-Status")
	switch code {
	case "0":
		return nil
	case "":
		return errors.New("comin fetch: the reply has no grpc-status")
	default:
		return fmt.Errorf("comin fetch: %w", &StatusError{Code: code, Message: decodeMessage(status.Get("Grpc-Message"))})
	}
}

// decodeMessage undoes the percent-encoding of a grpc-message value.
func decodeMessage(encoded string) string {
	if decoded, err := url.PathUnescape(encoded); err == nil {
		return decoded
	}
	return encoded
}

// Close releases the idle connections.
func (c *Client) Close() error {
	c.http.CloseIdleConnections()
	return nil
}
