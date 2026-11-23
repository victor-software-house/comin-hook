// Package verify checks a CI job's OIDC token and decides whether its holder
// may trigger a fetch.
package verify

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// ErrUnauthenticated marks a token that is not a valid token of the issuer
// for this audience.
var ErrUnauthenticated = errors.New("unauthenticated")

// ErrForbidden marks a valid token whose subject or claims are not allowed.
var ErrForbidden = errors.New("forbidden")

// httpTimeout bounds each request to the issuer's discovery and key endpoints.
const httpTimeout = 10 * time.Second

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
	idTokens *oidc.IDTokenVerifier
}

// New discovers the issuer's configuration and returns a verifier that
// accepts RS256 tokens for audience. The issuer's signing keys are fetched
// when needed and cached by their remote key set, until ctx ends.
func New(ctx context.Context, issuer, audience string) (*Verifier, error) {
	ctx = oidc.ClientContext(ctx, &http.Client{Timeout: httpTimeout})
	provider, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, fmt.Errorf("discovering OIDC issuer %s: %w", issuer, err)
	}
	return &Verifier{idTokens: provider.Verifier(&oidc.Config{
		ClientID:             audience,
		SupportedSigningAlgs: []string{oidc.RS256},
	})}, nil
}

// Verify checks the signature, issuer, audience and expiry of raw. A token
// with no expiry or no jti is refused, since a replay could not be told apart.
func (v *Verifier) Verify(ctx context.Context, raw string) (Identity, error) {
	token, err := v.idTokens.Verify(ctx, raw)
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %w", ErrUnauthenticated, err)
	}
	var claims map[string]any
	if err := token.Claims(&claims); err != nil {
		return Identity{}, fmt.Errorf("%w: reading claims: %w", ErrUnauthenticated, err)
	}
	id, _ := claims["jti"].(string)
	if id == "" {
		return Identity{}, fmt.Errorf("%w: token has no jti", ErrUnauthenticated)
	}
	if token.Expiry.IsZero() {
		return Identity{}, fmt.Errorf("%w: token has no exp", ErrUnauthenticated)
	}
	return Identity{Subject: token.Subject, ID: id, Expiry: token.Expiry, Claims: claims}, nil
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
