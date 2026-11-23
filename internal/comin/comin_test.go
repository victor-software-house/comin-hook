package comin_test

import (
	"errors"
	"testing"
	"time"

	"github.com/victor-software-house/comin-hook/internal/comin"
	"github.com/victor-software-house/comin-hook/internal/testcomin"
)

func TestFetch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		fetchErr    error
		wantErr     bool
		wantFetches int
	}{
		{name: "comin accepts", wantFetches: 1},
		{name: "comin fails", fetchErr: errors.New("boom"), wantErr: true, wantFetches: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := testcomin.Start(t, tt.fetchErr)
			client, err := comin.New(fake.Socket, time.Second)
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
				t.Errorf("Fetch error = %v, want error: %t", err, tt.wantErr)
			}
			if got := fake.Fetches(); got != tt.wantFetches {
				t.Errorf("fetches = %d, want %d", got, tt.wantFetches)
			}
		})
	}
}

func TestFetchWithoutComin(t *testing.T) {
	t.Parallel()

	client, err := comin.New(t.TempDir()+"/missing.sock", time.Second)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	if err := client.Fetch(t.Context()); err == nil {
		t.Error("Fetch succeeded with no comin listening")
	}
}
