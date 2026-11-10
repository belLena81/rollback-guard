package httpserver

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/platform/config"
)

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// startServer runs serve with the given timeouts and returns the base URL
// and a stop function that cancels it and returns its error.
func startServer(t *testing.T, handler http.Handler, limits timeouts) (string, func() error) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, listener, "test", handler, discardLogger(), limits) }()
	var stopped atomic.Bool
	stop := func() error {
		if stopped.Swap(true) {
			return nil
		}
		cancel()
		select {
		case err := <-done:
			return err
		case <-time.After(5 * time.Second):
			return errors.New("server did not stop")
		}
	}
	t.Cleanup(func() {
		if err := stop(); err != nil {
			t.Errorf("stop server: %v", err)
		}
	})
	return "http://" + listener.Addr().String(), stop
}

// longHandler mimics the two long-running response shapes the services
// produce. /buffered answers once, late, like the MCP watch_rollout tool
// (JSON response mode). /stream flushes an event, waits, and sends another,
// like an A2A task subscription. It records whether the request context was
// cancelled while it worked.
func longHandler(work time.Duration, cancelled *atomic.Bool) http.Handler {
	wait := func(r *http.Request) {
		select {
		case <-r.Context().Done():
			cancelled.Store(true)
		case <-time.After(work):
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /buffered", func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			cancelled.Store(true)
		}
		wait(r)
		w.Header().Set("Content-Type", "application/json")
		writeBody(w, `{"complete":true}`)
	})
	mux.HandleFunc("GET /stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeBody(w, "data: working\n\n")
		if err := http.NewResponseController(w).Flush(); err != nil {
			cancelled.Store(true)
		}
		wait(r)
		writeBody(w, "data: completed\n\n")
	})
	return mux
}

// writeBody writes a response chunk. A failed write (the connection was cut)
// is detected by the client-side assertions.
func writeBody(w io.Writer, chunk string) {
	if _, err := io.WriteString(w, chunk); err != nil {
		return
	}
}

func fetch(t *testing.T, method, url string) (string, error) {
	t.Helper()
	var body io.Reader
	if method == http.MethodPost {
		body = strings.NewReader(`{"timeoutSeconds":120}`)
	}
	request, err := http.NewRequestWithContext(t.Context(), method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return "", err
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			t.Errorf("close response body: %v", err)
		}
	}()
	data, err := io.ReadAll(response.Body)
	return string(data), err
}

func TestLongResponsesOutliveEveryTimeoutExceptWrite(t *testing.T) {
	const work = 400 * time.Millisecond
	// Everything but the write timeout expires while the handler works.
	limits := timeouts{readHeader: 50 * time.Millisecond, read: 50 * time.Millisecond, idle: 50 * time.Millisecond, write: 10 * time.Second, shutdown: time.Second}
	tests := []struct {
		name, method, path, want string
	}{
		{name: "late buffered reply (MCP watch_rollout)", method: http.MethodPost, path: "/buffered", want: `{"complete":true}`},
		{name: "event stream (A2A task subscription)", method: http.MethodGet, path: "/stream", want: "data: working\n\ndata: completed\n\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var cancelled atomic.Bool
			url, _ := startServer(t, longHandler(work, &cancelled), limits)

			got, err := fetch(t, test.method, url+test.path)

			if err != nil {
				t.Fatalf("response was cut off: %v (partial body %q)", err, got)
			}
			if got != test.want {
				t.Fatalf("body = %q, want %q", got, test.want)
			}
			if cancelled.Load() {
				t.Fatal("request context was cancelled while the handler worked")
			}
		})
	}
}

// The write timeout truncates a response that is still being produced and
// does not tell the handler. This is why WriteTimeout must exceed the
// longest legitimate response.
func TestWriteTimeoutCutsOffSlowerResponses(t *testing.T) {
	limits := timeouts{readHeader: time.Second, read: time.Second, idle: time.Second, write: 50 * time.Millisecond, shutdown: time.Second}
	for _, test := range []struct{ method, path, complete string }{
		{method: http.MethodPost, path: "/buffered", complete: `{"complete":true}`},
		{method: http.MethodGet, path: "/stream", complete: "data: working\n\ndata: completed\n\n"},
	} {
		t.Run(test.path, func(t *testing.T) {
			var cancelled atomic.Bool
			url, _ := startServer(t, longHandler(400*time.Millisecond, &cancelled), limits)

			got, err := fetch(t, test.method, url+test.path)

			if err == nil && got == test.complete {
				t.Fatal("response survived the write timeout; the timeout is not applied")
			}
		})
	}
}

func TestWriteTimeoutExceedsLongestConfigurableResponse(t *testing.T) {
	// Upper bound on MCP calls one orchestrator approve request makes around
	// its rollout watch (inspect, prepare, execute, verify), with headroom.
	const approveCalls = 8
	longest := map[string]time.Duration{
		"MCP watch_rollout":    domain.MaxRolloutWatch + config.MaxMCPCallTimeout,
		"A2A task stream":      config.MaxA2ATaskTimeout + config.MaxA2ACancelTimeout,
		"orchestrator approve": domain.MaxRolloutWatch + approveCalls*config.MaxMCPCallTimeout,
		"Prometheus queries":   approveCalls * config.MaxPrometheusTimeout,
	}
	for name, duration := range longest {
		if WriteTimeout < duration+time.Minute {
			t.Errorf("WriteTimeout %s leaves less than a minute of slack over %s (%s)", WriteTimeout, name, duration)
		}
	}
	if IdleTimeout <= 90*time.Second {
		t.Errorf("IdleTimeout %s must exceed the 90s idle timeout of Go HTTP clients", IdleTimeout)
	}
	if got := defaultTimeouts(); got != (timeouts{readHeader: ReadHeaderTimeout, read: ReadTimeout, write: WriteTimeout, idle: IdleTimeout, shutdown: ShutdownTimeout}) {
		t.Errorf("defaultTimeouts() = %+v does not use the named constants", got)
	}
}

func TestReadHeaderTimeoutDropsStalledClients(t *testing.T) {
	limits := timeouts{readHeader: 50 * time.Millisecond, read: time.Second, idle: time.Second, write: time.Second, shutdown: time.Second}
	url, _ := startServer(t, http.NotFoundHandler(), limits)
	conn, err := net.Dial("tcp", strings.TrimPrefix(url, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			t.Errorf("close connection: %v", err)
		}
	}()
	if _, err := io.WriteString(conn, "GET / HTTP/1.1\r\nHost: stalled\r\n"); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}

	// The server answers 408 or just closes; either way the read ends
	// instead of hitting the client's own deadline.
	_, err = io.ReadAll(bufio.NewReader(conn))

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		t.Fatal("server kept a connection with unfinished headers open past ReadHeaderTimeout")
	}
}

func TestServeFinishesInFlightRequestsOnShutdown(t *testing.T) {
	limits := timeouts{readHeader: time.Second, read: time.Second, idle: time.Second, write: 5 * time.Second, shutdown: 5 * time.Second}
	started := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		time.Sleep(200 * time.Millisecond)
		writeBody(w, "done")
	})
	url, stop := startServer(t, handler, limits)
	type result struct {
		body string
		err  error
	}
	results := make(chan result, 1)
	go func() {
		body, err := fetch(t, http.MethodGet, url)
		results <- result{body, err}
	}()
	<-started

	if err := stop(); err != nil {
		t.Fatalf("serve returned %v after a graceful shutdown, want nil", err)
	}

	got := <-results
	if got.err != nil || got.body != "done" {
		t.Fatalf("in-flight request: body = %q err = %v", got.body, got.err)
	}
	if _, err := fetch(t, http.MethodGet, url); err == nil {
		t.Fatal("server still accepts requests after shutdown")
	}
}

func TestServeReportsShutdownTimeout(t *testing.T) {
	limits := timeouts{readHeader: time.Second, read: time.Second, idle: time.Second, write: 5 * time.Second, shutdown: 20 * time.Millisecond}
	started := make(chan struct{})
	release := make(chan struct{})
	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(started)
		<-release
	})
	url, stop := startServer(t, handler, limits)
	go func() {
		// The request outlives the shutdown timeout; only that it was in
		// flight matters.
		if _, err := fetch(t, http.MethodGet, url); err != nil {
			return
		}
	}()
	<-started

	err := stop()
	close(release)

	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "test shutdown") {
		t.Fatalf("serve error = %v, want a named shutdown deadline error", err)
	}
}

func TestServeListensAndStops(t *testing.T) {
	// Reserve a free port, release it, and let Serve bind it.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mux := http.NewServeMux()
	NewHealth(nil, 0, nil).Register(mux)
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, Config{Name: "test", Addr: addr}, mux, discardLogger()) }()

	var body string
	deadline := time.Now().Add(5 * time.Second)
	for {
		body, err = fetch(t, http.MethodGet, "http://"+addr+LivePath)
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil || body != HealthOKBody {
		t.Fatalf("GET %s: body = %q err = %v", LivePath, body, err)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve() = %v, want nil after cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after cancellation")
	}
}

func TestServeRejectsInvalidInput(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := occupied.Close(); err != nil {
			t.Errorf("close listener: %v", err)
		}
	}()
	handler := http.NotFoundHandler()
	tests := []struct {
		name    string
		cfg     Config
		handler http.Handler
		logger  *slog.Logger
		want    string
	}{
		{name: "missing name", cfg: Config{Addr: "127.0.0.1:0"}, handler: handler, logger: discardLogger(), want: "name is required"},
		{name: "missing handler", cfg: Config{Name: "svc", Addr: "127.0.0.1:0"}, logger: discardLogger(), want: "svc HTTP server requires"},
		{name: "missing logger", cfg: Config{Name: "svc", Addr: "127.0.0.1:0"}, handler: handler, want: "svc HTTP server requires"},
		{name: "missing address", cfg: Config{Name: "svc", Addr: " "}, handler: handler, logger: discardLogger(), want: "svc listen address is required"},
		{name: "address in use", cfg: Config{Name: "svc", Addr: occupied.Addr().String()}, handler: handler, logger: discardLogger(), want: "svc listen on " + occupied.Addr().String()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := Serve(context.Background(), test.cfg, test.handler, test.logger)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Serve() error = %v, want %q", err, test.want)
			}
		})
	}
}
