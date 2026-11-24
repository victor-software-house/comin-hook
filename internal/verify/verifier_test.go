package verify

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
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
		{name: "empty sub", mutate: func(c map[string]any) { c["sub"] = "" }, wantErr: ErrUnauthenticated},
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

	const (
		reasonAlg      = "token algorithm is not RS256"
		reasonKid      = "token has no key id"
		reasonParts    = "token is not three parts"
		reasonObject   = "token payload is not a JSON object"
		reasonHeader   = "token header is not valid JSON"
		reasonLong     = "token is too long"
		reasonB64Head  = "token header is not base64url"
		reasonB64Sig   = "token signature is not base64url"
		duplicateClaim = `{"iss":"x","sub":"a","sub":"b","jti":"j","exp":4000000000}`
	)
	tests := []struct {
		name       string
		raw        string
		wantReason string
	}{
		{name: "alg none", raw: testissuer.SignWith(t, f.issuer.Key, header("none", testissuer.KeyID), claims), wantReason: reasonAlg},
		{name: "alg HS256", raw: testissuer.SignWith(t, f.issuer.Key, header("HS256", testissuer.KeyID), claims), wantReason: reasonAlg},
		{name: "alg RS512", raw: testissuer.SignWith(t, f.issuer.Key, header("RS512", testissuer.KeyID), claims), wantReason: reasonAlg},
		{name: "alg in lower case", raw: testissuer.SignWith(t, f.issuer.Key, header("rs256", testissuer.KeyID), claims), wantReason: reasonAlg},
		{name: "no alg", raw: testissuer.SignWith(t, f.issuer.Key, header(nil, testissuer.KeyID), claims), wantReason: reasonAlg},
		{name: "no kid", raw: testissuer.SignWith(t, f.issuer.Key, header("RS256", nil), claims), wantReason: reasonKid},
		{name: "empty kid", raw: testissuer.SignWith(t, f.issuer.Key, header("RS256", ""), claims), wantReason: reasonKid},
		{name: "empty", raw: "", wantReason: reasonParts},
		{name: "two parts", raw: valid[:strings.LastIndex(valid, ".")], wantReason: reasonParts},
		{name: "four parts", raw: valid + ".extra", wantReason: reasonParts},
		{name: "padded signature", raw: valid + "=", wantReason: reasonB64Sig},
		{name: "not base64url", raw: "a+b.c/d.e", wantReason: reasonB64Head},
		{name: "header is not JSON", raw: "bm90LWpzb24.bm90LWpzb24.c2ln", wantReason: reasonHeader},
		{name: "too long", raw: valid + strings.Repeat("A", maxTokenBytes), wantReason: reasonLong},
		{
			name:       "a claim name repeated",
			raw:        testissuer.SignRaw(t, f.issuer.Key, `{"alg":"RS256","kid":"`+testissuer.KeyID+`"}`, duplicateClaim),
			wantReason: reasonObject,
		},
		{
			name:       "a header member repeated",
			raw:        testissuer.SignRaw(t, f.issuer.Key, `{"alg":"RS256","alg":"none","kid":"`+testissuer.KeyID+`"}`, `{}`),
			wantReason: reasonHeader,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := f.verifier.Verify(t.Context(), tt.raw)
			if !errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("Verify error = %v, want %v", err, ErrUnauthenticated)
			}
			if !strings.Contains(err.Error(), tt.wantReason) {
				t.Errorf("Verify error = %q, want the reason %q", err, tt.wantReason)
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

func TestVerifyStaleSetWithoutUsableKey(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.clock.advance(maxKeyAge + maxStaleGrace + time.Minute)

	// The issuer now publishes a set with no usable key.
	// That counts as a failed load, so the old keys have no grace left.
	f.issuer.Unpublish(testissuer.KeyID)
	_, err := f.verifier.Verify(t.Context(), f.issuer.Token(t, f.claims(nil)))
	if !errors.Is(err, ErrUnauthenticated) || !strings.Contains(err.Error(), "key set is stale") {
		t.Errorf("Verify error = %v, want %v with a stale key set", err, ErrUnauthenticated)
	}

	// The issuer publishes the key again.
	f.issuer.Publish(testissuer.KeyID, &f.issuer.Key.PublicKey)
	f.clock.advance(time.Minute)
	if _, err := f.verifier.Verify(t.Context(), f.issuer.Token(t, f.claims(nil))); err != nil {
		t.Errorf("Verify after a good load: %v", err)
	}
}

func TestVerifyKeySetAge(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	next := testissuer.NewKey(t)
	oldToken := func() string { return f.issuer.Token(t, f.claims(nil)) }
	nextToken := func() string {
		return testissuer.SignWith(t, next,
			map[string]any{"alg": "RS256", "typ": "JWT", "kid": "next"}, f.claims(nil))
	}

	// The issuer rotates: the first key goes away and a new one comes.
	f.issuer.Publish("next", &next.PublicKey)
	f.issuer.Unpublish(testissuer.KeyID)

	steps := []struct {
		name        string
		advance     time.Duration
		failing     bool
		token       func() string
		wantErr     bool
		wantReason  string
		wantFetches int
	}{
		{name: "a young set serves the old key", advance: 59 * time.Minute, token: oldToken, wantFetches: 1},
		{
			name: "an old set whose reload fails keeps its keys", advance: 2 * time.Minute, failing: true,
			token: oldToken, wantFetches: 2,
		},
		{name: "the failed reload waits out the minute", advance: 30 * time.Second, failing: true, token: oldToken, wantFetches: 2},
		{
			name: "after the minute the reload works and the dropped key stops verifying", advance: 31 * time.Second,
			token: oldToken, wantErr: true, wantFetches: 3,
		},
		{name: "the new key verifies", token: nextToken, wantFetches: 3},
		{name: "the set is young again", advance: 59 * time.Minute, token: nextToken, wantFetches: 3},
		{name: "the next hour loads it again", advance: 2 * time.Minute, token: nextToken, wantFetches: 4},
		{
			name: "a failed reload keeps the keys for a day", advance: 24*time.Hour + 59*time.Minute, failing: true,
			token: nextToken, wantFetches: 5,
		},
		{
			name: "after the day the set is stale and nothing verifies", advance: time.Hour, failing: true,
			token: nextToken, wantErr: true, wantReason: "key set is stale", wantFetches: 6,
		},
		{
			name: "the limit of one load a minute still applies", advance: 10 * time.Second, failing: true,
			token: nextToken, wantErr: true, wantReason: "key set is stale", wantFetches: 6,
		},
		{name: "the first load that works restores service", advance: time.Minute, token: nextToken, wantFetches: 7},
	}
	for _, step := range steps {
		f.clock.advance(step.advance)
		f.issuer.FailJWKS(step.failing)
		_, err := f.verifier.Verify(t.Context(), step.token())
		if (err != nil) != step.wantErr {
			t.Errorf("%s: error = %v, want error: %t", step.name, err, step.wantErr)
		}
		if err != nil && !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("%s: error = %v, want %v", step.name, err, ErrUnauthenticated)
		}
		if err != nil && !strings.Contains(err.Error(), step.wantReason) {
			t.Errorf("%s: error = %q, want the reason %q", step.name, err, step.wantReason)
		}
		if got := f.issuer.JWKSFetches(); got != step.wantFetches {
			t.Errorf("%s: fetches = %d, want %d", step.name, got, step.wantFetches)
		}
	}
}

func TestVerifyReloadSurvivesCanceledRequest(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	rotated := testissuer.NewKey(t)
	f.issuer.Publish("rotated", &rotated.PublicKey)
	token := testissuer.SignWith(t, rotated,
		map[string]any{"alg": "RS256", "typ": "JWT", "kid": "rotated"}, f.claims(nil))

	// The client leaves while the issuer receives the request for the key set.
	ctx, cancel := context.WithCancel(t.Context())
	f.issuer.OnJWKS(cancel)
	if _, err := f.verifier.Verify(ctx, token); err != nil {
		t.Errorf("Verify with a request that ended during the reload: %v", err)
	}
	f.issuer.OnJWKS(nil)

	// The keys loaded in spite of that, and the interval allows no new fetch.
	f.clock.advance(time.Second)
	if _, err := f.verifier.Verify(t.Context(), token); err != nil {
		t.Errorf("Verify after the reload: %v", err)
	}
	if got := f.issuer.JWKSFetches(); got != 2 {
		t.Errorf("key set fetches = %d, want 2: the start and one reload", got)
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
			srv := serveIssuer(t, func() string { return tt.doc(issuerURL) }, tt.status, goodKeySet(t))
			issuerURL = srv.URL

			_, err := newVerifier(t.Context(), srv.URL, testAudience, srv.Client(), time.Now)
			if (err != nil) != tt.wantErr {
				t.Errorf("newVerifier error = %v, want error: %t", err, tt.wantErr)
			}
		})
	}
}

// goodKeySet returns a key set with one usable key.
func goodKeySet(t *testing.T) string {
	t.Helper()
	return `{"keys":[` + keyEntry("k1", "RSA", testissuer.NewKey(t).N) + `]}`
}

// keyEntry returns a key set member with the exponent 65537.
func keyEntry(kid, kty string, modulus *big.Int) string {
	return fmt.Sprintf(`{"kty":%q,"use":"sig","kid":%q,"n":%q,"e":"AQAB"}`,
		kty, kid, base64.RawURLEncoding.EncodeToString(modulus.Bytes()))
}

// serveIssuer serves a discovery document with the status given, and the key
// set at /jwks, over TLS.
func serveIssuer(t *testing.T, doc func() string, status int, jwks string) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			w.WriteHeader(status)
			_, _ = w.Write([]byte(doc()))
		case "/jwks":
			_, _ = w.Write([]byte(jwks))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestNewReadsKeySet(t *testing.T) {
	t.Parallel()

	key := testissuer.NewKey(t)
	good := keyEntry("good", "RSA", key.N)
	tests := []struct {
		name     string
		entries  []string
		wantKids []string
		wantErr  string
	}{
		{name: "one good key", entries: []string{good}, wantKids: []string{"good"}},
		{
			name:     "a bad entry beside a good one",
			entries:  []string{`{"kty":"RSA","use":"sig","kid":"bad-n","n":"!!","e":"AQAB"}`, good},
			wantKids: []string{"good"},
		},
		{
			name:     "an exponent out of range beside a good key",
			entries:  []string{`{"kty":"RSA","kid":"bad-e","n":"AQAB","e":"AA"}`, good},
			wantKids: []string{"good"},
		},
		{
			name:     "other types, uses and unnamed keys are skipped",
			entries:  []string{keyEntry("ec", "EC", key.N), `{"kty":"RSA","use":"enc","kid":"enc","n":"AQAB","e":"AQAB"}`, `{"kty":"RSA","n":"AQAB","e":"AQAB"}`, good},
			wantKids: []string{"good"},
		},
		{
			name:    "only bad entries",
			entries: []string{`{"kty":"RSA","use":"sig","kid":"bad-n","n":"!!","e":"AQAB"}`},
			wantErr: "no usable RSA signature key",
		},
		{name: "no keys", entries: nil, wantErr: "no usable RSA signature key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var issuerURL string
			srv := serveIssuer(t, func() string {
				return `{"issuer":"` + issuerURL + `","jwks_uri":"` + issuerURL + `/jwks"}`
			}, http.StatusOK, `{"keys":[`+strings.Join(tt.entries, ",")+`]}`)
			issuerURL = srv.URL

			v, err := newVerifier(t.Context(), srv.URL, testAudience, srv.Client(), time.Now)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("newVerifier error = %v, want one containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("newVerifier: %v", err)
			}
			var kids []string
			for kid := range v.keys {
				kids = append(kids, kid)
			}
			if diff := cmp.Diff(tt.wantKids, kids); diff != "" {
				t.Errorf("key IDs mismatch (-want +got):\n%s", diff)
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
