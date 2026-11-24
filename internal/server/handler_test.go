package server_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/victor-software-house/comin-hook/internal/server"
	"github.com/victor-software-house/comin-hook/internal/verify"
)

const (
	hookPath = "/deploy"
	subject  = "repo:example-org/example-repo:ref:refs/heads/main"
)

// stubVerifier accepts the tokens it was given an identity for. Other tokens
// are unauthenticated.
type stubVerifier map[string]verify.Identity

func (s stubVerifier) Verify(_ context.Context, raw string) (verify.Identity, error) {
	id, ok := s[raw]
	if !ok {
		return verify.Identity{}, errors.Join(verify.ErrUnauthenticated, errors.New("unknown token"))
	}
	return id, nil
}

// stubFetcher counts the calls and fails the first ones with errs.
type stubFetcher struct {
	errs  []error
	calls atomic.Int32
}

func (s *stubFetcher) Fetch(context.Context) error {
	call := int(s.calls.Add(1))
	if call <= len(s.errs) {
		return s.errs[call-1]
	}
	return nil
}

func TestHandler(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	identity := func(mutate func(*verify.Identity)) verify.Identity {
		id := verify.Identity{
			Subject: subject,
			ID:      "jti-1",
			Expiry:  now.Add(10 * time.Minute),
			Claims:  map[string]any{"event_name": "push", "ref": "refs/heads/main"},
		}
		if mutate != nil {
			mutate(&id)
		}
		return id
	}
	verifier := stubVerifier{
		"good":          identity(nil),
		"other subject": identity(func(id *verify.Identity) { id.Subject = "repo:example-org/other:ref:refs/heads/main" }),
		"other ref": identity(func(id *verify.Identity) {
			id.Claims = map[string]any{"event_name": "push", "ref": "refs/heads/dev"}
		}),
		"no event": identity(func(id *verify.Identity) { id.Claims = map[string]any{"ref": "refs/heads/main"} }),
	}
	policy := verify.Policy{
		Subjects: []string{subject},
		Claims:   map[string]string{"event_name": "push", "ref": "refs/heads/main"},
	}
	bearer := func(token string) string { return "Bearer " + token }

	tests := []struct {
		name string
		// method and path default to POST and the hook path.
		method      string
		path        string
		auths       []string
		fetchErrs   []error
		wantStatus  []int
		wantFetches int
		wantLogged  []string
	}{
		{
			name:        "accepted",
			auths:       []string{bearer("good")},
			wantStatus:  []int{http.StatusAccepted},
			wantFetches: 1,
			wantLogged:  []string{"reason=accepted", "sub=" + subject},
		},
		{
			name:       "wrong path",
			path:       "/other",
			auths:      []string{bearer("good")},
			wantStatus: []int{http.StatusNotFound},
			wantLogged: []string{"reason=not_found"},
		},
		{
			name:       "path with a suffix",
			path:       hookPath + "/",
			auths:      []string{bearer("good")},
			wantStatus: []int{http.StatusNotFound},
		},
		{
			name:       "wrong method",
			method:     http.MethodGet,
			auths:      []string{bearer("good")},
			wantStatus: []int{http.StatusMethodNotAllowed},
			wantLogged: []string{"reason=method_not_allowed"},
		},
		{
			name:       "no token",
			auths:      []string{""},
			wantStatus: []int{http.StatusUnauthorized},
			wantLogged: []string{"reason=missing_token"},
		},
		{
			name:       "not a bearer header",
			auths:      []string{"Basic good"},
			wantStatus: []int{http.StatusUnauthorized},
		},
		{
			name:       "the verifier rejects the token",
			auths:      []string{bearer("forged")},
			wantStatus: []int{http.StatusUnauthorized},
			wantLogged: []string{"reason=invalid_token", "unauthenticated", "unknown token"},
		},
		{
			name:       "wrong subject",
			auths:      []string{bearer("other subject")},
			wantStatus: []int{http.StatusForbidden},
			wantLogged: []string{"reason=forbidden", "sub=repo:example-org/other:ref:refs/heads/main"},
		},
		{
			name:       "wrong claim",
			auths:      []string{bearer("other ref")},
			wantStatus: []int{http.StatusForbidden},
			wantLogged: []string{"reason=forbidden", "claim"},
		},
		{
			name:       "missing claim",
			auths:      []string{bearer("no event")},
			wantStatus: []int{http.StatusForbidden},
		},
		{
			name:        "replayed jti",
			auths:       []string{bearer("good"), bearer("good")},
			wantStatus:  []int{http.StatusAccepted, http.StatusConflict},
			wantFetches: 1,
			wantLogged:  []string{"reason=replayed"},
		},
		{
			name:        "comin fails",
			auths:       []string{bearer("good")},
			fetchErrs:   []error{errors.New("boom")},
			wantStatus:  []int{http.StatusBadGateway},
			wantFetches: 1,
			wantLogged:  []string{"reason=fetch_failed", "boom"},
		},
		{
			name:        "the same token tries again after comin failed",
			auths:       []string{bearer("good"), bearer("good"), bearer("good")},
			fetchErrs:   []error{errors.New("boom")},
			wantStatus:  []int{http.StatusBadGateway, http.StatusAccepted, http.StatusConflict},
			wantFetches: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fetcher := &stubFetcher{errs: tt.fetchErrs}
			var logs strings.Builder
			handler := server.NewHandler(server.Options{
				Path:     hookPath,
				Verifier: verifier,
				Policy:   policy,
				Fetcher:  fetcher,
				Logger:   slog.New(slog.NewTextHandler(&logs, nil)),
				Now:      func() time.Time { return now },
			})

			method, path := tt.method, tt.path
			if method == "" {
				method = http.MethodPost
			}
			if path == "" {
				path = hookPath
			}
			var gotStatus []int
			for _, auth := range tt.auths {
				req := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader("ignored"))
				if auth != "" {
					req.Header.Set("Authorization", auth)
				}
				got := httptest.NewRecorder()
				handler.ServeHTTP(got, req)
				gotStatus = append(gotStatus, got.Code)
			}

			if diff := cmp.Diff(
				[]any{tt.wantStatus, tt.wantFetches},
				[]any{gotStatus, int(fetcher.calls.Load())},
			); diff != "" {
				t.Errorf("statuses and fetches mismatch (-want +got):\n%s", diff)
			}
			for _, want := range tt.wantLogged {
				if !strings.Contains(logs.String(), want) {
					t.Errorf("the log lacks %q: %s", want, logs.String())
				}
			}
			for _, auth := range tt.auths {
				if token := strings.TrimPrefix(strings.TrimPrefix(auth, "Bearer "), "Basic "); token != "" && strings.Contains(logs.String(), token) {
					t.Errorf("the log contains the token %q: %s", token, logs.String())
				}
			}
		})
	}
}
