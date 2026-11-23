package replay

import (
	"testing"
	"time"
)

func TestSeen(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	type step struct {
		at     time.Duration
		id     string
		expiry time.Duration
		want   bool
	}
	tests := []struct {
		name     string
		steps    []step
		wantHeld int
	}{
		{
			name:     "a new id is not seen",
			steps:    []step{{at: 0, id: "a", expiry: time.Hour, want: false}},
			wantHeld: 1,
		},
		{
			name: "the same id is seen until it expires",
			steps: []step{
				{at: 0, id: "a", expiry: time.Hour, want: false},
				{at: time.Minute, id: "a", expiry: time.Hour, want: true},
				{at: time.Hour - time.Second, id: "a", expiry: time.Hour, want: true},
			},
			wantHeld: 1,
		},
		{
			name: "an expired id is accepted again",
			steps: []step{
				{at: 0, id: "a", expiry: time.Minute, want: false},
				{at: 2 * time.Minute, id: "a", expiry: time.Hour, want: false},
			},
			wantHeld: 1,
		},
		{
			name: "other ids do not collide",
			steps: []step{
				{at: 0, id: "a", expiry: time.Hour, want: false},
				{at: 0, id: "b", expiry: time.Hour, want: false},
				{at: 0, id: "a", expiry: time.Hour, want: true},
			},
			wantHeld: 2,
		},
		{
			name: "expired entries are pruned",
			steps: []step{
				{at: 0, id: "a", expiry: time.Minute, want: false},
				{at: 0, id: "b", expiry: time.Minute, want: false},
				{at: 3 * time.Minute, id: "c", expiry: time.Hour, want: false},
			},
			wantHeld: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var now time.Time
			set := New(func() time.Time { return now })
			for i, s := range tt.steps {
				now = start.Add(s.at)
				if got := set.Seen(s.id, start.Add(s.expiry)); got != s.want {
					t.Errorf("step %d: Seen(%q) = %t, want %t", i, s.id, got, s.want)
				}
			}
			if got := set.Len(); got != tt.wantHeld {
				t.Errorf("Len = %d, want %d", got, tt.wantHeld)
			}
		})
	}
}
