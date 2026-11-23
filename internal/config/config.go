// Package config reads the settings of each subcommand from flags, with a
// COMIN_HOOK_* environment variable as the fallback for each flag.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path"
	"strings"
)

// Defaults of the flags that have one.
const (
	DefaultListen           = "127.0.0.1:8371"
	DefaultPath             = "/deploy"
	DefaultIssuer           = "https://token.actions.githubusercontent.com"
	DefaultCominSocket      = "/run/comin-grpc.sock"
	DefaultTailscaledSocket = "/var/run/tailscale/tailscaled.sock"
	DefaultFunnelPort       = 443
)

// funnelPorts are the ports Tailscale Funnel accepts.
var funnelPorts = []uint16{443, 8443, 10000}

// Serve is the configuration of the serve subcommand.
type Serve struct {
	Listen      string
	Path        string
	Issuer      string
	Audience    string
	Subjects    []string
	Claims      map[string]string
	CominSocket string
}

// Funnel is the configuration of the publish and unpublish subcommands.
type Funnel struct {
	Listen           string
	Path             string
	FunnelPort       uint16
	TailscaledSocket string
}

// ParseServe reads the serve flags from args and the environment from getenv.
func ParseServe(args []string, getenv func(string) string) (Serve, error) {
	var (
		s        Serve
		subjects list
		claims   list
	)
	fs := newFlagSet("serve")
	fs.StringVar(&s.Listen, "listen", DefaultListen, "address the receiver listens on")
	fs.StringVar(&s.Path, "path", DefaultPath, "request path the receiver accepts")
	fs.StringVar(&s.Issuer, "issuer", DefaultIssuer, "OIDC issuer URL of the CI tokens")
	fs.StringVar(&s.Audience, "audience", "", "audience the CI tokens must carry (required)")
	fs.Var(&subjects, "subject", "accepted sub claim, repeatable (required)")
	fs.Var(&claims, "claim", "extra string claim as key=value that must match, repeatable")
	fs.StringVar(&s.CominSocket, "comin-socket", DefaultCominSocket, "path of comin's gRPC unix socket")
	if err := parse(fs, args, getenv); err != nil {
		return Serve{}, err
	}

	s.Subjects = subjects.values
	var err error
	if s.Claims, err = parseClaims(claims.values); err != nil {
		return Serve{}, err
	}
	if err := s.validate(); err != nil {
		return Serve{}, err
	}
	return s, nil
}

func (s Serve) validate() error {
	if err := validateListen(s.Listen); err != nil {
		return err
	}
	if err := validatePath(s.Path); err != nil {
		return err
	}
	issuer, err := url.Parse(s.Issuer)
	if err != nil || issuer.Scheme != "https" || issuer.Host == "" {
		return fmt.Errorf("--issuer must be an https URL, got %q", s.Issuer)
	}
	if s.Audience == "" {
		return errors.New("--audience is required (or COMIN_HOOK_AUDIENCE)")
	}
	if len(s.Subjects) == 0 {
		return errors.New("--subject is required at least once (or COMIN_HOOK_SUBJECT)")
	}
	if !path.IsAbs(s.CominSocket) {
		return fmt.Errorf("--comin-socket must be an absolute path, got %q", s.CominSocket)
	}
	return nil
}

// ParseFunnel reads the publish and unpublish flags from args and the
// environment from getenv.
func ParseFunnel(name string, args []string, getenv func(string) string) (Funnel, error) {
	var (
		f    Funnel
		port int
	)
	fs := newFlagSet(name)
	fs.StringVar(&f.Listen, "listen", DefaultListen, "address the receiver listens on, which Funnel proxies to")
	fs.StringVar(&f.Path, "path", DefaultPath, "request path the receiver accepts, which Funnel serves")
	fs.IntVar(&port, "funnel-port", DefaultFunnelPort, "HTTPS port Funnel listens on: 443, 8443 or 10000")
	fs.StringVar(&f.TailscaledSocket, "tailscaled-socket", DefaultTailscaledSocket, "path of tailscaled's unix socket")
	if err := parse(fs, args, getenv); err != nil {
		return Funnel{}, err
	}

	if err := validateListen(f.Listen); err != nil {
		return Funnel{}, err
	}
	if host, _, _ := net.SplitHostPort(f.Listen); host == "" {
		return Funnel{}, fmt.Errorf("--listen needs a host for Funnel to proxy to, got %q", f.Listen)
	}
	if err := validatePath(f.Path); err != nil {
		return Funnel{}, err
	}
	for _, allowed := range funnelPorts {
		if int(allowed) == port {
			f.FunnelPort = allowed
		}
	}
	if f.FunnelPort == 0 {
		return Funnel{}, fmt.Errorf("--funnel-port must be 443, 8443 or 10000, got %d", port)
	}
	return f, nil
}

func validateListen(listen string) error {
	if _, _, err := net.SplitHostPort(listen); err != nil {
		return fmt.Errorf("--listen must be host:port, got %q: %w", listen, err)
	}
	return nil
}

func validatePath(p string) error {
	if !strings.HasPrefix(p, "/") || path.Clean(p) != p || strings.ContainsAny(p, "?#") {
		return fmt.Errorf("--path must be a clean absolute path without a trailing slash, got %q", p)
	}
	return nil
}

func parseClaims(pairs []string) (map[string]string, error) {
	claims := make(map[string]string, len(pairs))
	for _, pair := range pairs {
		key, value, ok := strings.Cut(pair, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("--claim must be key=value, got %q", pair)
		}
		claims[key] = value
	}
	return claims, nil
}

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// parse reads args into fs, then fills each flag that args left unset from
// its COMIN_HOOK_* variable. A list flag takes comma-separated values there.
func parse(fs *flag.FlagSet, args []string, getenv func(string) string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fs.SetOutput(os.Stderr)
			fs.PrintDefaults()
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	given := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })

	var errs []error
	fs.VisitAll(func(f *flag.Flag) {
		if given[f.Name] {
			return
		}
		name := EnvName(f.Name)
		value := getenv(name)
		if value == "" {
			return
		}
		if l, ok := f.Value.(*list); ok {
			for part := range strings.SplitSeq(value, ",") {
				if part = strings.TrimSpace(part); part != "" {
					errs = append(errs, l.Set(part))
				}
			}
			return
		}
		if err := fs.Set(f.Name, value); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	})
	return errors.Join(errs...)
}

// EnvName is the environment variable that backs the flag with the given name.
func EnvName(flagName string) string {
	return "COMIN_HOOK_" + strings.ToUpper(strings.ReplaceAll(flagName, "-", "_"))
}

// list is a repeatable string flag.
type list struct{ values []string }

func (l *list) String() string { return strings.Join(l.values, ",") }

func (l *list) Set(v string) error {
	l.values = append(l.values, v)
	return nil
}
