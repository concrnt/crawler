package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/concrnt/concrnt-search/internal/search/app"
	searchconfig "github.com/concrnt/concrnt-search/internal/search/config"
)

var version = "unknown"

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	cfg, err := searchconfig.LoadFromEnv()
	if err != nil {
		slog.Error("failed to load config", slog.String("error", err.Error()))
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := app.Run(ctx, cfg, version); err != nil {
		slog.Error("concrnt-search stopped with error", slog.String("error", err.Error()))
		os.Exit(1)
	}
}
