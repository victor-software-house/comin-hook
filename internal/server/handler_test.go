package server_test

import (
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/victor-software-house/comin-hook/internal/comin"
	"github.com/victor-software-house/comin-hook/internal/server"
	"github.com/victor-software-house/comin-hook/internal/testcomin"
	"github.com/victor-software-house/comin-hook/internal/testissuer"
	"github.com/victor-software-house/comin-hook/internal/verify"
)

const (
	hookPath = "/deploy"
	audience = "https://example.com/hook"
	subject  = "repo:example-org/example-repo:ref:refs/heads/main"
)

func TestHandler(t *testing.T) {
	t.Parallel()

	issuer := testissuer.New(t)
	otherKey := testissuer.NewKey(t)
	verifier, err := verify.New(t.Context(), issuer.URL, audience)
	if err != nil {
		t.Fatalf("verify.New: %v", err)
	}
	policy := verify.Policy{
		Subjects: []string{subject},
		Claims:   map[string]string{"event_name": "push", "ref": "refs/heads/main"},
	}
	claims := func(mutate func(map[string]any)) map[string]any {
		c := map[string]any{
			"iss": issuer.URL, "aud": audience, "sub": subject, "jti": "jti-1",
			"iat": time.Now().Unix(), "exp": time.Now().Add(10 * time.Minute).Unix(),
			"event_name": "push", "ref": "refs/heads/main",
		}
		if mutate != nil {
			mutate(c)
		}
		return c
	}
	bearer := func(token string) string { return "Bearer " + token }

	tests := []struct {
		name        string
		method      string
		path        string
		auth        string
		fetchErr    error
		requests    int
		wantStatus  int
		wantFetches int
	}{
		{
			name:        "accepted",
			auth:        bearer(issuer.Token(t, claims(nil))),
			wantStatus:  http.StatusAccepted,
			wantFetches: 1,
		},
		{
			name:       "wrong path",
			path:       "/other",
			auth:       bearer(issuer.Token(t, claims(nil))),
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "path with a suffix",
			path:       hookPath + "/",
			auth:       bearer(issuer.Token(t, claims(nil))),
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "wrong method",
			method:     http.MethodGet,
			auth:       bearer(issuer.Token(t, claims(nil))),
			wantStatus: http.StatusMethodNotAllowed,
		},
		{
			name:       "no token",
			auth:       "",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "not a bearer header",
			auth:       "Basic " + issuer.Token(t, claims(nil)),
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "not a token",
			auth:       bearer("not-a-jwt"),
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "bad signature",
			auth:       bearer(issuer.Sign(t, otherKey, claims(nil))),
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "wrong issuer",
			auth:       bearer(issuer.Token(t, claims(func(c map[string]any) { c["iss"] = "https://other.example.com" }))),
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "wrong audience",
			auth:       bearer(issuer.Token(t, claims(func(c map[string]any) { c["aud"] = "https://example.com/other" }))),
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "expired",
			auth:       bearer(issuer.Token(t, claims(func(c map[string]any) { c["exp"] = time.Now().Add(-time.Hour).Unix() }))),
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "no jti",
			auth:       bearer(issuer.Token(t, claims(func(c map[string]any) { delete(c, "jti") }))),
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "wrong subject",
			auth:       bearer(issuer.Token(t, claims(func(c map[string]any) { c["sub"] = "repo:example-org/other:ref:refs/heads/main" }))),
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "wrong claim",
			auth:       bearer(issuer.Token(t, claims(func(c map[string]any) { c["ref"] = "refs/heads/dev" }))),
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "missing claim",
			auth:       bearer(issuer.Token(t, claims(func(c map[string]any) { delete(c, "event_name") }))),
			wantStatus: http.StatusForbidden,
		},
		{
			name:        "replayed jti",
			auth:        bearer(issuer.Token(t, claims(nil))),
			requests:    2,
			wantStatus:  http.StatusConflict,
			wantFetches: 1,
		},
		{
			name:        "comin fails",
			auth:        bearer(issuer.Token(t, claims(nil))),
			fetchErr:    errors.New("boom"),
			wantStatus:  http.StatusBadGateway,
			wantFetches: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := testcomin.Start(t, tt.fetchErr)
			fetcher, err := comin.New(fake.Socket, time.Second)
			if err != nil {
				t.Fatalf("comin.New: %v", err)
			}
			t.Cleanup(func() { _ = fetcher.Close() })

			var logs strings.Builder
			handler := server.NewHandler(server.Options{
				Path:     hookPath,
				Verifier: verifier,
				Policy:   policy,
				Fetcher:  fetcher,
				Logger:   slog.New(slog.NewTextHandler(&logs, nil)),
				Now:      time.Now,
			})

			method, path := tt.method, tt.path
			if method == "" {
				method = http.MethodPost
			}
			if path == "" {
				path = hookPath
			}
			auth := tt.auth
			requests := max(tt.requests, 1)

			var got *httptest.ResponseRecorder
			for range requests {
				req := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader("ignored"))
				if auth != "" {
					req.Header.Set("Authorization", auth)
				}
				got = httptest.NewRecorder()
				handler.ServeHTTP(got, req)
			}

			if diff := cmp.Diff(
				[]int{tt.wantStatus, tt.wantFetches},
				[]int{got.Code, fake.Fetches()},
			); diff != "" {
				t.Errorf("status and fetches mismatch (-want +got):\n%s", diff)
			}
			if token := strings.TrimPrefix(auth, "Bearer "); token != "" && strings.Contains(logs.String(), token) {
				t.Errorf("the log contains the token: %s", logs.String())
			}
			if tt.wantStatus == http.StatusAccepted && !strings.Contains(logs.String(), subject) {
				t.Errorf("the log lacks the subject: %s", logs.String())
			}
		})
	}
}
