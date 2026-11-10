package main

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/belLena81/rollback-guard/internal/demoapi"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	err := run(ctx, logger)
	stop()
	if err != nil {
		logger.Error("demo-api exited", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, logger *slog.Logger) error {
	cfg, err := demoapi.LoadConfig(os.LookupEnv)
	if err != nil {
		return err
	}
	handler, err := demoapi.NewHandler(cfg, demoapi.Deps{Sample: rand.Float64, Wait: demoapi.SleepContext, Logger: logger})
	if err != nil {
		return err
	}
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(ctx, "tcp", cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.HTTPAddr, err)
	}
	return demoapi.Serve(ctx, listener, handler, cfg, logger)
}
