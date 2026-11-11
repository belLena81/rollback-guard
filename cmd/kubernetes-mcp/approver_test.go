package main

import (
	"testing"

	"github.com/belLena81/rollback-guard/internal/platform/config"
)

func TestApproverVerifierFollowsTheAuthModeAndFailsClosed(t *testing.T) {
	tests := []struct {
		name    string
		cfg     config.ApproverAuthConfig
		wantErr bool
	}{
		{name: "github", cfg: config.ApproverAuthConfig{Mode: config.AuthModeGitHub}},
		{name: "local", cfg: config.ApproverAuthConfig{Mode: config.AuthModeLocal, LocalActorID: "local-admin"}},
		{name: "local with a github actor", cfg: config.ApproverAuthConfig{Mode: config.AuthModeLocal, LocalActorID: "github:1001"}, wantErr: true},
		{name: "local without an actor", cfg: config.ApproverAuthConfig{Mode: config.AuthModeLocal}, wantErr: true},
		{name: "unset mode", cfg: config.ApproverAuthConfig{}, wantErr: true},
		{name: "unknown mode", cfg: config.ApproverAuthConfig{Mode: "none"}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			verifier, err := approverVerifier(test.cfg)
			if test.wantErr {
				if err == nil || verifier != nil {
					t.Fatalf("approverVerifier() = %v, %v; want an error and no verifier", verifier, err)
				}
				return
			}
			if err != nil || verifier == nil {
				t.Fatalf("approverVerifier() = %v, %v", verifier, err)
			}
		})
	}
}
