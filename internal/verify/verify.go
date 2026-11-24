// Package verify checks a CI job's OIDC token and decides whether its holder
// may trigger a fetch.
//
// It reads the issuer's discovery document and signing keys with the standard
// library, and accepts only RS256 tokens.
package verify

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

// ErrUnauthenticated marks a token that is not a valid token of the issuer
// for this audience.
var ErrUnauthenticated = errors.New("unauthenticated")

// ErrForbidden marks a valid token whose subject or claims are not allowed.
var ErrForbidden = errors.New("forbidden")

// Limits of the requests to the issuer and of the tokens.
const (
	// httpTimeout bounds each request to the issuer's discovery and key endpoints.
	httpTimeout = 10 * time.Second
	// maxBodyBytes bounds the body of the issuer's responses.
	maxBodyBytes = 1 << 20
	// maxTokenBytes bounds the length of a token.
	maxTokenBytes = 8 << 10
	// minRefetchInterval is the shortest time between two fetches of the key
	// set that an unknown key ID causes.
	minRefetchInterval = time.Minute
	// timeLeeway is how far in the future an nbf or iat claim may be.
	timeLeeway = time.Minute
	// maxNumericDate bounds the seconds of a date claim, so that converting
	// it to a time cannot overflow.
	maxNumericDate = 1 << 40
)

// algRS256 is the one signing algorithm the verifier accepts.
const algRS256 = "RS256"

// Identity is what a verified token shows about its holder.
type Identity struct {
	// Subject is the sub claim.
	Subject string
	// ID is the jti claim, which a token carries once.
	ID string
	// Expiry is the exp claim.
	Expiry time.Time
	// Claims are all claims of the token.
	Claims map[string]any
}

// Verifier checks tokens of one issuer for one audience.
type Verifier struct {
	issuer   string
	audience string
	jwksURL  string
	client   *http.Client
	now      func() time.Time

	// mu guards the fields below. It is never held during a request.
	mu        sync.Mutex
	keys      map[string]*rsa.PublicKey
	lastFetch time.Time
}

// New discovers the issuer's configuration, loads its signing keys, and
// returns a verifier that accepts RS256 tokens for audience.
// ctx bounds only this start.
// Afterwards, a token with an unknown key ID makes the verifier fetch the key
// set again, at most once a minute across all requests.
func New(ctx context.Context, issuer, audience string) (*Verifier, error) {
	client := &http.Client{
		Timeout: httpTimeout,
		// A redirect could lead from https to another scheme.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return newVerifier(ctx, issuer, audience, client, time.Now)
}

func newVerifier(
	ctx context.Context,
	issuer, audience string,
	client *http.Client,
	now func() time.Time,
) (*Verifier, error) {
	v := &Verifier{issuer: issuer, audience: audience, client: client, now: now}

	var doc struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}
	if err := v.getJSON(ctx, strings.TrimSuffix(issuer, "/")+"/.well-known/openid-configuration", &doc); err != nil {
		return nil, fmt.Errorf("discovering OIDC issuer %s: %w", issuer, err)
	}
	if doc.Issuer != issuer {
		return nil, fmt.Errorf("discovering OIDC issuer %s: the document names issuer %q", issuer, doc.Issuer)
	}
	jwksURL, err := url.Parse(doc.JWKSURI)
	if err != nil || jwksURL.Scheme != "https" || jwksURL.Host == "" {
		return nil, fmt.Errorf("discovering OIDC issuer %s: jwks_uri %q is not an https URL", issuer, doc.JWKSURI)
	}
	v.jwksURL = jwksURL.String()

	// This first load does not count toward the refetch interval, so the
	// first unknown key ID can still fetch.
	if v.keys, err = v.fetchKeys(ctx); err != nil {
		return nil, fmt.Errorf("loading the keys of OIDC issuer %s: %w", issuer, err)
	}
	return v, nil
}

// getJSON decodes the JSON body of a successful GET response into out.
func (v *Verifier) getJSON(ctx context.Context, target string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, http.NoBody)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return fmt.Errorf("fetching: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetching %s: status %d", target, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return fmt.Errorf("reading %s: %w", target, err)
	}
	if len(body) > maxBodyBytes {
		return fmt.Errorf("reading %s: the body exceeds %d bytes", target, maxBodyBytes)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decoding %s: %w", target, err)
	}
	return nil
}

// fetchKeys returns the signature keys of the issuer by key ID.
// It skips keys of another type or use.
func (v *Verifier) fetchKeys(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	var set struct {
		Keys []struct {
			Kty string `json:"kty"`
			Use string `json:"use"`
			Kid string `json:"kid"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := v.getJSON(ctx, v.jwksURL, &set); err != nil {
		return nil, err
	}
	keys := make(map[string]*rsa.PublicKey, len(set.Keys))
	for _, k := range set.Keys {
		if k.Kty != "RSA" || (k.Use != "sig" && k.Use != "") || k.Kid == "" {
			continue
		}
		n, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			return nil, fmt.Errorf("key %q: decoding n: %w", k.Kid, err)
		}
		e, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil {
			return nil, fmt.Errorf("key %q: decoding e: %w", k.Kid, err)
		}
		exponent := new(big.Int).SetBytes(e)
		if !exponent.IsInt64() || exponent.Int64() < 2 || exponent.Int64() > math.MaxInt32 {
			return nil, fmt.Errorf("key %q: the exponent is out of range", k.Kid)
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(exponent.Int64())}
	}
	return keys, nil
}

// key returns the key with the given ID. An unknown ID makes it fetch the key
// set again, unless a fetch ran within the last minute.
func (v *Verifier) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	if key, ok := v.keys[kid]; ok {
		v.mu.Unlock()
		return key, nil
	}
	now := v.now()
	if !v.lastFetch.IsZero() && now.Sub(v.lastFetch) < minRefetchInterval {
		v.mu.Unlock()
		return nil, reject("unknown key id")
	}
	// Stamp the attempt before the request, so that concurrent requests and
	// a failing issuer both wait out the interval.
	v.lastFetch = now
	v.mu.Unlock()

	keys, err := v.fetchKeys(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: fetching the keys: %w", ErrUnauthenticated, err)
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.keys = keys
	key, ok := keys[kid]
	if !ok {
		return nil, reject("unknown key id")
	}
	return key, nil
}

// reject returns an error wrapping ErrUnauthenticated.
// The reason is a fixed string, so it never contains a part of the token.
func reject(reason string) error {
	return fmt.Errorf("%w: %s", ErrUnauthenticated, reason)
}

// token is a parsed JWT whose signature is not checked yet.
type token struct {
	kid          string
	signingInput string
	signature    []byte
	claims       map[string]any
}

// parseToken splits and decodes raw, and accepts only the RS256 algorithm and
// a token with a key ID. It does not check the signature or the claims.
func parseToken(raw string) (token, error) {
	if len(raw) > maxTokenBytes {
		return token{}, reject("token is too long")
	}
	header, rest, ok := strings.Cut(raw, ".")
	if !ok {
		return token{}, reject("token is not three parts")
	}
	payload, signature, ok := strings.Cut(rest, ".")
	if !ok || strings.Contains(signature, ".") {
		return token{}, reject("token is not three parts")
	}

	var tok token
	headerJSON, err := base64.RawURLEncoding.DecodeString(header)
	if err != nil {
		return token{}, reject("token header is not base64url")
	}
	payloadJSON, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return token{}, reject("token payload is not base64url")
	}
	if tok.signature, err = base64.RawURLEncoding.DecodeString(signature); err != nil {
		return token{}, reject("token signature is not base64url")
	}

	var head struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(headerJSON, &head); err != nil {
		return token{}, reject("token header is not valid JSON")
	}
	if head.Alg != algRS256 {
		return token{}, reject("token algorithm is not RS256")
	}
	if head.Kid == "" {
		return token{}, reject("token has no key id")
	}
	if err := json.Unmarshal(payloadJSON, &tok.claims); err != nil || tok.claims == nil {
		return token{}, reject("token payload is not a JSON object")
	}
	tok.kid = head.Kid
	tok.signingInput = header + "." + payload
	return tok, nil
}

// Verify checks the signature, issuer, audience and times of raw. A token
// with no expiry, no jti or no subject is refused, since a replay could not
// be told apart.
func (v *Verifier) Verify(ctx context.Context, raw string) (Identity, error) {
	tok, err := parseToken(raw)
	if err != nil {
		return Identity{}, err
	}
	key, err := v.key(ctx, tok.kid)
	if err != nil {
		return Identity{}, err
	}
	digest := sha256.Sum256([]byte(tok.signingInput))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], tok.signature); err != nil {
		return Identity{}, reject("bad signature")
	}
	return v.checkClaims(tok.claims, v.now())
}

func (v *Verifier) checkClaims(claims map[string]any, now time.Time) (Identity, error) {
	if iss, _ := claims["iss"].(string); iss != v.issuer {
		return Identity{}, reject("wrong issuer")
	}
	if !v.hasAudience(claims["aud"]) {
		return Identity{}, reject("wrong audience")
	}

	rawExp, ok := claims["exp"]
	if !ok {
		return Identity{}, reject("token has no exp")
	}
	expiry, ok := numericDate(rawExp)
	if !ok {
		return Identity{}, reject("exp is not a date")
	}
	if !now.Before(expiry) {
		return Identity{}, reject("token is expired")
	}
	for _, name := range []string{"nbf", "iat"} {
		raw, present := claims[name]
		if !present {
			continue
		}
		at, ok := numericDate(raw)
		if !ok {
			return Identity{}, reject(name + " is not a date")
		}
		if at.After(now.Add(timeLeeway)) {
			return Identity{}, reject(name + " is in the future")
		}
	}

	id, _ := claims["jti"].(string)
	if id == "" {
		return Identity{}, reject("token has no jti")
	}
	subject, ok := claims["sub"].(string)
	if !ok {
		return Identity{}, reject("sub is not a string")
	}
	return Identity{Subject: subject, ID: id, Expiry: expiry, Claims: claims}, nil
}

// hasAudience reports whether the aud claim, a string or a list of strings,
// contains the audience of the verifier.
func (v *Verifier) hasAudience(aud any) bool {
	switch aud := aud.(type) {
	case string:
		return aud == v.audience
	case []any:
		return slices.ContainsFunc(aud, func(item any) bool {
			s, ok := item.(string)
			return ok && s == v.audience
		})
	default:
		return false
	}
}

// numericDate converts a JSON number of seconds since the epoch to a time.
func numericDate(raw any) (time.Time, bool) {
	seconds, ok := raw.(float64)
	if !ok || math.IsNaN(seconds) || math.Abs(seconds) > maxNumericDate {
		return time.Time{}, false
	}
	whole, fraction := math.Modf(seconds)
	return time.Unix(int64(whole), int64(fraction*float64(time.Second))), true
}

// Policy lists which verified tokens may trigger a fetch.
type Policy struct {
	// Subjects are the accepted sub values, matched exactly.
	Subjects []string
	// Claims are string claims that must equal their value.
	Claims map[string]string
}

// Allow returns an error wrapping ErrForbidden unless id has an accepted
// subject and every required claim.
func (p Policy) Allow(id Identity) error {
	if !slices.Contains(p.Subjects, id.Subject) {
		return fmt.Errorf("%w: subject is not allowed", ErrForbidden)
	}
	for key, want := range p.Claims {
		if got, ok := id.Claims[key].(string); !ok || got != want {
			return fmt.Errorf("%w: claim %q does not match", ErrForbidden, key)
		}
	}
	return nil
}
