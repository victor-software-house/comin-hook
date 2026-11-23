// Command comin-hook lets a CI job trigger a fetch in comin on this host.
//
// The serve subcommand receives the CI job's request. The publish and
// unpublish subcommands add the receiver to, and remove it from, the host's
// Tailscale Funnel configuration.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/victor-software-house/comin-hook/internal/comin"
	"github.com/victor-software-house/comin-hook/internal/config"
	"github.com/victor-software-house/comin-hook/internal/funnel"
	"github.com/victor-software-house/comin-hook/internal/server"
	"github.com/victor-software-house/comin-hook/internal/verify"
)

// fetchTimeout bounds each call to comin. It stays below the server's
// write timeout, so a slow comin gives a 502 instead of a dropped connection.
const fetchTimeout = 5 * time.Second

const usage = "usage: comin-hook <serve|publish|unpublish> [flags]"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	err := run(ctx)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "comin-hook:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	if len(os.Args) < 2 {
		return errors.New(usage)
	}
	command, args := os.Args[1], os.Args[2:]

	var err error
	switch command {
	case "serve":
		err = serveCommand(ctx, args)
	case "publish":
		err = funnelCommand(ctx, command, args, funnel.Publish)
	case "unpublish":
		err = funnelCommand(ctx, command, args, funnel.Unpublish)
	default:
		return fmt.Errorf("unknown command %q\n%s", command, usage)
	}
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	return err
}

func serveCommand(ctx context.Context, args []string) (err error) {
	cfg, err := config.ParseServe(args, os.Getenv)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	verifier, err := verify.New(ctx, cfg.Issuer, cfg.Audience)
	if err != nil {
		return err
	}
	fetcher, err := comin.New(cfg.CominSocket, fetchTimeout)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, fetcher.Close()) }()

	var listenConfig net.ListenConfig
	ln, err := listenConfig.Listen(ctx, "tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", cfg.Listen, err)
	}
	log.Info("listening", "addr", ln.Addr().String(), "path", cfg.Path)

	handler := server.NewHandler(server.Options{
		Path:     cfg.Path,
		Verifier: verifier,
		Policy:   verify.Policy{Subjects: cfg.Subjects, Claims: cfg.Claims},
		Fetcher:  fetcher,
		Logger:   log,
		Now:      time.Now,
	})
	return server.Run(ctx, ln, handler, log)
}

func funnelCommand(
	ctx context.Context,
	name string,
	args []string,
	act func(context.Context, funnel.Options) error,
) error {
	cfg, err := config.ParseFunnel(name, args, os.Getenv)
	if err != nil {
		return err
	}
	return act(ctx, funnel.Options{
		Listen: cfg.Listen,
		Path:   cfg.Path,
		Port:   cfg.FunnelPort,
		Socket: cfg.TailscaledSocket,
	})
}
