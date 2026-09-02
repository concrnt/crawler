package api

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/concrnt/concrnt-crawler/internal/search/crawler"
	"github.com/concrnt/concrnt-crawler/internal/search/meili"
	"github.com/concrnt/concrnt-crawler/internal/search/model"
	"github.com/labstack/echo/v4"
	meilisearch "github.com/meilisearch/meilisearch-go"
	"gorm.io/gorm"
)

type SearchStore interface {
	Search(ctx context.Context, indexUID string, query string, limit int64, offset int64, filter string, sort []string) (*meilisearch.SearchResponse, error)
	Stats(ctx context.Context) (*meilisearch.Stats, error)
}

type CCFSCrawler interface {
	CrawlCCFS(ctx context.Context, ccfs string) (crawler.ManualCrawlResult, error)
}

type Handler struct {
	db      *gorm.DB
	store   SearchStore
	crawler CCFSCrawler
}

func New(db *gorm.DB, store SearchStore, crawler CCFSCrawler) *Handler {
	return &Handler{
		db:      db,
		store:   store,
		crawler: crawler,
	}
}

func (h *Handler) RegisterRoutes(e *echo.Echo) {
	e.GET("/health", h.health)
	e.GET("/api/v1/search/users", h.searchUsers)
	e.GET("/api/v1/search/communities", h.searchCommunities)
	e.GET("/api/v1/search/servers", h.searchServers)
	e.GET("/api/v1/stats", h.stats)
	e.POST("/api/v1/crawl/ccfs", h.crawlCCFS)
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
	// must stay in sync with the sortable attributes in meili.EnsureIndexes
	return h.search(c, meili.UsersIndex, filter, map[string]bool{
		"createdAt": true,
		"indexedAt": true,
		"username":  true,
	})
}

func (h *Handler) searchCommunities(c echo.Context) error {
	filter := meili.BuildFilter(map[string]string{
		"sourceServer": c.QueryParam("sourceServer"),
		"owner":        c.QueryParam("owner"),
	}, map[string]bool{
		"sourceServer": true,
		"owner":        true,
	})
	return h.search(c, meili.CommunitiesIndex, filter, map[string]bool{
		"createdAt": true,
		"indexedAt": true,
		"name":      true,
	})
}

func (h *Handler) searchServers(c echo.Context) error {
	filter := meili.BuildFilter(map[string]string{
		"status": c.QueryParam("status"),
	}, map[string]bool{
		"status": true,
	})
	return h.search(c, meili.ServersIndex, filter, map[string]bool{
		"lastSeenAt":    true,
		"lastCrawledAt": true,
	})
}

func (h *Handler) search(c echo.Context, indexUID string, filter string, sortable map[string]bool) error {
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
	sort, err := meili.BuildSort(c.QueryParam("sort"), sortable)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	resp, err := h.store.Search(c.Request().Context(), indexUID, query, int64(limit), int64(offset), filter, sort)
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

type crawlCCFSRequest struct {
	CCFS string `json:"ccfs"`
}

func (h *Handler) crawlCCFS(c echo.Context) error {
	ccfs, err := readCCFS(c)
	if err != nil {
		return err
	}
	if ccfs == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "ccfs is required")
	}
	result, err := h.crawler.CrawlCCFS(c.Request().Context(), ccfs)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	return c.JSON(http.StatusOK, map[string]any{
		"status": "indexed",
		"result": result,
	})
}

func readCCFS(c echo.Context) (string, error) {
	contentType := c.Request().Header.Get("Content-Type")
	if strings.HasPrefix(contentType, "application/json") {
		var req crawlCCFSRequest
		if err := c.Bind(&req); err != nil {
			return "", err
		}
		return strings.TrimSpace(req.CCFS), nil
	}
	if value := strings.TrimSpace(c.FormValue("ccfs")); value != "" {
		return value, nil
	}
	body, err := io.ReadAll(c.Request().Body)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(body)), nil
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
