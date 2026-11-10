package demoapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"
)

func listen(t *testing.T) net.Listener {
	t.Helper()
	var lc net.ListenConfig
	listener, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return listener
}

func TestServe(t *testing.T) {
	tests := []struct {
		name       string
		mode       Mode
		crashAfter time.Duration
		cancel     bool
		wantErr    error
	}{
		{name: "healthy shuts down gracefully on cancel", mode: ModeHealthy, cancel: true},
		{name: "error storm shuts down gracefully on cancel", mode: ModeErrorStorm, cancel: true},
		{name: "crashloop exits with error", mode: ModeCrashLoop, crashAfter: 50 * time.Millisecond, wantErr: ErrCrashLoop},
		{name: "crashloop cancelled before crash exits cleanly", mode: ModeCrashLoop, crashAfter: time.Hour, cancel: true},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := testConfig(test.mode)
			cfg.CrashAfter = test.crashAfter
			handler := newTestHandler(t, cfg, 0, &recordingWait{})
			listener := listen(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			done := make(chan error, 1)
			go func() { done <- Serve(ctx, listener, handler, cfg, logger) }()

			if test.mode != ModeCrashLoop {
				request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+listener.Addr().String()+"/healthz", http.NoBody)
				if err != nil {
					t.Fatal(err)
				}
				response, err := http.DefaultClient.Do(request)
				if err != nil {
					t.Fatal(err)
				}
				if err := response.Body.Close(); err != nil {
					t.Fatal(err)
				}
				if response.StatusCode != http.StatusOK {
					t.Fatalf("healthz = %d", response.StatusCode)
				}
			}
			if test.cancel {
				cancel()
			}
			select {
			case err := <-done:
				if !errors.Is(err, test.wantErr) || (test.wantErr == nil && err != nil) {
					t.Fatalf("Serve err = %v, want %v", err, test.wantErr)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Serve did not return")
			}
		})
	}
}

func TestServeValidatesArguments(t *testing.T) {
	cfg := testConfig(ModeHealthy)
	handler := newTestHandler(t, cfg, 0, &recordingWait{})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := Serve(t.Context(), nil, handler, cfg, logger); err == nil {
		t.Fatal("expected error for nil listener")
	}
	listener := listen(t)
	t.Cleanup(func() {
		if err := listener.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := Serve(t.Context(), listener, nil, cfg, logger); err == nil {
		t.Fatal("expected error for nil handler")
	}
	if err := Serve(t.Context(), listener, handler, cfg, nil); err == nil {
		t.Fatal("expected error for nil logger")
	}
}
