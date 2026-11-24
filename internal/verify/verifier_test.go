package verify

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/victor-software-house/comin-hook/internal/testissuer"
)

const testAudience = "audience"

// clock is a settable time source.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// fixture is a verifier of a test issuer, with the clock of the verifier.
type fixture struct {
	issuer   *testissuer.Issuer
	verifier *Verifier
	clock    *clock
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	issuer := testissuer.New(t)
	clk := &clock{t: time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)}
	verifier, err := newVerifier(t.Context(), issuer.URL, testAudience, issuer.Client, clk.now)
	if err != nil {
		t.Fatalf("newVerifier: %v", err)
	}
	return fixture{issuer: issuer, verifier: verifier, clock: clk}
}

// claims returns valid claims at the time of the fixture, changed by mutate.
func (f fixture) claims(mutate func(map[string]any)) map[string]any {
	now := f.clock.now()
	c := map[string]any{
		"iss": f.issuer.URL, "aud": testAudience, "sub": "sub-a", "jti": "id-1",
		"iat": now.Unix(), "nbf": now.Unix(), "exp": now.Add(10 * time.Minute).Unix(),
		"extra": "value",
	}
	if mutate != nil {
		mutate(c)
	}
	return c
}

func TestVerify(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	now := f.clock.now()
	expiry := now.Add(10 * time.Minute)

	tests := []struct {
		name    string
		mutate  func(map[string]any)
		want    Identity
		wantErr error
	}{
		{
			name: "valid token",
			want: Identity{
				Subject: "sub-a", ID: "id-1", Expiry: expiry,
				Claims: map[string]any{
					"iss": f.issuer.URL, "aud": testAudience, "sub": "sub-a", "jti": "id-1",
					"iat": float64(now.Unix()), "nbf": float64(now.Unix()), "exp": float64(expiry.Unix()),
					"extra": "value",
				},
			},
		},
		{
			name:   "audience in a list",
			mutate: func(c map[string]any) { c["aud"] = []string{"other", testAudience} },
			want: Identity{
				Subject: "sub-a", ID: "id-1", Expiry: expiry,
				Claims: map[string]any{
					"iss": f.issuer.URL, "aud": []any{"other", testAudience}, "sub": "sub-a", "jti": "id-1",
					"iat": float64(now.Unix()), "nbf": float64(now.Unix()), "exp": float64(expiry.Unix()),
					"extra": "value",
				},
			},
		},
		{
			name:   "nbf a little ahead",
			mutate: func(c map[string]any) { c["nbf"] = now.Add(59 * time.Second).Unix() },
			want: Identity{
				Subject: "sub-a", ID: "id-1", Expiry: expiry,
				Claims: map[string]any{
					"iss": f.issuer.URL, "aud": testAudience, "sub": "sub-a", "jti": "id-1",
					"iat": float64(now.Unix()), "nbf": float64(now.Add(59 * time.Second).Unix()), "exp": float64(expiry.Unix()),
					"extra": "value",
				},
			},
		},
		{name: "wrong issuer", mutate: func(c map[string]any) { c["iss"] = "https://other.example.com" }, wantErr: ErrUnauthenticated},
		{name: "wrong audience", mutate: func(c map[string]any) { c["aud"] = "other" }, wantErr: ErrUnauthenticated},
		{name: "audience list without it", mutate: func(c map[string]any) { c["aud"] = []string{"a", "b"} }, wantErr: ErrUnauthenticated},
		{name: "no audience", mutate: func(c map[string]any) { delete(c, "aud") }, wantErr: ErrUnauthenticated},
		{name: "expired", mutate: func(c map[string]any) { c["exp"] = now.Add(-time.Second).Unix() }, wantErr: ErrUnauthenticated},
		{name: "expires now", mutate: func(c map[string]any) { c["exp"] = now.Unix() }, wantErr: ErrUnauthenticated},
		{name: "no exp", mutate: func(c map[string]any) { delete(c, "exp") }, wantErr: ErrUnauthenticated},
		{name: "exp is a string", mutate: func(c map[string]any) { c["exp"] = "soon" }, wantErr: ErrUnauthenticated},
		{
			name:    "nbf in the future",
			mutate:  func(c map[string]any) { c["nbf"] = now.Add(61 * time.Second).Unix() },
			wantErr: ErrUnauthenticated,
		},
		{
			name:    "iat in the future",
			mutate:  func(c map[string]any) { c["iat"] = now.Add(time.Hour).Unix() },
			wantErr: ErrUnauthenticated,
		},
		{name: "no jti", mutate: func(c map[string]any) { delete(c, "jti") }, wantErr: ErrUnauthenticated},
		{name: "empty jti", mutate: func(c map[string]any) { c["jti"] = "" }, wantErr: ErrUnauthenticated},
		{name: "sub is a number", mutate: func(c map[string]any) { c["sub"] = 7 }, wantErr: ErrUnauthenticated},
		{name: "no sub", mutate: func(c map[string]any) { delete(c, "sub") }, wantErr: ErrUnauthenticated},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := f.verifier.Verify(t.Context(), f.issuer.Token(t, f.claims(tt.mutate)))
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Verify error = %v, want %v", err, tt.wantErr)
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("identity mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestVerifyRejectsTokenForm(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	claims := f.claims(nil)
	header := func(alg, kid any) map[string]any {
		h := map[string]any{"typ": "JWT"}
		if alg != nil {
			h["alg"] = alg
		}
		if kid != nil {
			h["kid"] = kid
		}
		return h
	}
	valid := f.issuer.Token(t, claims)

	tests := []struct {
		name string
		raw  string
	}{
		{name: "alg none", raw: testissuer.SignWith(t, f.issuer.Key, header("none", testissuer.KeyID), claims)},
		{name: "alg HS256", raw: testissuer.SignWith(t, f.issuer.Key, header("HS256", testissuer.KeyID), claims)},
		{name: "alg RS512", raw: testissuer.SignWith(t, f.issuer.Key, header("RS512", testissuer.KeyID), claims)},
		{name: "alg in lower case", raw: testissuer.SignWith(t, f.issuer.Key, header("rs256", testissuer.KeyID), claims)},
		{name: "no alg", raw: testissuer.SignWith(t, f.issuer.Key, header(nil, testissuer.KeyID), claims)},
		{name: "no kid", raw: testissuer.SignWith(t, f.issuer.Key, header("RS256", nil), claims)},
		{name: "empty kid", raw: testissuer.SignWith(t, f.issuer.Key, header("RS256", ""), claims)},
		{name: "empty", raw: ""},
		{name: "two parts", raw: valid[:strings.LastIndex(valid, ".")]},
		{name: "four parts", raw: valid + ".extra"},
		{name: "padded signature", raw: valid + "="},
		{name: "not base64url", raw: "a+b.c/d.e"},
		{name: "header is not JSON", raw: "bm90LWpzb24.bm90LWpzb24.c2ln"},
		{name: "too long", raw: valid + strings.Repeat("A", maxTokenBytes)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := f.verifier.Verify(t.Context(), tt.raw); !errors.Is(err, ErrUnauthenticated) {
				t.Errorf("Verify error = %v, want %v", err, ErrUnauthenticated)
			}
		})
	}
	if got := f.issuer.JWKSFetches(); got != 1 {
		t.Errorf("key set fetches = %d, want 1: a malformed token never fetches", got)
	}
}

func TestVerifyKeyFetches(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	rotated := testissuer.NewKey(t)
	rotatedToken := func() string {
		return testissuer.SignWith(t, rotated,
			map[string]any{"alg": "RS256", "typ": "JWT", "kid": "rotated"}, f.claims(nil))
	}
	verify := func(token string) error {
		_, err := f.verifier.Verify(t.Context(), token)
		return err
	}

	// The load in newVerifier is the first fetch.
	if got := f.issuer.JWKSFetches(); got != 1 {
		t.Fatalf("fetches after start = %d, want 1", got)
	}

	// A known key never fetches, and a bad signature never does either.
	if err := verify(f.issuer.Token(t, f.claims(nil))); err != nil {
		t.Fatalf("valid token: %v", err)
	}
	if err := verify(f.issuer.Sign(t, testissuer.NewKey(t), f.claims(nil))); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("bad signature error = %v, want %v", err, ErrUnauthenticated)
	}
	if got := f.issuer.JWKSFetches(); got != 1 {
		t.Fatalf("fetches after known keys = %d, want 1", got)
	}

	steps := []struct {
		name        string
		advance     time.Duration
		publish     bool
		wantErr     bool
		wantFetches int
	}{
		{name: "the first unknown key fetches at once", wantErr: true, wantFetches: 2},
		{name: "a second one a moment later does not", advance: 10 * time.Second, publish: true, wantErr: true, wantFetches: 2},
		{name: "still inside the minute", advance: 49 * time.Second, wantErr: true, wantFetches: 2},
		{name: "after the minute it fetches and finds the key", advance: 2 * time.Second, wantFetches: 3},
		{name: "the key is known now", wantFetches: 3},
	}
	for _, step := range steps {
		f.clock.advance(step.advance)
		if step.publish {
			f.issuer.Publish("rotated", &rotated.PublicKey)
		}
		err := verify(rotatedToken())
		if (err != nil) != step.wantErr {
			t.Errorf("%s: error = %v, want error: %t", step.name, err, step.wantErr)
		}
		if got := f.issuer.JWKSFetches(); got != step.wantFetches {
			t.Errorf("%s: fetches = %d, want %d", step.name, got, step.wantFetches)
		}
	}
}

func TestNewChecksDiscovery(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		doc     func(issuer string) string
		status  int
		wantErr bool
	}{
		{
			name:   "a good document",
			doc:    func(issuer string) string { return `{"issuer":"` + issuer + `","jwks_uri":"` + issuer + `/jwks"}` },
			status: http.StatusOK,
		},
		{
			name: "another issuer",
			doc: func(string) string {
				return `{"issuer":"https://other.example.com","jwks_uri":"https://keys.example.com/jwks"}`
			},
			status:  http.StatusOK,
			wantErr: true,
		},
		{
			name: "the issuer with a slash",
			doc: func(issuer string) string {
				return `{"issuer":"` + issuer + `/","jwks_uri":"https://keys.example.com/jwks"}`
			},
			status:  http.StatusOK,
			wantErr: true,
		},
		{
			name: "keys over http",
			doc: func(issuer string) string {
				return `{"issuer":"` + issuer + `","jwks_uri":"http://keys.example.com/jwks"}`
			},
			status:  http.StatusOK,
			wantErr: true,
		},
		{
			name:    "no keys uri",
			doc:     func(issuer string) string { return `{"issuer":"` + issuer + `"}` },
			status:  http.StatusOK,
			wantErr: true,
		},
		{name: "not found", doc: func(string) string { return "{}" }, status: http.StatusNotFound, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var issuerURL string
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/.well-known/openid-configuration" {
					w.WriteHeader(tt.status)
					_, _ = w.Write([]byte(tt.doc(issuerURL)))
					return
				}
				if r.URL.Path == "/jwks" {
					_, _ = w.Write([]byte(`{"keys":[]}`))
					return
				}
				http.NotFound(w, r)
			}))
			t.Cleanup(srv.Close)
			issuerURL = srv.URL

			_, err := newVerifier(t.Context(), srv.URL, testAudience, srv.Client(), time.Now)
			if (err != nil) != tt.wantErr {
				t.Errorf("newVerifier error = %v, want error: %t", err, tt.wantErr)
			}
		})
	}
}

func FuzzParseToken(f *testing.F) {
	f.Add("eyJhbGciOiJSUzI1NiIsImtpZCI6ImsifQ.eyJzdWIiOiJzIn0.c2ln")
	f.Add("eyJhbGciOiJub25lIiwia2lkIjoiayJ9.eyJzdWIiOiJzIn0.")
	f.Add("a.b.c")
	f.Add("..")
	f.Add("")
	f.Fuzz(func(t *testing.T, raw string) {
		tok, err := parseToken(raw)
		if err != nil {
			if !errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("parseToken error = %v, want %v", err, ErrUnauthenticated)
			}
			return
		}
		if tok.kid == "" || tok.claims == nil || strings.Count(tok.signingInput, ".") != 1 || !strings.HasPrefix(raw, tok.signingInput+".") {
			t.Fatalf("parseToken(%q) accepted a malformed token: %+v", raw, tok)
		}
	})
}
