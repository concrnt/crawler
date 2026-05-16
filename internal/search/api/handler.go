package api

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/concrnt/concrnt-search/internal/search/crawler"
	"github.com/concrnt/concrnt-search/internal/search/meili"
	"github.com/concrnt/concrnt-search/internal/search/model"
	"github.com/labstack/echo/v4"
	meilisearch "github.com/meilisearch/meilisearch-go"
	"gorm.io/gorm"
)

type SearchStore interface {
	Search(ctx context.Context, indexUID string, query string, limit int64, offset int64, filter string) (*meilisearch.SearchResponse, error)
	Stats(ctx context.Context) (*meilisearch.Stats, error)
}

type Recrawler interface {
	CrawlOnce(ctx context.Context) error
}

type Handler struct {
	db         *gorm.DB
	store      SearchStore
	recrawler  Recrawler
	adminToken string
}

func New(db *gorm.DB, store SearchStore, recrawler Recrawler, adminToken string) *Handler {
	return &Handler{
		db:         db,
		store:      store,
		recrawler:  recrawler,
		adminToken: adminToken,
	}
}

func (h *Handler) RegisterRoutes(e *echo.Echo) {
	e.GET("/health", h.health)
	e.GET("/api/v1/search/users", h.searchUsers)
	e.GET("/api/v1/search/communities", h.searchCommunities)
	e.GET("/api/v1/search/servers", h.searchServers)
	e.GET("/api/v1/stats", h.stats)
	if h.adminToken != "" {
		e.POST("/api/v1/admin/recrawl", h.adminRecrawl)
	}
}

func (h *Handler) health(c echo.Context) error {
	return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) searchUsers(c echo.Context) error {
	filter := meili.BuildFilter(map[string]string{
		"sourceServer": c.QueryParam("sourceServer"),
		"owner":        c.QueryParam("owner"),
	}, map[string]bool{
		"sourceServer": true,
		"owner":        true,
	})
	return h.search(c, meili.UsersIndex, filter)
}

func (h *Handler) searchCommunities(c echo.Context) error {
	filter := meili.BuildFilter(map[string]string{
		"sourceServer": c.QueryParam("sourceServer"),
		"owner":        c.QueryParam("owner"),
	}, map[string]bool{
		"sourceServer": true,
		"owner":        true,
	})
	return h.search(c, meili.CommunitiesIndex, filter)
}

func (h *Handler) searchServers(c echo.Context) error {
	filter := meili.BuildFilter(map[string]string{
		"layer":  c.QueryParam("layer"),
		"status": c.QueryParam("status"),
	}, map[string]bool{
		"layer":  true,
		"status": true,
	})
	return h.search(c, meili.ServersIndex, filter)
}

func (h *Handler) search(c echo.Context, indexUID string, filter string) error {
	limit := parseInt(c.QueryParam("limit"), 20)
	if limit < 1 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	offset := parseInt(c.QueryParam("offset"), 0)
	if offset < 0 {
		offset = 0
	}
	query := c.QueryParam("q")

	resp, err := h.store.Search(c.Request().Context(), indexUID, query, int64(limit), int64(offset), filter)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string]any{
		"hits":               resp.Hits,
		"query":              query,
		"limit":              limit,
		"offset":             offset,
		"estimatedTotalHits": resp.EstimatedTotalHits,
		"processingTimeMs":   resp.ProcessingTimeMs,
	})
}

func (h *Handler) stats(c echo.Context) error {
	ctx := c.Request().Context()
	var serverCount int64
	var cursorCount int64
	var failureCount int64
	var lastCrawled sql.NullTime
	if err := h.db.WithContext(ctx).Model(&model.ServerState{}).Count(&serverCount).Error; err != nil {
		return err
	}
	if err := h.db.WithContext(ctx).Model(&model.CrawlCursor{}).Count(&cursorCount).Error; err != nil {
		return err
	}
	if err := h.db.WithContext(ctx).Model(&model.CrawlCursor{}).Where("fail_count > 0").Count(&failureCount).Error; err != nil {
		return err
	}
	if err := h.db.WithContext(ctx).Model(&model.ServerState{}).Select("MAX(last_crawled_at)").Scan(&lastCrawled).Error; err != nil {
		return err
	}

	meiliStats, err := h.store.Stats(ctx)
	if err != nil {
		return err
	}

	var lastCrawl any
	if lastCrawled.Valid {
		lastCrawl = lastCrawled.Time
	}
	return c.JSON(http.StatusOK, map[string]any{
		"servers": map[string]any{
			"count":       serverCount,
			"lastCrawlAt": lastCrawl,
		},
		"cursors": map[string]any{
			"count":        cursorCount,
			"failureCount": failureCount,
		},
		"meilisearch": meiliStats,
	})
}

type recrawlRequest struct {
	Server   string `json:"server"`
	Kind     string `json:"kind"`
	Backfill bool   `json:"backfill"`
}

func (h *Handler) adminRecrawl(c echo.Context) error {
	if h.adminToken == "" {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	if !strings.HasPrefix(c.Request().Header.Get("Authorization"), "Bearer ") {
		return echo.NewHTTPError(http.StatusUnauthorized)
	}
	token := strings.TrimPrefix(c.Request().Header.Get("Authorization"), "Bearer ")
	if token != h.adminToken {
		return echo.NewHTTPError(http.StatusForbidden)
	}

	var req recrawlRequest
	if err := c.Bind(&req); err != nil {
		return err
	}
	if req.Kind != "" && req.Kind != crawler.KindProfile && req.Kind != crawler.KindCommunity {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid kind")
	}

	query := h.db.WithContext(c.Request().Context()).Model(&model.CrawlCursor{})
	if req.Server != "" {
		query = query.Where("server_domain = ?", req.Server)
	}
	if req.Kind != "" {
		query = query.Where("kind = ?", req.Kind)
	}
	updates := map[string]any{
		"last_error":    "",
		"last_error_at": nil,
		"fail_count":    0,
	}
	if req.Backfill {
		updates["last_backfill_at"] = nil
		updates["backfill_until"] = nil
	}
	if err := query.Updates(updates).Error; err != nil {
		if errors.Is(err, gorm.ErrMissingWhereClause) {
			return echo.NewHTTPError(http.StatusBadRequest, "server or kind is required")
		}
		return err
	}

	if h.recrawler != nil {
		go func() {
			_ = h.recrawler.CrawlOnce(context.Background())
		}()
	}
	return c.JSON(http.StatusAccepted, map[string]string{"status": "scheduled"})
}

func parseInt(value string, fallback int) int {
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}
