package funnel

import (
	"encoding/json/v2"
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"tailscale.com/ipn"
	"tailscale.com/tailcfg"
)

const (
	host     = "node.example.com"
	hostPort = ipn.HostPort("node.example.com:443")
	upstream = "http://127.0.0.1:8371/deploy"
)

var spec = Spec{Host: host, Port: 443, Path: "/deploy", Upstream: upstream}

// ours is the serve config that Add builds from nothing.
func ours() *ipn.ServeConfig {
	return &ipn.ServeConfig{
		TCP:         map[uint16]*ipn.TCPPortHandler{443: {HTTPS: true}},
		Web:         map[ipn.HostPort]*ipn.WebServerConfig{hostPort: {Handlers: map[string]*ipn.HTTPHandler{"/deploy": {Proxy: upstream}}}},
		AllowFunnel: map[ipn.HostPort]bool{hostPort: true},
	}
}

// service is a Tailscale Services entry, which neither Add nor Remove touches.
func service() map[tailcfg.ServiceName]*ipn.ServiceConfig {
	return map[tailcfg.ServiceName]*ipn.ServiceConfig{
		"svc:example": {
			TCP: map[uint16]*ipn.TCPPortHandler{443: {HTTPS: true}},
			Web: map[ipn.HostPort]*ipn.WebServerConfig{
				"example.tailnet.example.com:443": {Handlers: map[string]*ipn.HTTPHandler{"/": {Proxy: "http://127.0.0.1:3000"}}},
			},
		},
	}
}

// otherPort is a listener on another port, with its own web handler.
func otherPort(cfg *ipn.ServeConfig) *ipn.ServeConfig {
	cfg.TCP[8080] = &ipn.TCPPortHandler{TCPForward: "127.0.0.1:5432"}
	cfg.TCP[8443] = &ipn.TCPPortHandler{HTTPS: true}
	cfg.Web["node.example.com:8443"] = &ipn.WebServerConfig{Handlers: map[string]*ipn.HTTPHandler{"/": {Text: "hello"}}}
	return cfg
}

func withTCP(cfg *ipn.ServeConfig) *ipn.ServeConfig {
	if cfg.TCP == nil {
		cfg.TCP = make(map[uint16]*ipn.TCPPortHandler)
	}
	return cfg
}

func TestAdd(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		in           *ipn.ServeConfig
		want         *ipn.ServeConfig
		wantChanged  bool
		wantConflict bool
	}{
		{name: "empty config", in: &ipn.ServeConfig{}, want: ours(), wantChanged: true},
		{name: "nil config", in: nil, want: ours(), wantChanged: true},
		{
			name:        "beside a Tailscale Services entry",
			in:          &ipn.ServeConfig{Services: service()},
			want:        func() *ipn.ServeConfig { c := ours(); c.Services = service(); return c }(),
			wantChanged: true,
		},
		{
			name:        "beside other ports",
			in:          otherPort(withTCP(&ipn.ServeConfig{Web: map[ipn.HostPort]*ipn.WebServerConfig{}})),
			want:        otherPort(ours()),
			wantChanged: true,
		},
		{name: "already added", in: ours(), want: ours()},
		{
			name: "Funnel allowed for another host and port",
			in:   &ipn.ServeConfig{AllowFunnel: map[ipn.HostPort]bool{"node.example.com:8443": true}},
			want: func() *ipn.ServeConfig {
				c := ours()
				c.AllowFunnel["node.example.com:8443"] = true
				return c
			}(),
			wantChanged: true,
		},
		{
			name: "proxy at the path with another upstream",
			in: func() *ipn.ServeConfig {
				c := ours()
				c.Web[hostPort].Handlers["/deploy"].Proxy = "http://127.0.0.1:9999/deploy"
				return c
			}(),
			want:        ours(),
			wantChanged: true,
		},
		{
			name: "another handler on the host and port",
			in: func() *ipn.ServeConfig {
				c := ours()
				c.Web[hostPort].Handlers["/"] = &ipn.HTTPHandler{Text: "hello"}
				return c
			}(),
			wantConflict: true,
		},
		{
			name: "handler at the path that is not a proxy",
			in: func() *ipn.ServeConfig {
				c := ours()
				c.Web[hostPort].Handlers["/deploy"] = &ipn.HTTPHandler{Text: "hello"}
				return c
			}(),
			wantConflict: true,
		},
		{
			name:         "port serves plain TCP",
			in:           &ipn.ServeConfig{TCP: map[uint16]*ipn.TCPPortHandler{443: {TCPForward: "127.0.0.1:22"}}},
			wantConflict: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var before *ipn.ServeConfig
			if tt.in != nil {
				before = tt.in.Clone()
			}

			got, changed, err := Add(tt.in, spec)

			if _, isConflict := errors.AsType[*ConflictError](err); isConflict != tt.wantConflict {
				t.Fatalf("Add error = %v, want conflict: %t", err, tt.wantConflict)
			}
			if diff := cmp.Diff(before, tt.in); diff != "" {
				t.Errorf("Add changed its input (-before +after):\n%s", diff)
			}
			if tt.wantConflict {
				return
			}
			if changed != tt.wantChanged {
				t.Errorf("changed = %t, want %t", changed, tt.wantChanged)
			}
			if diff := cmp.Diff(tt.want, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("config mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestAddIsIdempotent(t *testing.T) {
	t.Parallel()

	first, changed, err := Add(&ipn.ServeConfig{Services: service()}, spec)
	if err != nil || !changed {
		t.Fatalf("first Add = (changed %t, error %v), want a change", changed, err)
	}
	second, changed, err := Add(first, spec)
	if err != nil || changed {
		t.Fatalf("second Add = (changed %t, error %v), want no change", changed, err)
	}
	if diff := cmp.Diff(first, second); diff != "" {
		t.Errorf("second Add changed the config (-first +second):\n%s", diff)
	}
}

func TestAddKeepsServicesBytes(t *testing.T) {
	t.Parallel()

	in := &ipn.ServeConfig{Services: service()}
	want, err := json.Marshal(in.Services, json.Deterministic(true))
	if err != nil {
		t.Fatalf("encoding the services: %v", err)
	}
	got, _, err := Add(in, spec)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	gotJSON, err := json.Marshal(got.Services, json.Deterministic(true))
	if err != nil {
		t.Fatalf("encoding the services after Add: %v", err)
	}
	if string(gotJSON) != string(want) {
		t.Errorf("services changed:\nbefore %s\nafter  %s", want, gotJSON)
	}
}

func TestRemove(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		in          *ipn.ServeConfig
		want        *ipn.ServeConfig
		wantChanged bool
	}{
		{name: "only our entries", in: ours(), want: &ipn.ServeConfig{}, wantChanged: true},
		{
			name: "beside a Tailscale Services entry",
			in:   func() *ipn.ServeConfig { c := ours(); c.Services = service(); return c }(),
			want: &ipn.ServeConfig{Services: service()}, wantChanged: true,
		},
		{
			name:        "beside other ports",
			in:          otherPort(ours()),
			want:        otherPort(&ipn.ServeConfig{TCP: map[uint16]*ipn.TCPPortHandler{}, Web: map[ipn.HostPort]*ipn.WebServerConfig{}}),
			wantChanged: true,
		},
		{
			name: "another handler remains on the host and port",
			in: func() *ipn.ServeConfig {
				c := ours()
				c.Web[hostPort].Handlers["/other"] = &ipn.HTTPHandler{Text: "hello"}
				return c
			}(),
			want: func() *ipn.ServeConfig {
				c := ours()
				delete(c.Web[hostPort].Handlers, "/deploy")
				c.Web[hostPort].Handlers["/other"] = &ipn.HTTPHandler{Text: "hello"}
				return c
			}(),
			wantChanged: true,
		},
		{name: "nothing to remove", in: &ipn.ServeConfig{Services: service()}, want: &ipn.ServeConfig{Services: service()}},
		{name: "nil config", in: nil, want: &ipn.ServeConfig{}},
		{
			name: "handler at the path with another upstream",
			in: func() *ipn.ServeConfig {
				c := ours()
				c.Web[hostPort].Handlers["/deploy"].Proxy = "http://127.0.0.1:9999/deploy"
				return c
			}(),
			want: func() *ipn.ServeConfig {
				c := ours()
				c.Web[hostPort].Handlers["/deploy"].Proxy = "http://127.0.0.1:9999/deploy"
				return c
			}(),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var before *ipn.ServeConfig
			if tt.in != nil {
				before = tt.in.Clone()
			}

			got, changed := Remove(tt.in, spec)

			if diff := cmp.Diff(before, tt.in); diff != "" {
				t.Errorf("Remove changed its input (-before +after):\n%s", diff)
			}
			if changed != tt.wantChanged {
				t.Errorf("changed = %t, want %t", changed, tt.wantChanged)
			}
			if diff := cmp.Diff(tt.want, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("config mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestAddThenRemoveRestoresTheConfig(t *testing.T) {
	t.Parallel()

	start := otherPort(&ipn.ServeConfig{
		TCP:      map[uint16]*ipn.TCPPortHandler{},
		Web:      map[ipn.HostPort]*ipn.WebServerConfig{},
		Services: service(),
	})
	added, _, err := Add(start, spec)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	removed, changed := Remove(added, spec)
	if !changed {
		t.Error("Remove reported no change")
	}
	if diff := cmp.Diff(start, removed, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("config after Add and Remove differs (-start +end):\n%s", diff)
	}
}
