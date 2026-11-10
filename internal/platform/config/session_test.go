package config

import (
	"maps"
	"strings"
	"testing"
	"time"
)

func TestLoadSessionConfig(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		want    SessionConfig
		wantErr string
	}{
		{name: "defaults", want: SessionConfig{TTL: 8 * time.Hour, CookieSecure: true}},
		{name: "lower bound", env: map[string]string{"SESSION_TTL": "5m"}, want: SessionConfig{TTL: 5 * time.Minute, CookieSecure: true}},
		{name: "upper bound", env: map[string]string{"SESSION_TTL": "720h"}, want: SessionConfig{TTL: 30 * 24 * time.Hour, CookieSecure: true}},
		{name: "insecure cookie for local http", env: map[string]string{"SESSION_COOKIE_SECURE": "false"}, want: SessionConfig{TTL: 8 * time.Hour}},
		{name: "explicit secure", env: map[string]string{"SESSION_COOKIE_SECURE": "true"}, want: SessionConfig{TTL: 8 * time.Hour, CookieSecure: true}},
		{name: "too short", env: map[string]string{"SESSION_TTL": "4m59s"}, wantErr: "SESSION_TTL must be between 5m and 720h"},
		{name: "too long", env: map[string]string{"SESSION_TTL": "721h"}, wantErr: "SESSION_TTL must be between 5m and 720h"},
		{name: "negative", env: map[string]string{"SESSION_TTL": "-1h"}, wantErr: "SESSION_TTL must be between"},
		{name: "not a duration", env: map[string]string{"SESSION_TTL": "8"}, wantErr: "SESSION_TTL must be between"},
		{name: "not a bool", env: map[string]string{"SESSION_COOKIE_SECURE": "maybe"}, wantErr: "SESSION_COOKIE_SECURE must be true or false"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := map[string]string{"DATABASE_URL": testDatabaseURL}
			maps.Copy(env, tt.env)
			isolateEnvironment(t, env)
			got, err := Load()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("got %v, want error containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Session != tt.want {
				t.Fatalf("got %+v, want %+v", got.Session, tt.want)
			}
		})
	}
}
