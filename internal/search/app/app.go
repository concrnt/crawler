package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
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

func Run(ctx context.Context, cfg searchconfig.Config, version string) error {
	if cfg.Observability.EnableTrace {
		cleanup, err := observability.SetupTraceProvider(ctx, cfg.Observability.TraceEndpoint, "concrnt-search", version)
		if err != nil {
			return fmt.Errorf("setup tracing: %w", err)
		}
		defer cleanup()
	}

	db, err := database.OpenPostgres(cfg.Backends.PostgresDsn)
	if err != nil {
		return fmt.Errorf("connect postgres: %w", err)
	}
	if err := database.Migrate(db); err != nil {
		return fmt.Errorf("migrate postgres: %w", err)
	}

	meiliClient := meili.NewClient(cfg.Backends.MeiliHost, cfg.Backends.MeiliAPIKey)
	searchStore := meili.New(meiliClient, 2*time.Minute, slog.Default())
	if err := searchStore.EnsureIndexes(ctx); err != nil {
		return fmt.Errorf("setup meilisearch indexes: %w", err)
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

	api.New(db, searchStore, searchCrawler).RegisterRoutes(e)

	serverErr := make(chan error, 1)
	go func() {
		slog.Info("concrnt-search starting", slog.String("listen", cfg.Server.Listen), slog.String("version", version))
		serverErr <- e.Start(cfg.Server.Listen)
	}()

	select {
	case <-ctx.Done():
	case err := <-serverErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("http server failed: %w", err)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := e.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown server: %w", err)
	}
	return nil
}
