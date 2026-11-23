// Package server receives the CI job's request and makes comin fetch.
package server

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/victor-software-house/comin-hook/internal/replay"
	"github.com/victor-software-house/comin-hook/internal/verify"
)

// maxBodyBytes bounds a request body.
// The handler never reads it, since the request carries no data.
const maxBodyBytes = 4 << 10

// Verifier turns a bearer token into the identity it proves.
type Verifier interface {
	Verify(ctx context.Context, raw string) (verify.Identity, error)
}

// Fetcher makes comin fetch.
type Fetcher interface {
	Fetch(ctx context.Context) error
}

// Options are what NewHandler needs.
type Options struct {
	// Path is the one request path the handler serves.
	Path     string
	Verifier Verifier
	Policy   verify.Policy
	Fetcher  Fetcher
	Logger   *slog.Logger
	// Now is the clock of the replay set.
	Now func() time.Time
}

// Handler serves POST requests on one path.
type Handler struct {
	path     string
	verifier Verifier
	policy   verify.Policy
	fetcher  Fetcher
	log      *slog.Logger
	replays  *replay.Set
}

// NewHandler returns a handler for opts.
func NewHandler(opts Options) *Handler {
	return &Handler{
		path:     opts.Path,
		verifier: opts.Verifier,
		policy:   opts.Policy,
		fetcher:  opts.Fetcher,
		log:      opts.Logger,
		replays:  replay.New(opts.Now),
	}
}

// result is how a request ended.
type result struct {
	status  int
	outcome string
	subject string
	jti     string
}

// ServeHTTP checks the request in order: path, method, token, subject and
// claims, replay. Only then does it call comin.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	res := h.handle(r)
	if res.status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", "Bearer")
	}
	if res.status == http.StatusMethodNotAllowed {
		w.Header().Set("Allow", http.MethodPost)
	}
	if res.status == http.StatusAccepted {
		w.WriteHeader(res.status)
	} else {
		http.Error(w, http.StatusText(res.status), res.status)
	}
	h.log.Info("request", "outcome", res.outcome, "status", res.status, "sub", res.subject, "jti", res.jti)
}

func (h *Handler) handle(r *http.Request) result {
	if r.URL.Path != h.path {
		return result{status: http.StatusNotFound, outcome: "not_found"}
	}
	if r.Method != http.MethodPost {
		return result{status: http.StatusMethodNotAllowed, outcome: "method_not_allowed"}
	}

	raw, ok := bearerToken(r)
	if !ok {
		return result{status: http.StatusUnauthorized, outcome: "missing_token"}
	}
	id, err := h.verifier.Verify(r.Context(), raw)
	if err != nil {
		return result{status: http.StatusUnauthorized, outcome: "invalid_token"}
	}
	res := result{subject: id.Subject, jti: id.ID}
	if err := h.policy.Allow(id); err != nil {
		res.status, res.outcome = http.StatusForbidden, "forbidden"
		return res
	}
	if h.replays.Seen(id.ID, id.Expiry) {
		res.status, res.outcome = http.StatusConflict, "replayed"
		return res
	}
	if err := h.fetcher.Fetch(r.Context()); err != nil {
		h.log.Error("comin fetch failed", "error", err, "sub", id.Subject, "jti", id.ID)
		res.status, res.outcome = http.StatusBadGateway, "fetch_failed"
		return res
	}
	res.status, res.outcome = http.StatusAccepted, "accepted"
	return res
}

// bearerToken returns the token of an "Authorization: Bearer <token>" header.
func bearerToken(r *http.Request) (string, bool) {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	token = strings.TrimSpace(token)
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return "", false
	}
	return token, true
}
