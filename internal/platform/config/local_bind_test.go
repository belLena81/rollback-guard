package config

import (
	"fmt"
	"strings"
	"testing"
)

func TestLoadRefusesLocalAuthOffLoopback(t *testing.T) {
	const refused = "requires a loopback HTTP_ADDR"
	tests := []struct {
		addr       string
		allow      string
		github     bool
		wantErr    string
		wantUnsafe bool
	}{
		{addr: "127.0.0.1:8080"},
		{addr: "127.0.0.2:8080"},
		{addr: "[::1]:8080"},
		{addr: "localhost:8080"},
		{addr: "LOCALHOST:8080"},
		{addr: ":8080", wantErr: refused},
		{addr: "0.0.0.0:8080", wantErr: refused},
		{addr: "[::]:8080", wantErr: refused},
		{addr: "10.0.0.5:8080", wantErr: refused},
		{addr: "orchestrator:8080", wantErr: refused},
		{addr: "localhost.example.com:8080", wantErr: refused},
		{addr: "127.0.0.1", wantErr: refused},
		{addr: ":8080", allow: "false", wantErr: refused},
		{addr: ":8080", allow: "yes", wantErr: "ALLOW_INSECURE_LOCAL_AUTH"},
		{addr: ":8080", allow: "true", wantUnsafe: true},
		{addr: "0.0.0.0:8080", allow: "true", wantUnsafe: true},
		{addr: "orchestrator:8080", allow: "true", wantUnsafe: true},
		{addr: "127.0.0.1:8080", allow: "true"},
		// github mode authenticates every request; its bind is not restricted.
		{addr: ":8080", github: true},
		{addr: "0.0.0.0:8080", github: true},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s/allow=%s/github=%v", tt.addr, tt.allow, tt.github), func(t *testing.T) {
			env := map[string]string{"DATABASE_URL": testDatabaseURL}
			if tt.github {
				env = githubEnv()
			}
			env["HTTP_ADDR"] = tt.addr
			env["ALLOW_INSECURE_LOCAL_AUTH"] = tt.allow
			isolateEnvironment(t, env)
			got, err := Load()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Load() error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.HTTPAddr != tt.addr || got.Auth.InsecureLocalBind != tt.wantUnsafe {
				t.Fatalf("HTTPAddr %q InsecureLocalBind %v, want %q %v", got.HTTPAddr, got.Auth.InsecureLocalBind, tt.addr, tt.wantUnsafe)
			}
		})
	}
}
