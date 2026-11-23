package config

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func envFrom(env map[string]string) func(string) string {
	return func(key string) string { return env[key] }
}

func TestParseServe(t *testing.T) {
	t.Parallel()

	base := Serve{
		Listen:      DefaultListen,
		Path:        DefaultPath,
		Issuer:      DefaultIssuer,
		Audience:    "https://example.com/hook",
		Subjects:    []string{"repo:example-org/example-repo:ref:refs/heads/main"},
		Claims:      map[string]string{},
		CominSocket: DefaultCominSocket,
	}
	required := []string{"--audience", base.Audience, "--subject", base.Subjects[0]}

	tests := []struct {
		name    string
		args    []string
		env     map[string]string
		want    Serve
		wantErr string
	}{
		{name: "flags with defaults", args: required, want: base},
		{
			name: "environment fills what flags leave unset",
			env: map[string]string{
				"COMIN_HOOK_AUDIENCE":     base.Audience,
				"COMIN_HOOK_SUBJECT":      "sub-a, sub-b",
				"COMIN_HOOK_CLAIM":        "event_name=push,ref=refs/heads/main",
				"COMIN_HOOK_LISTEN":       "127.0.0.1:9000",
				"COMIN_HOOK_PATH":         "/hook",
				"COMIN_HOOK_COMIN_SOCKET": "/run/comin.sock",
			},
			want: Serve{
				Listen:      "127.0.0.1:9000",
				Path:        "/hook",
				Issuer:      DefaultIssuer,
				Audience:    base.Audience,
				Subjects:    []string{"sub-a", "sub-b"},
				Claims:      map[string]string{"event_name": "push", "ref": "refs/heads/main"},
				CominSocket: "/run/comin.sock",
			},
		},
		{
			name: "a flag wins over the environment",
			args: []string{"--audience", "from-flag", "--subject", "s", "--listen", "127.0.0.1:1"},
			env:  map[string]string{"COMIN_HOOK_AUDIENCE": "from-env", "COMIN_HOOK_LISTEN": "127.0.0.1:2"},
			want: Serve{
				Listen: "127.0.0.1:1", Path: DefaultPath, Issuer: DefaultIssuer, Audience: "from-flag",
				Subjects: []string{"s"}, Claims: map[string]string{}, CominSocket: DefaultCominSocket,
			},
		},
		{
			name: "a repeated flag collects its values",
			args: []string{"--audience", "a", "--subject", "s1", "--subject", "s2", "--claim", "k=v=w"},
			want: Serve{
				Listen: DefaultListen, Path: DefaultPath, Issuer: DefaultIssuer, Audience: "a",
				Subjects: []string{"s1", "s2"}, Claims: map[string]string{"k": "v=w"}, CominSocket: DefaultCominSocket,
			},
		},
		{name: "missing audience", args: []string{"--subject", "s"}, wantErr: "--audience is required"},
		{name: "missing subject", args: []string{"--audience", "a"}, wantErr: "--subject is required"},
		{name: "claim without a value", args: append([]string{"--claim", "novalue"}, required...), wantErr: "key=value"},
		{name: "claim without a key", args: append([]string{"--claim", "=v"}, required...), wantErr: "key=value"},
		{name: "http issuer", args: append([]string{"--issuer", "http://issuer.example.com"}, required...), wantErr: "https URL"},
		{name: "relative path", args: append([]string{"--path", "deploy"}, required...), wantErr: "--path"},
		{name: "trailing slash", args: append([]string{"--path", "/deploy/"}, required...), wantErr: "--path"},
		{name: "listen without a port", args: append([]string{"--listen", "localhost"}, required...), wantErr: "--listen"},
		{name: "relative socket", args: append([]string{"--comin-socket", "grpc.sock"}, required...), wantErr: "--comin-socket"},
		{name: "unknown flag", args: append([]string{"--nope"}, required...), wantErr: "flag provided but not defined"},
		{name: "stray argument", args: append([]string{"extra"}, required...), wantErr: "unexpected argument"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseServe(tt.args, envFrom(tt.env))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want one containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("config mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestParseFunnel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		args    []string
		env     map[string]string
		want    Funnel
		wantErr string
	}{
		{
			name: "defaults",
			want: Funnel{
				Listen: DefaultListen, Path: DefaultPath, FunnelPort: 443, TailscaledSocket: DefaultTailscaledSocket,
			},
		},
		{
			name: "environment",
			env:  map[string]string{"COMIN_HOOK_FUNNEL_PORT": "8443", "COMIN_HOOK_TAILSCALED_SOCKET": "/run/ts.sock"},
			want: Funnel{Listen: DefaultListen, Path: DefaultPath, FunnelPort: 8443, TailscaledSocket: "/run/ts.sock"},
		},
		{
			name: "port 10000",
			args: []string{"--funnel-port", "10000"},
			want: Funnel{Listen: DefaultListen, Path: DefaultPath, FunnelPort: 10000, TailscaledSocket: DefaultTailscaledSocket},
		},
		{name: "port Funnel does not allow", args: []string{"--funnel-port", "8080"}, wantErr: "443, 8443 or 10000"},
		{name: "port zero", args: []string{"--funnel-port", "0"}, wantErr: "443, 8443 or 10000"},
		{name: "listen without a host", args: []string{"--listen", ":8371"}, wantErr: "needs a host"},
		{name: "bad path", args: []string{"--path", "/a/../b"}, wantErr: "--path"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseFunnel("publish", tt.args, envFrom(tt.env))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want one containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("config mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
