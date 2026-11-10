package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/platform/config"
	"github.com/belLena81/rollback-guard/internal/platform/httpserver"
)

type coordinatorFunc func(context.Context) error

func (f coordinatorFunc) Run(ctx context.Context) error { return f(ctx) }

func blockUntilCancelled(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

func waitUntilLive(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+httpserver.LivePath, nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := http.DefaultClient.Do(request)
		if err == nil {
			if closeErr := response.Body.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("server on %s did not become live: %v", addr, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func healthOnly() http.Handler {
	mux := http.NewServeMux()
	httpserver.NewHealth(nil, 0, nil).Register(mux)
	return mux
}

func runServe(ctx context.Context, addr string, coordinator coordinatorRunner) <-chan error {
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, config.Config{HTTPAddr: addr}, healthOnly(), coordinator, slog.New(slog.DiscardHandler))
	}()
	return done
}

func awaitServe(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return")
		return nil
	}
}

func TestServeShutsDownCleanlyOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr := freeLoopbackAddr(t)
	done := runServe(ctx, addr, coordinatorFunc(blockUntilCancelled))
	waitUntilLive(t, addr)

	cancel()

	if err := awaitServe(t, done); err != nil {
		t.Fatalf("serve() = %v, want nil after a signal", err)
	}
}

func TestServeStopsWhenTheCoordinatorFails(t *testing.T) {
	failure := errors.New("coordinator failed")
	release := make(chan struct{})
	addr := freeLoopbackAddr(t)
	done := runServe(context.Background(), addr, coordinatorFunc(func(context.Context) error {
		<-release
		return failure
	}))
	waitUntilLive(t, addr)

	close(release)

	if err := awaitServe(t, done); !errors.Is(err, failure) {
		t.Fatalf("serve() = %v, want the coordinator failure", err)
	}
}

func TestServeReportsCoordinatorFailureDuringShutdown(t *testing.T) {
	failure := errors.New("lease release failed")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr := freeLoopbackAddr(t)
	done := runServe(ctx, addr, coordinatorFunc(func(ctx context.Context) error {
		<-ctx.Done()
		return failure
	}))
	waitUntilLive(t, addr)

	cancel()

	if err := awaitServe(t, done); !errors.Is(err, failure) {
		t.Fatalf("serve() = %v, want the coordinator failure reported after shutdown", err)
	}
}

func TestServeReportsListenFailure(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := occupied.Close(); err != nil {
			t.Errorf("close listener: %v", err)
		}
	}()
	ctx := t.Context()

	err = awaitServe(t, runServe(ctx, occupied.Addr().String(), coordinatorFunc(blockUntilCancelled)))

	if err == nil || !strings.Contains(err.Error(), "orchestrator listen on") {
		t.Fatalf("serve() = %v, want a listen error naming the service", err)
	}
}
