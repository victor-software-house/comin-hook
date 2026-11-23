// Package funnel publishes the receiver on the internet through the host's
// own tailscaled, with Tailscale Funnel, and takes it down again.
package funnel

import (
	"fmt"
	"net"
	"strconv"

	"tailscale.com/ipn"
)

// Spec is the serve configuration that publish adds and unpublish removes.
type Spec struct {
	// Host is the node's DNS name, without the trailing dot.
	Host string
	// Port is the HTTPS port Funnel listens on.
	Port uint16
	// Path is the mount point of the handler and the path the receiver serves.
	Path string
	// Upstream is the URL the handler proxies to, from Target.
	Upstream string
}

func (s Spec) hostPort() ipn.HostPort {
	return ipn.HostPort(net.JoinHostPort(s.Host, strconv.Itoa(int(s.Port))))
}

// Target is the URL a handler mounted at path proxies to, so that a request
// for path reaches the receiver at path.
//
// Tailscaled cuts the mount point off the request path before it proxies.
// It joins what remains to the path of the target.
// A target without the path would deliver the request at the root.
// The tailscale source for this is the serve file of the ipnlocal package.
func Target(listen, path string) string {
	return "http://" + listen + path
}

// ConflictError reports a serve configuration that publish refuses to change.
type ConflictError struct {
	HostPort ipn.HostPort
	Reason   string
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("serve config conflict at %s: %s", e.HostPort, e.Reason)
}

// Add returns a copy of cfg that serves the spec, and whether that differs.
// It leaves every other entry of cfg as it is.
// It refuses with a conflict error when the host and port of the spec
// already have a handler at another path.
// Funnel applies to a whole host and port, so it would expose that handler.
func Add(cfg *ipn.ServeConfig, s Spec) (*ipn.ServeConfig, bool, error) {
	next := clone(cfg)
	hp := s.hostPort()

	if web := next.Web[hp]; web != nil {
		for mount := range web.Handlers {
			if mount != s.Path {
				return nil, false, &ConflictError{HostPort: hp, Reason: fmt.Sprintf(
					"it has a handler at %q, and Funnel would expose it", mount)}
			}
		}
	}
	if tcp := next.TCP[s.Port]; tcp != nil && !isHTTPSOnly(tcp) {
		return nil, false, &ConflictError{HostPort: hp, Reason: fmt.Sprintf(
			"port %d serves something other than HTTPS handlers", s.Port)}
	}
	if existing := handlerAt(next.Web[hp], s.Path); existing != nil && !isProxy(existing) {
		return nil, false, &ConflictError{HostPort: hp, Reason: fmt.Sprintf(
			"the handler at %q is not a proxy", s.Path)}
	}

	changed := false
	if next.TCP[s.Port] == nil {
		if next.TCP == nil {
			next.TCP = make(map[uint16]*ipn.TCPPortHandler)
		}
		next.TCP[s.Port] = &ipn.TCPPortHandler{HTTPS: true}
		changed = true
	}
	if next.Web[hp] == nil {
		if next.Web == nil {
			next.Web = make(map[ipn.HostPort]*ipn.WebServerConfig)
		}
		next.Web[hp] = &ipn.WebServerConfig{}
	}
	web := next.Web[hp]
	if existing := handlerAt(web, s.Path); existing == nil || existing.Proxy != s.Upstream {
		if web.Handlers == nil {
			web.Handlers = make(map[string]*ipn.HTTPHandler)
		}
		web.Handlers[s.Path] = &ipn.HTTPHandler{Proxy: s.Upstream}
		changed = true
	}
	if !next.AllowFunnel[hp] {
		if next.AllowFunnel == nil {
			next.AllowFunnel = make(map[ipn.HostPort]bool)
		}
		next.AllowFunnel[hp] = true
		changed = true
	}
	return next, changed, nil
}

// Remove returns a copy of cfg without what Add put there, and whether that
// differs.
// It removes only the handler at the path of the spec.
// It does so only when that is the proxy to the upstream of the spec.
// When that leaves the host and port with no handler, it removes more.
// It also removes their web entry, their Funnel entry and the HTTPS listener.
// Otherwise those stay, since they serve a handler that Add did not add.
func Remove(cfg *ipn.ServeConfig, s Spec) (*ipn.ServeConfig, bool) {
	next := clone(cfg)
	hp := s.hostPort()

	web := next.Web[hp]
	existing := handlerAt(web, s.Path)
	if existing == nil || !isProxy(existing) || existing.Proxy != s.Upstream {
		return next, false
	}
	delete(web.Handlers, s.Path)
	if len(web.Handlers) > 0 {
		return next, true
	}

	delete(next.Web, hp)
	if len(next.Web) == 0 {
		next.Web = nil
	}
	delete(next.AllowFunnel, hp)
	if len(next.AllowFunnel) == 0 {
		next.AllowFunnel = nil
	}
	if tcp := next.TCP[s.Port]; tcp != nil && isHTTPSOnly(tcp) && !portHasWeb(next, s.Port) {
		delete(next.TCP, s.Port)
		if len(next.TCP) == 0 {
			next.TCP = nil
		}
	}
	return next, true
}

// clone copies cfg, ETag included, and turns nil into an empty config.
func clone(cfg *ipn.ServeConfig) *ipn.ServeConfig {
	if cfg == nil {
		return new(ipn.ServeConfig)
	}
	return cfg.Clone()
}

func handlerAt(web *ipn.WebServerConfig, mount string) *ipn.HTTPHandler {
	if web == nil {
		return nil
	}
	return web.Handlers[mount]
}

func isHTTPSOnly(tcp *ipn.TCPPortHandler) bool {
	return tcp.HTTPS && !tcp.HTTP && tcp.TCPForward == ""
}

func isProxy(h *ipn.HTTPHandler) bool {
	return h.Proxy != "" && h.Path == "" && h.Text == "" && h.Redirect == "" && len(h.AcceptAppCaps) == 0
}

func portHasWeb(cfg *ipn.ServeConfig, port uint16) bool {
	for hp := range cfg.Web {
		if p, err := hp.Port(); err == nil && p == port {
			return true
		}
	}
	return false
}
