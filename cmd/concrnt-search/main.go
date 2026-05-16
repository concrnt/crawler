package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/concrnt/concrnt-search/internal/search/api"
	searchconfig "github.com/concrnt/concrnt-search/internal/search/config"
	"github.com/concrnt/concrnt-search/internal/search/crawler"
	"github.com/concrnt/concrnt-search/internal/search/database"
	"github.com/concrnt/concrnt-search/internal/search/meili"
	"github.com/concrnt/concrnt-search/internal/search/observability"
	"github.com/concrnt/concrnt/client"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"go.opentelemetry.io/contrib/instrumentation/github.com/labstack/echo/otelecho"
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

	if cfg.Observability.EnableTrace {
		cleanup, err := observability.SetupTraceProvider(ctx, cfg.Observability.TraceEndpoint, "concrnt-search", version)
		if err != nil {
			slog.Error("failed to setup tracing", slog.String("error", err.Error()))
			os.Exit(1)
		}
		defer cleanup()
	}

	db, err := database.OpenPostgres(cfg.Backends.PostgresDsn)
	if err != nil {
		slog.Error("failed to connect postgres", slog.String("error", err.Error()))
		os.Exit(1)
	}
	if err := database.Migrate(db); err != nil {
		slog.Error("failed to migrate postgres", slog.String("error", err.Error()))
		os.Exit(1)
	}

	meiliClient := meili.NewClient(cfg.Backends.MeiliHost, cfg.Backends.MeiliAPIKey)
	searchStore := meili.New(meiliClient, 2*time.Minute, slog.Default())
	if err := searchStore.EnsureIndexes(ctx); err != nil {
		slog.Error("failed to setup meilisearch indexes", slog.String("error", err.Error()))
		os.Exit(1)
	}

	concrntClient := client.New(cfg.Crawl.Seed)
	concrntClient.SetUserAgent("concrnt-search", version)
	concrntClient.GetClient().Timeout = cfg.Crawl.RequestTimeout.Duration()

	searchCrawler := crawler.New(db, searchStore, concrntClient, cfg.Crawl, slog.Default())
	searchCrawler.Start(ctx)

	e := echo.New()
	e.HideBanner = true
	e.HidePort = true
	e.Use(middleware.Recover())
	if cfg.Observability.EnableTrace {
		e.Use(otelecho.Middleware("concrnt-search", otelecho.WithSkipper(func(c echo.Context) bool {
			return c.Path() == "/health"
		})))
	}
	e.Use(middleware.LoggerWithConfig(middleware.LoggerConfig{
		Skipper: func(c echo.Context) bool {
			return c.Path() == "/health"
		},
		Format: `{"time":"${time_rfc3339_nano}","remote_ip":"${remote_ip}",` +
			`"host":"${host}","method":"${method}","uri":"${uri}","status":${status},` +
			`"error":"${error}","latency":${latency},"bytes_in":${bytes_in},"bytes_out":${bytes_out}}` + "\n",
	}))

	api.New(db, searchStore, searchCrawler, cfg.Server.AdminToken).RegisterRoutes(e)

	serverErr := make(chan error, 1)
	go func() {
		slog.Info("concrnt-search starting", slog.String("listen", cfg.Server.Listen), slog.String("version", version))
		serverErr <- e.Start(cfg.Server.Listen)
	}()

	select {
	case <-ctx.Done():
	case err := <-serverErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("http server failed", slog.String("error", err.Error()))
			os.Exit(1)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := e.Shutdown(shutdownCtx); err != nil {
		fmt.Fprintln(os.Stderr, "failed to shutdown server:", err)
		os.Exit(1)
	}
}
