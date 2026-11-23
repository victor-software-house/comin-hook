package funnel

import (
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"tailscale.com/client/local"
	"tailscale.com/ipn"
)

// fakeTailscaled serves the API calls that publish and unpublish make.
// Its serve config has a version, which is also its ETag.
// It refuses a write whose If-Match is not the current version.
type fakeTailscaled struct {
	t *testing.T

	mu      sync.Mutex
	cfg     ipn.ServeConfig
	version int
	gets    int
	posts   int
	// onFirstGet runs after the first read, as another writer would between
	// this client's read and write.
	onFirstGet func(cfg *ipn.ServeConfig)
}

func (f *fakeTailscaled) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	f.serve(rec, req)
	return rec.Result(), nil
}

func (f *fakeTailscaled) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	switch {
	case r.URL.Path == "/localapi/v0/status":
		_, _ = io.WriteString(w, `{"Self":{"DNSName":"`+host+`."}}`)
	case r.URL.Path == "/localapi/v0/serve-config" && r.Method == http.MethodGet:
		f.gets++
		body, err := json.Marshal(f.cfg)
		if err != nil {
			f.t.Errorf("encoding the serve config: %v", err)
		}
		w.Header().Set("ETag", strconv.Itoa(f.version))
		_, _ = w.Write(body)
		if f.gets == 1 && f.onFirstGet != nil {
			f.onFirstGet(&f.cfg)
			f.version++
		}
	case r.URL.Path == "/localapi/v0/serve-config" && r.Method == http.MethodPost:
		f.posts++
		if r.Header.Get("If-Match") != strconv.Itoa(f.version) {
			w.WriteHeader(http.StatusPreconditionFailed)
			_, _ = io.WriteString(w, `{"error":"etag mismatch"}`)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			f.t.Errorf("reading the request body: %v", err)
		}
		var cfg ipn.ServeConfig
		if err := json.Unmarshal(body, &cfg); err != nil {
			f.t.Errorf("decoding the serve config: %v", err)
		}
		f.cfg = cfg
		f.version++
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeTailscaled) client() *local.Client {
	return &local.Client{Transport: f, OmitAuth: true}
}

var options = Options{Listen: "127.0.0.1:8371", Path: "/deploy", Port: 443}

func TestPublishAndUnpublish(t *testing.T) {
	t.Parallel()

	fake := &fakeTailscaled{t: t, cfg: ipn.ServeConfig{Services: service()}}
	api := fake.client()

	if err := publish(t.Context(), api, options); err != nil {
		t.Fatalf("publish: %v", err)
	}
	want := ours()
	want.Services = service()
	if diff := cmp.Diff(want, &fake.cfg, cmpopts.EquateEmpty(), cmpopts.IgnoreFields(ipn.ServeConfig{}, "ETag")); diff != "" {
		t.Errorf("config after publish (-want +got):\n%s", diff)
	}

	if err := publish(t.Context(), api, options); err != nil {
		t.Fatalf("second publish: %v", err)
	}
	if fake.posts != 1 {
		t.Errorf("writes after publishing twice = %d, want 1", fake.posts)
	}

	if err := unpublish(t.Context(), api, options); err != nil {
		t.Fatalf("unpublish: %v", err)
	}
	if diff := cmp.Diff(&ipn.ServeConfig{Services: service()}, &fake.cfg, cmpopts.EquateEmpty(), cmpopts.IgnoreFields(ipn.ServeConfig{}, "ETag")); diff != "" {
		t.Errorf("config after unpublish (-want +got):\n%s", diff)
	}

	if err := unpublish(t.Context(), api, options); err != nil {
		t.Fatalf("second unpublish: %v", err)
	}
	if fake.posts != 2 {
		t.Errorf("writes after unpublishing twice = %d, want 2", fake.posts)
	}
}

func TestPublishRetriesOnConflict(t *testing.T) {
	t.Parallel()

	fake := &fakeTailscaled{
		t: t,
		onFirstGet: func(cfg *ipn.ServeConfig) {
			cfg.Services = service()
		},
	}

	if err := publish(t.Context(), fake.client(), options); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if fake.posts != 2 {
		t.Errorf("writes = %d, want 2: one refused, one accepted", fake.posts)
	}
	want := ours()
	want.Services = service()
	if diff := cmp.Diff(want, &fake.cfg, cmpopts.EquateEmpty(), cmpopts.IgnoreFields(ipn.ServeConfig{}, "ETag")); diff != "" {
		t.Errorf("config keeps the other writer's change (-want +got):\n%s", diff)
	}
}

func TestPublishRefusesAnotherHandler(t *testing.T) {
	t.Parallel()

	other := ours()
	other.Web[hostPort].Handlers["/"] = &ipn.HTTPHandler{Text: "hello"}
	fake := &fakeTailscaled{t: t, cfg: *other.Clone(), version: 7}

	err := publish(t.Context(), fake.client(), options)

	if _, isConflict := errors.AsType[*ConflictError](err); !isConflict {
		t.Fatalf("publish error = %v, want a conflict", err)
	}
	if fake.posts != 0 {
		t.Errorf("writes = %d, want 0", fake.posts)
	}
}

// TestTargetReachesReceiverAtPath replays how tailscaled proxies a handler.
// It cuts the mount point off the request path.
// It joins what remains to the path of the target.
// A request for the mount point itself goes to the target path.
func TestTargetReachesReceiverAtPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		path string
	}{
		{name: "one segment", path: "/deploy"},
		{name: "two segments", path: "/hooks/comin"},
		{name: "root", path: "/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var got string
			receiver := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				got = r.URL.Path
			}))
			t.Cleanup(receiver.Close)

			target, err := url.Parse(Target(strings.TrimPrefix(receiver.URL, "http://"), tt.path))
			if err != nil {
				t.Fatalf("parsing the target: %v", err)
			}
			proxy := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) {
				oldOutPath := r.Out.URL.Path
				r.SetURL(target)
				if oldOutPath == "" && target.Path != "" {
					r.Out.URL.Path = target.Path
				}
			}}
			var handler http.Handler = proxy
			if tt.path != "/" {
				handler = http.StripPrefix(strings.TrimSuffix(tt.path, "/"), proxy)
			}

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodPost, tt.path, http.NoBody))

			if got != tt.path {
				t.Errorf("the receiver got path %q, want %q", got, tt.path)
			}
		})
	}
}
