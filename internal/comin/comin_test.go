package comin_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/victor-software-house/comin-hook/internal/comin"
)

// request is what the fake comin saw of a call.
type request struct {
	Method      string
	Path        string
	Proto       string
	ContentType string
	TE          string
	Body        []byte
}

// fakeComin serves the Fetch call on a unix socket over unencrypted HTTP/2,
// as comin does. It records each call it gets.
func fakeComin(tb testing.TB, reply func(w http.ResponseWriter, r *http.Request)) (socket string, calls <-chan request) {
	tb.Helper()
	socket = filepath.Join(tb.TempDir(), "grpc.sock")
	seen := make(chan request, 8)

	var listenConfig net.ListenConfig
	ln, err := listenConfig.Listen(tb.Context(), "unix", socket)
	if err != nil {
		tb.Fatalf("listening on %s: %v", socket, err)
	}
	var protocols http.Protocols
	protocols.SetUnencryptedHTTP2(true)
	srv := &http.Server{
		Protocols:         &protocols,
		ReadHeaderTimeout: time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			seen <- request{
				Method:      r.Method,
				Path:        r.URL.Path,
				Proto:       r.Proto,
				ContentType: r.Header.Get("Content-Type"),
				TE:          r.Header.Get("Te"),
				Body:        body,
			}
			reply(w, r)
		}),
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Serve(ln)
	}()
	tb.Cleanup(func() {
		_ = srv.Close()
		<-done
	})
	return socket, seen
}

// replyWithTrailers answers as grpc-go does for a call with a message.
// It sends headers, an empty message, and then the status in trailers.
func replyWithTrailers(status, message string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/grpc")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte{0, 0, 0, 0, 0})
		w.Header().Set(http.TrailerPrefix+"Grpc-Status", status)
		if message != "" {
			w.Header().Set(http.TrailerPrefix+"Grpc-Message", message)
		}
	}
}

// replyTrailersOnly answers as grpc-go does for a failed call.
// The status is in the headers, and there is no body.
func replyTrailersOnly(status, message string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/grpc")
		w.Header().Set("Grpc-Status", status)
		w.Header().Set("Grpc-Message", message)
		w.WriteHeader(http.StatusOK)
	}
}

func TestFetch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		reply      func(http.ResponseWriter, *http.Request)
		wantStatus *comin.StatusError
		wantErr    bool
		// wantMessage is a text that the error contains.
		wantMessage string
	}{
		{name: "status 0 in the trailers", reply: replyWithTrailers("0", "")},
		{
			name:       "non-zero status in the headers",
			reply:      replyTrailersOnly("13", "boom%20now"),
			wantStatus: &comin.StatusError{Code: "13", Message: "boom now"},
			wantErr:    true,
		},
		{
			name:       "non-zero status in the trailers",
			reply:      replyWithTrailers("14", "gone"),
			wantStatus: &comin.StatusError{Code: "14", Message: "gone"},
			wantErr:    true,
		},
		{
			name: "no status",
			reply: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			},
			wantErr: true,
		},
		{
			name: "reply over the limit",
			reply: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/grpc")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(make([]byte, 2<<20))
				w.Header().Set(http.TrailerPrefix+"Grpc-Status", "0")
			},
			wantErr:     true,
			wantMessage: "exceeds",
		},
		{
			name:    "stream reset before the reply",
			reply:   func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) },
			wantErr: true,
		},
		{
			name: "stream reset during the reply",
			reply: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/grpc")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte{0, 0, 0, 0, 0})
				_ = http.NewResponseController(w).Flush()
				panic(http.ErrAbortHandler)
			},
			wantErr: true,
		},
		{
			name: "HTTP error",
			reply: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusBadGateway)
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			socket, calls := fakeComin(t, tt.reply)
			client, err := comin.New(socket, 5*time.Second)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			t.Cleanup(func() {
				if err := client.Close(); err != nil {
					t.Errorf("Close: %v", err)
				}
			})

			err = client.Fetch(t.Context())
			if (err != nil) != tt.wantErr {
				t.Fatalf("Fetch error = %v, want error: %t", err, tt.wantErr)
			}
			if tt.wantMessage != "" && !strings.Contains(err.Error(), tt.wantMessage) {
				t.Errorf("Fetch error = %q, want one that contains %q", err, tt.wantMessage)
			}
			if tt.wantStatus != nil {
				statusErr, ok := errors.AsType[*comin.StatusError](err)
				if !ok {
					t.Fatalf("Fetch error = %v, want a *comin.StatusError", err)
				}
				if diff := cmp.Diff(tt.wantStatus, statusErr); diff != "" {
					t.Errorf("status error mismatch (-want +got):\n%s", diff)
				}
			}

			want := request{
				Method:      http.MethodPost,
				Path:        "/protobuf.Comin/Fetch",
				Proto:       "HTTP/2.0",
				ContentType: "application/grpc",
				TE:          "trailers",
				Body:        []byte{0, 0, 0, 0, 0},
			}
			select {
			case got := <-calls:
				if diff := cmp.Diff(want, got); diff != "" {
					t.Errorf("request mismatch (-want +got):\n%s", diff)
				}
			default:
				t.Error("comin received no call")
			}
		})
	}
}

func TestFetchWithoutComin(t *testing.T) {
	t.Parallel()

	client, err := comin.New(filepath.Join(t.TempDir(), "missing.sock"), time.Second)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	if err := client.Fetch(t.Context()); err == nil {
		t.Error("Fetch succeeded with no comin listening")
	}
}

func TestFetchDeadline(t *testing.T) {
	t.Parallel()

	socket, _ := fakeComin(t, func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	client, err := comin.New(socket, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	if err := client.Fetch(t.Context()); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Fetch error = %v, want %v", err, context.DeadlineExceeded)
	}
}

func TestNewNeedsAnAbsolutePath(t *testing.T) {
	t.Parallel()

	if _, err := comin.New("relative.sock", time.Second); err == nil {
		t.Error("New accepted a relative socket path")
	}
}
