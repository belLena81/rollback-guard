package demoapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// ErrCrashLoop is returned by Serve when a crashloop build deliberately exits.
var ErrCrashLoop = errors.New("demo-api crashloop mode: simulated fatal startup failure")

const (
	shutdownTimeout   = 10 * time.Second
	readHeaderTimeout = 5 * time.Second
	writeTimeoutSlack = 10 * time.Second
)

// Serve runs handler on listener until ctx is cancelled (graceful shutdown,
// nil error) or, in crashloop mode, until CrashAfter elapses (ErrCrashLoop).
func Serve(ctx context.Context, listener net.Listener, handler http.Handler, cfg Config, logger *slog.Logger) error {
	if listener == nil || handler == nil || logger == nil {
		return errors.New("demo-api serve requires listener, handler and logger")
	}
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		WriteTimeout:      cfg.WorkLatency + writeTimeoutSlack,
		IdleTimeout:       time.Minute,
	}
	errCh := make(chan error, 1)
	go func() {
		logger.Info("demo-api listening", "addr", listener.Addr().String(), "mode", string(cfg.Mode), "version", cfg.Version)
		errCh <- server.Serve(listener)
	}()

	var crash <-chan time.Time
	if cfg.Mode == ModeCrashLoop {
		timer := time.NewTimer(cfg.CrashAfter)
		defer timer.Stop()
		crash = timer.C
	}

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("demo-api serve: %w", err)
	case <-crash:
		if err := server.Close(); err != nil {
			logger.Warn("demo-api close before crash", "error", err)
		}
		logger.Error("demo-api crashing", "mode", string(cfg.Mode))
		return ErrCrashLoop
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("demo-api shutdown: %w", err)
		}
		return nil
	}
}
