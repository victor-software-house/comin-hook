// Package testissuer runs an OIDC issuer for tests.
// It is an HTTPS server with a discovery document and a key set.
// It signs tokens with the keys that the caller picks, and counts how often
// its key set was fetched.
package testissuer

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/v2"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

// KeyID is the ID of the key that every issuer publishes first.
const KeyID = "test-key"

// Issuer is a running test issuer.
type Issuer struct {
	// URL is the issuer URL, which a token carries as its iss claim.
	URL string
	// Client trusts the certificate of the issuer.
	Client *http.Client
	// Key signs the tokens that Token returns.
	Key *rsa.PrivateKey

	jwksFetches atomic.Int32

	mu         sync.Mutex
	keys       map[string]*rsa.PublicKey
	onJWKSHook func()
}

// New starts an issuer that lives until the test ends.
func New(tb testing.TB) *Issuer {
	tb.Helper()
	issuer := &Issuer{Key: NewKey(tb), keys: make(map[string]*rsa.PublicKey)}
	issuer.keys[KeyID] = &issuer.Key.PublicKey

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"issuer":                                issuer.URL,
			"authorization_endpoint":                issuer.URL + "/authorize",
			"token_endpoint":                        issuer.URL + "/token",
			"jwks_uri":                              issuer.URL + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		issuer.jwksFetches.Add(1)
		issuer.mu.Lock()
		hook := issuer.onJWKSHook
		issuer.mu.Unlock()
		if hook != nil {
			hook()
		}
		writeJSON(w, issuer.keySet())
	})
	srv := httptest.NewTLSServer(mux)
	tb.Cleanup(srv.Close)
	issuer.URL = srv.URL
	issuer.Client = srv.Client()
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

// Publish adds a key to the key set of the issuer, from now on.
func (i *Issuer) Publish(kid string, key *rsa.PublicKey) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.keys[kid] = key
}

// OnJWKS sets a function that runs when a request for the key set arrives,
// before the issuer answers it.
func (i *Issuer) OnJWKS(hook func()) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.onJWKSHook = hook
}

// JWKSFetches is the number of times the key set was fetched so far.
func (i *Issuer) JWKSFetches() int {
	return int(i.jwksFetches.Load())
}

// Token signs claims with the issuer's key, under the key ID it publishes.
func (i *Issuer) Token(tb testing.TB, claims map[string]any) string {
	tb.Helper()
	return i.Sign(tb, i.Key, claims)
}

// Sign signs claims with key, under the key ID that the issuer publishes first.
func (i *Issuer) Sign(tb testing.TB, key *rsa.PrivateKey, claims map[string]any) string {
	tb.Helper()
	return SignWith(tb, key, map[string]any{"alg": "RS256", "typ": "JWT", "kid": KeyID}, claims)
}

// SignWith signs claims with key, under the header that the caller gives.
// The signature always uses SHA-256, whatever alg the header names.
// A test can thus build a token whose alg does not match its signature.
func SignWith(tb testing.TB, key *rsa.PrivateKey, header, claims map[string]any) string {
	tb.Helper()
	input := encodePart(tb, header) + "." + encodePart(tb, claims)
	digest := sha256.Sum256([]byte(input))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		tb.Fatalf("signing the token: %v", err)
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(signature)
}

// SignRaw signs the given header and payload texts as they are, so a test can
// build a token whose JSON is malformed or repeats a member name.
// The signature uses SHA-256.
func SignRaw(tb testing.TB, key *rsa.PrivateKey, header, payload string) string {
	tb.Helper()
	input := base64.RawURLEncoding.EncodeToString([]byte(header)) + "." +
		base64.RawURLEncoding.EncodeToString([]byte(payload))
	digest := sha256.Sum256([]byte(input))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		tb.Fatalf("signing the token: %v", err)
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func encodePart(tb testing.TB, part map[string]any) string {
	tb.Helper()
	raw, err := json.Marshal(part)
	if err != nil {
		tb.Fatalf("encoding a token part: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

type jwk struct {
	Kty string `json:"kty"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	N   string `json:"n"`
	E   string `json:"e"`
}

func (i *Issuer) keySet() map[string]any {
	i.mu.Lock()
	defer i.mu.Unlock()
	keys := make([]jwk, 0, len(i.keys))
	for kid, key := range i.keys {
		keys = append(keys, jwk{
			Kty: "RSA",
			Use: "sig",
			Alg: "RS256",
			Kid: kid,
			N:   base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			E:   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		})
	}
	return map[string]any{"keys": keys}
}

func writeJSON(w http.ResponseWriter, v any) {
	doc, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(doc)
}
