package httpserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
)

// Server timeouts shared by every Rollback Guardian service.
const (
	// ReadHeaderTimeout bounds how long a client may take to send headers.
	ReadHeaderTimeout = 5 * time.Second
	// ReadTimeout bounds reading one whole request. Bodies are capped at
	// 1 MiB by the handlers. It does not limit how long a handler may then
	// take to respond.
	ReadTimeout = 30 * time.Second
	// WriteTimeout bounds one response, measured from the end of the request
	// headers. net/http enforces it by closing the connection without
	// cancelling the request context, so a response that is still being
	// produced is silently truncated. It therefore has to exceed the longest
	// legitimate response of any service: the MCP watch_rollout call (one
	// buffered JSON reply after up to domain.MaxRolloutWatch), an A2A task
	// stream (open for up to the maximum A2A_TASK_TIMEOUT), and the
	// orchestrator approve request (one rollout watch plus a handful of
	// bounded MCP calls). All of those are capped by configuration at a few
	// minutes; TestWriteTimeoutExceedsLongestConfigurableResponse pins the
	// relation.
	WriteTimeout = 10 * time.Minute
	// IdleTimeout is longer than the 90s idle timeout of Go HTTP clients, so
	// the client is the side that retires an idle keep-alive connection and
	// a request never races with the server closing it.
	IdleTimeout = 2 * time.Minute
	// ShutdownTimeout bounds graceful shutdown after the context is cancelled.
	ShutdownTimeout = 10 * time.Second
)

// Config identifies one listening service.
type Config struct {
	// Name labels the log lines and errors, for example "orchestrator".
	Name string
	// Addr is the TCP listen address, for example "127.0.0.1:8080".
	Addr string
}

type timeouts struct {
	readHeader, read, write, idle, shutdown time.Duration
}

func defaultTimeouts() timeouts {
	return timeouts{readHeader: ReadHeaderTimeout, read: ReadTimeout, write: WriteTimeout, idle: IdleTimeout, shutdown: ShutdownTimeout}
}

// Serve listens on cfg.Addr and serves handler until ctx is cancelled, then
// shuts down gracefully within ShutdownTimeout. It returns nil after a clean
// shutdown and an error when listening, serving, or shutting down fails.
func Serve(ctx context.Context, cfg Config, handler http.Handler, logger *slog.Logger) error {
	if err := validate(cfg, handler, logger); err != nil {
		return err
	}
	if strings.TrimSpace(cfg.Addr) == "" {
		return fmt.Errorf("%s listen address is required", cfg.Name)
	}
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(ctx, "tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("%s listen on %s: %w", cfg.Name, cfg.Addr, err)
	}
	return serve(ctx, listener, cfg.Name, handler, logger, defaultTimeouts())
}

func validate(cfg Config, handler http.Handler, logger *slog.Logger) error {
	if strings.TrimSpace(cfg.Name) == "" {
		return errors.New("HTTP server name is required")
	}
	if handler == nil || logger == nil {
		return fmt.Errorf("%s HTTP server requires a handler and a logger", cfg.Name)
	}
	return nil
}

func serve(ctx context.Context, listener net.Listener, name string, handler http.Handler, logger *slog.Logger, limits timeouts) error {
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: limits.readHeader,
		ReadTimeout:       limits.read,
		WriteTimeout:      limits.write,
		IdleTimeout:       limits.idle,
	}
	errCh := make(chan error, 1)
	go func() {
		logger.InfoContext(ctx, name+" listening", "addr", listener.Addr().String())
		errCh <- server.Serve(listener)
	}()
	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("%s serve: %w", name, err)
	case <-ctx.Done():
		// The parent context is already cancelled; shutdown needs its own
		// deadline but keeps the context values.
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), limits.shutdown)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("%s shutdown: %w", name, err)
		}
		return nil
	}
}
