package verify_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/victor-software-house/comin-hook/internal/testissuer"
	"github.com/victor-software-house/comin-hook/internal/verify"
)

func TestPolicyAllow(t *testing.T) {
	t.Parallel()

	policy := verify.Policy{
		Subjects: []string{"sub-a", "sub-b"},
		Claims:   map[string]string{"event_name": "push", "ref": "refs/heads/main"},
	}
	push := map[string]any{"event_name": "push", "ref": "refs/heads/main"}

	tests := []struct {
		name string
		id   verify.Identity
		want error
	}{
		{name: "allowed", id: verify.Identity{Subject: "sub-b", Claims: push}},
		{name: "other subject", id: verify.Identity{Subject: "sub-c", Claims: push}, want: verify.ErrForbidden},
		{
			name: "claim differs",
			id:   verify.Identity{Subject: "sub-a", Claims: map[string]any{"event_name": "push", "ref": "refs/heads/dev"}},
			want: verify.ErrForbidden,
		},
		{
			name: "claim missing",
			id:   verify.Identity{Subject: "sub-a", Claims: map[string]any{"event_name": "push"}},
			want: verify.ErrForbidden,
		},
		{
			name: "claim is not a string",
			id:   verify.Identity{Subject: "sub-a", Claims: map[string]any{"event_name": "push", "ref": 1}},
			want: verify.ErrForbidden,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := policy.Allow(tt.id); !errors.Is(got, tt.want) {
				t.Errorf("Allow error = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestVerify(t *testing.T) {
	t.Parallel()

	issuer := testissuer.New(t)
	verifier, err := verify.New(t.Context(), issuer.URL, "audience")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	expiry := time.Now().Add(time.Hour).Truncate(time.Second)
	claims := func(mutate func(map[string]any)) map[string]any {
		c := map[string]any{
			"iss": issuer.URL, "aud": "audience", "sub": "sub-a", "jti": "id-1",
			"exp": expiry.Unix(), "extra": "value",
		}
		mutate(c)
		return c
	}

	tests := []struct {
		name    string
		mutate  func(map[string]any)
		want    verify.Identity
		wantErr error
	}{
		{
			name:   "valid token",
			mutate: func(map[string]any) {},
			want: verify.Identity{
				Subject: "sub-a", ID: "id-1", Expiry: expiry,
				Claims: map[string]any{
					"iss": issuer.URL, "aud": "audience", "sub": "sub-a", "jti": "id-1",
					"exp": float64(expiry.Unix()), "extra": "value",
				},
			},
		},
		{name: "no jti", mutate: func(c map[string]any) { delete(c, "jti") }, wantErr: verify.ErrUnauthenticated},
		{name: "no exp", mutate: func(c map[string]any) { delete(c, "exp") }, wantErr: verify.ErrUnauthenticated},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := verifier.Verify(t.Context(), issuer.Token(t, claims(tt.mutate)))
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Verify error = %v, want %v", err, tt.wantErr)
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("identity mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
