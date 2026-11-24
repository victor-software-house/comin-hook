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
// reason is a short name for the outcome, and cause the error behind it, if
// any. Neither contains a part of the token.
type result struct {
	status  int
	reason  string
	cause   error
	subject string
	jti     string
}

// ServeHTTP checks the request in order: path, method, token, subject and
// claims, replay. Only then does it call comin.
// It reserves the token's ID before the call and releases it when the call
// fails, so the job may retry with the same token.
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
	h.logResult(res)
}

// logResult writes one line per request. A rejection logs its reason, its
// status, and the subject when the token got far enough to show one.
func (h *Handler) logResult(res result) {
	attrs := []any{"status", res.status, "reason", res.reason}
	if res.cause != nil {
		attrs = append(attrs, "error", res.cause)
	}
	if res.subject != "" {
		attrs = append(attrs, "sub", res.subject)
	}
	if res.jti != "" {
		attrs = append(attrs, "jti", res.jti)
	}
	if res.status == http.StatusAccepted {
		h.log.Info("request accepted", attrs...)
		return
	}
	h.log.Warn("request rejected", attrs...)
}

func (h *Handler) handle(r *http.Request) result {
	if r.URL.Path != h.path {
		return result{status: http.StatusNotFound, reason: "not_found"}
	}
	if r.Method != http.MethodPost {
		return result{status: http.StatusMethodNotAllowed, reason: "method_not_allowed"}
	}

	raw, ok := bearerToken(r)
	if !ok {
		return result{status: http.StatusUnauthorized, reason: "missing_token"}
	}
	id, err := h.verifier.Verify(r.Context(), raw)
	if err != nil {
		return result{status: http.StatusUnauthorized, reason: "invalid_token", cause: err}
	}
	res := result{subject: id.Subject, jti: id.ID}
	if err := h.policy.Allow(id); err != nil {
		res.status, res.reason, res.cause = http.StatusForbidden, "forbidden", err
		return res
	}
	if h.replays.Seen(id.ID, id.Expiry) {
		res.status, res.reason = http.StatusConflict, "replayed"
		return res
	}
	if err := h.fetcher.Fetch(r.Context()); err != nil {
		h.replays.Release(id.ID)
		res.status, res.reason, res.cause = http.StatusBadGateway, "fetch_failed", err
		return res
	}
	res.status, res.reason = http.StatusAccepted, "accepted"
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
