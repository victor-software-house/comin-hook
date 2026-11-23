// Package testissuer runs an OIDC issuer for tests.
// It is an HTTP server with a discovery document and a public key.
// It signs tokens with the keys that the caller picks.
package testissuer

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

const keyID = "test-key"

// Issuer is a running test issuer.
type Issuer struct {
	// URL is the issuer URL, which a token carries as its iss claim.
	URL string
	// Key signs the tokens that Token returns.
	Key *rsa.PrivateKey
}

// New starts an issuer that lives until the test ends.
func New(tb testing.TB) *Issuer {
	tb.Helper()
	issuer := &Issuer{Key: NewKey(tb)}

	jwks, err := json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key:       &issuer.Key.PublicKey,
		KeyID:     keyID,
		Algorithm: string(jose.RS256),
		Use:       "sig",
	}}})
	if err != nil {
		tb.Fatalf("encoding the key set: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		doc, err := json.Marshal(map[string]any{
			"issuer":                                issuer.URL,
			"authorization_endpoint":                issuer.URL + "/authorize",
			"token_endpoint":                        issuer.URL + "/token",
			"jwks_uri":                              issuer.URL + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(doc)
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jwks)
	})
	srv := httptest.NewServer(mux)
	tb.Cleanup(srv.Close)
	issuer.URL = srv.URL
	return issuer
}

// NewKey returns a signing key that no issuer publishes.
func NewKey(tb testing.TB) *rsa.PrivateKey {
	tb.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		tb.Fatalf("generating an RSA key: %v", err)
	}
	return key
}

// Token signs claims with the issuer's key.
func (i *Issuer) Token(tb testing.TB, claims map[string]any) string {
	tb.Helper()
	return i.Sign(tb, i.Key, claims)
}

// Sign signs claims with key, under the key ID that the issuer publishes.
func (i *Issuer) Sign(tb testing.TB, key *rsa.PrivateKey, claims map[string]any) string {
	tb.Helper()
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", keyID),
	)
	if err != nil {
		tb.Fatalf("creating a signer: %v", err)
	}
	raw, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		tb.Fatalf("signing the claims: %v", err)
	}
	return raw
}
