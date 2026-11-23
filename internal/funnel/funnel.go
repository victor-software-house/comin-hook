package funnel

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"tailscale.com/client/local"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
)

// maxAttempts bounds how often an update starts over.
const maxAttempts = 5

// Options name what publish and unpublish act on.
type Options struct {
	// Listen is the receiver's address, which Funnel proxies to.
	Listen string
	// Path is the receiver's request path.
	Path string
	// Port is the HTTPS port Funnel listens on.
	Port uint16
	// Socket is the path of tailscaled's unix socket.
	Socket string
}

// LocalAPI is the part of the tailscaled API that the updates use.
// The local client of tailscale implements it.
type LocalAPI interface {
	StatusWithoutPeers(ctx context.Context) (*ipnstate.Status, error)
	GetServeConfig(ctx context.Context) (*ipn.ServeConfig, error)
	SetServeConfig(ctx context.Context, cfg *ipn.ServeConfig) error
}

// Publish adds the receiver to the serve config of the host's tailscaled and
// allows Funnel for it. It changes nothing when that is already so.
func Publish(ctx context.Context, opts Options) error {
	return publish(ctx, &local.Client{Socket: opts.Socket}, opts)
}

// Unpublish removes what Publish added.
// It changes nothing when that is already gone.
func Unpublish(ctx context.Context, opts Options) error {
	return unpublish(ctx, &local.Client{Socket: opts.Socket}, opts)
}

func publish(ctx context.Context, api LocalAPI, opts Options) error {
	return update(ctx, api, opts, Add)
}

func unpublish(ctx context.Context, api LocalAPI, opts Options) error {
	return update(ctx, api, opts, func(cfg *ipn.ServeConfig, s Spec) (*ipn.ServeConfig, bool, error) {
		next, changed := Remove(cfg, s)
		return next, changed, nil
	})
}

// update reads the serve config with its ETag and applies change.
// It writes the result with that ETag.
// It starts over when another writer changed the config in between.
func update(
	ctx context.Context,
	api LocalAPI,
	opts Options,
	change func(*ipn.ServeConfig, Spec) (*ipn.ServeConfig, bool, error),
) error {
	host, err := nodeName(ctx, api)
	if err != nil {
		return err
	}
	spec := Spec{Host: host, Port: opts.Port, Path: opts.Path, Upstream: Target(opts.Listen, opts.Path)}

	for range maxAttempts {
		cfg, err := api.GetServeConfig(ctx)
		if err != nil {
			return fmt.Errorf("reading the serve config: %w", err)
		}
		next, changed, err := change(cfg, spec)
		if err != nil {
			return err
		}
		if !changed {
			return nil
		}
		err = api.SetServeConfig(ctx, next)
		if err == nil {
			return nil
		}
		if !local.IsPreconditionsFailedError(err) {
			return fmt.Errorf("writing the serve config: %w", err)
		}
	}
	return fmt.Errorf("the serve config changed %d times while it was being updated", maxAttempts)
}

// nodeName is the DNS name of the node, without the trailing dot.
func nodeName(ctx context.Context, api LocalAPI) (string, error) {
	status, err := api.StatusWithoutPeers(ctx)
	if err != nil {
		return "", fmt.Errorf("reading the tailscaled status: %w", err)
	}
	if status.Self == nil || status.Self.DNSName == "" {
		return "", errors.New("tailscaled reports no DNS name for this node: is it logged in, with HTTPS enabled?")
	}
	return strings.TrimSuffix(status.Self.DNSName, "."), nil
}
