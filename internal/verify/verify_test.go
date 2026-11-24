package verify_test

import (
	"errors"
	"testing"

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
