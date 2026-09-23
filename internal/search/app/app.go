package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/concrnt/concrnt-crawler/internal/search/api"
	searchconfig "github.com/concrnt/concrnt-crawler/internal/search/config"
	"github.com/concrnt/concrnt-crawler/internal/search/crawler"
	"github.com/concrnt/concrnt-crawler/internal/search/database"
	"github.com/concrnt/concrnt-crawler/internal/search/meili"
	"github.com/concrnt/concrnt-crawler/internal/search/observability"
	"github.com/concrnt/concrnt/client"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/github.com/labstack/echo/otelecho"
)

func Run(ctx context.Context, cfg searchconfig.Config, version string) error {
	if cfg.Observability.EnableTrace {
		cleanup, err := observability.SetupTraceProvider(ctx, cfg.Observability.TraceEndpoint, "concrnt-crawler", version)
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
	sqlDB, err := db.DB()
	if err != nil {
		return fmt.Errorf("connect postgres: %w", err)
	}
	prometheus.MustRegister(collectors.NewDBStatsCollector(sqlDB, "concrnt_crawler"))

	meiliClient := meili.NewClient(cfg.Backends.MeiliHost, cfg.Backends.MeiliAPIKey)
	// a settings update re-indexes the whole index in one task
	searchStore := meili.New(meiliClient, 10*time.Minute, slog.Default())
	if err := searchStore.EnsureIndexes(ctx); err != nil {
		return fmt.Errorf("setup meilisearch indexes: %w", err)
	}

	concrntClient := client.New(cfg.Crawl.Seed)
	concrntClient.SetUserAgent("concrnt-crawler", version)
	concrntClient.GetClient().Timeout = cfg.Crawl.RequestTimeout.Duration()

	searchCrawler := crawler.New(db, searchStore, concrntClient, cfg.Crawl, slog.Default())
	searchCrawler.Start(ctx)

	e := echo.New()
	e.HideBanner = true
	e.HidePort = true
	e.Use(middleware.Recover())
	e.Use(middleware.CORS())
	if cfg.Observability.EnableTrace {
		e.Use(otelecho.Middleware("concrnt-crawler", otelecho.WithSkipper(func(c echo.Context) bool {
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

	api.New(db, searchStore, searchCrawler, cfg.Crawl.ActivityHalfLife.Duration(), cfg.Crawl.AckSchemas).RegisterRoutes(e)

	prometheus.MustRegister(crawler.NewProgressCollector(db, cfg.Crawl.Layer))
	prometheus.MustRegister(meili.NewStatsCollector(searchStore))
	// the usual *_build_info shape: a constant 1 whose labels carry the build
	// identity, so dashboards can filter and join on the running version
	buildInfo := prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace:   "crawler",
		Name:        "build_info",
		Help:        "Build information of the running concrnt-crawler; the value is always 1.",
		ConstLabels: prometheus.Labels{"version": version},
	})
	buildInfo.Set(1)
	prometheus.MustRegister(buildInfo)

	// operational listener: /metrics and /health only, no middleware but
	// Recover; never exposed outside the cluster (the public listener is
	// behind cloudflared)
	internal := echo.New()
	internal.HideBanner = true
	internal.HidePort = true
	internal.Use(middleware.Recover())
	internal.GET("/health", func(c echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
	})
	internal.GET("/metrics", echo.WrapHandler(promhttp.Handler()))

	serverErr := make(chan error, 2)
	go func() {
		slog.Info("concrnt-crawler starting", slog.String("listen", cfg.Server.Listen), slog.String("internalListen", cfg.Server.InternalListen), slog.String("version", version))
		serverErr <- e.Start(cfg.Server.Listen)
	}()
	go func() {
		serverErr <- internal.Start(cfg.Server.InternalListen)
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
	if err := internal.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown internal server: %w", err)
	}
	return nil
}
