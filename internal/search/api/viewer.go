package api

import (
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/concrnt/concrnt"
	"github.com/concrnt/concrnt-crawler/internal/search/crawler"
	"github.com/concrnt/concrnt-crawler/internal/search/meili"
	"github.com/concrnt/concrnt-crawler/internal/search/model"
	"github.com/labstack/echo/v4"
	"gorm.io/gorm"
)

// topAuthorsLimit is how many of the viewer's followees a community hit
// names, most posts first.
const topAuthorsLimit = 5

// followeeRank is the activity of one key (a CCID or a community) among the
// viewer's followees over the activity window, scored like the global
// activity (2^(-age/halfLife) per entry).
type followeeRank struct {
	Key     string
	Score   float64
	Count   int
	Authors map[string]int
}

// keywordFetchLimit caps how many followee-ranked documents a keyword search
// pulls from Meilisearch in one go (a user has one document per profile).
const keywordFetchLimit = 1000

// viewerWindow validates the viewer-mode request. The ranking is decided
// here from the follow graph, so a sort makes no sense with it and is
// rejected rather than silently ignored. A text query is allowed: it narrows
// the ranked keys to the matches and appends the other matches after them.
func viewerWindow(c echo.Context, viewer string) (limit int, offset int, err error) {
	if !concrnt.IsCCID(viewer) {
		return 0, 0, echo.NewHTTPError(http.StatusBadRequest, "viewer must be a CCID")
	}
	if c.QueryParam("sort") != "" {
		return 0, 0, echo.NewHTTPError(http.StatusBadRequest, "viewer mode does not accept sort")
	}
	limit = parseInt(c.QueryParam("limit"), 20)
	if limit < 1 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	offset = parseInt(c.QueryParam("offset"), 0)
	if offset < 0 {
		offset = 0
	}
	return limit, offset, nil
}

// followees is the subquery of CCIDs the viewer currently acks with one of
// the indexed ack schemas. It stays a subquery so a viewer with thousands of
// follows never turns into thousands of bind parameters or a huge filter.
func (h *Handler) followees(viewer string) *gorm.DB {
	return h.db.Model(&model.Ack{}).Select("ackee").Where("acker = ? AND schema IN ? AND valid = ?", viewer, h.ackSchemas, true)
}

// rankFollowees turns the entries into per-key ranks, sorted by score then
// key, and returns the page plus the total.
func rankFollowees(entries []followeeEntry, now time.Time, halfLife time.Duration, limit int, offset int) ([]followeeRank, int) {
	ranks := map[string]*followeeRank{}
	for _, entry := range entries {
		rank, ok := ranks[entry.Key]
		if !ok {
			rank = &followeeRank{Key: entry.Key, Authors: map[string]int{}}
			ranks[entry.Key] = rank
		}
		// a future-dated entry counts as brand new (same as the activity tick)
		age := max(now.Sub(entry.CreatedAt), 0)
		rank.Score += math.Exp2(-age.Seconds() / halfLife.Seconds())
		rank.Count++
		rank.Authors[entry.Author]++
	}
	sorted := make([]followeeRank, 0, len(ranks))
	for _, rank := range ranks {
		sorted = append(sorted, *rank)
	}
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Score != sorted[j].Score {
			return sorted[i].Score > sorted[j].Score
		}
		return sorted[i].Key < sorted[j].Key
	})
	total := len(sorted)
	if offset >= total {
		return nil, total
	}
	return sorted[offset:min(offset+limit, total)], total
}

type followeeEntry struct {
	Key       string
	Author    string
	CreatedAt time.Time
}

// topAuthors lists the followees with the most entries, ties by CCID.
func topAuthors(counts map[string]int) []string {
	authors := make([]string, 0, len(counts))
	for author := range counts {
		authors = append(authors, author)
	}
	sort.Slice(authors, func(i, j int) bool {
		if counts[authors[i]] != counts[authors[j]] {
			return counts[authors[i]] > counts[authors[j]]
		}
		return authors[i] < authors[j]
	})
	return authors[:min(topAuthorsLimit, len(authors))]
}

func (h *Handler) viewerResponse(c echo.Context, hits []map[string]any, query string, limit int, offset int, total int, started time.Time) error {
	if hits == nil {
		hits = []map[string]any{}
	}
	return c.JSON(http.StatusOK, map[string]any{
		"hits":               hits,
		"query":              query,
		"limit":              limit,
		"offset":             offset,
		"estimatedTotalHits": total,
		"processingTimeMs":   time.Since(started).Milliseconds(),
	})
}

// keywordSearch is the viewer mode with a text query: the keys the
// followees are active in are searched first and ordered by their rank, then
// the remaining matches follow in relevance order, so a sort by followee
// activity still lists every match. The page is cut across the two lists and
// the total is the ranked matches plus Meilisearch's estimate of the rest.
func (h *Handler) keywordSearch(c echo.Context, indexUID string, field string, query string, ranks []followeeRank, limit int, offset int, annotate func(doc map[string]any, rank followeeRank)) ([]map[string]any, int, error) {
	ctx := c.Request().Context()
	keys := make([]string, len(ranks))
	byKey := make(map[string]int, len(ranks))
	for i, rank := range ranks {
		keys[i] = rank.Key
		byKey[rank.Key] = i
	}
	var matched []map[string]any
	if len(ranks) > 0 {
		resp, err := h.store.Search(ctx, indexUID, query, keywordFetchLimit, 0, meili.InFilter(field, keys), nil)
		if err != nil {
			return nil, 0, err
		}
		for _, hit := range resp.Hits {
			doc, ok := hit.(map[string]any)
			if !ok {
				continue
			}
			key, _ := doc[field].(string)
			if _, ok := byKey[key]; !ok {
				continue
			}
			matched = append(matched, doc)
		}
		// ranks are already sorted by score then key; a stable sort keeps the
		// relevance order between documents of the same key (a user's profiles)
		sort.SliceStable(matched, func(i, j int) bool {
			ki, _ := matched[i][field].(string)
			kj, _ := matched[j][field].(string)
			return byKey[ki] < byKey[kj]
		})
		for _, doc := range matched {
			key, _ := doc[field].(string)
			annotate(doc, ranks[byKey[key]])
		}
	}
	page := matched[min(offset, len(matched)):min(offset+limit, len(matched))]
	restLimit := limit - len(page)
	restOffset := max(0, offset-len(matched))
	// NotInFilter of no keys is empty, so an unranked viewer gets plain relevance order
	resp, err := h.store.Search(ctx, indexUID, query, int64(restLimit), int64(restOffset), meili.NotInFilter(field, keys), nil)
	if err != nil {
		return nil, 0, err
	}
	hits := append([]map[string]any{}, page...)
	for _, hit := range resp.Hits {
		if doc, ok := hit.(map[string]any); ok {
			hits = append(hits, doc)
		}
	}
	return hits, len(matched) + int(resp.EstimatedTotalHits), nil
}

// followeeUsers ranks the viewer's followees by their own recent posts and
// returns one profile document per CCID (the main profile when it is
// indexed, else the first key) with followeeScore / followeePostCount30d.
func (h *Handler) followeeUsers(c echo.Context, viewer string) error {
	started := time.Now()
	limit, offset, err := viewerWindow(c, viewer)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	now := started.UTC()

	var rows []struct {
		Author    string
		CreatedAt time.Time
	}
	// only users that are in the index can be shown, so a followee without
	// an indexed profile does not take a slot on the page
	if err := h.db.WithContext(ctx).Model(&model.UserEntry{}).
		Select("author", "created_at").
		Where("created_at > ?", now.Add(-crawler.ActivityWindow)).
		Where("author IN (?)", h.followees(viewer)).
		Where("author IN (?)", h.db.Model(&model.IndexedUser{}).Select("ccid")).
		Find(&rows).Error; err != nil {
		return err
	}
	entries := make([]followeeEntry, len(rows))
	for i, row := range rows {
		entries[i] = followeeEntry{Key: row.Author, Author: row.Author, CreatedAt: row.CreatedAt}
	}
	if query := c.QueryParam("q"); query != "" {
		// every matching profile document is a hit here (a subprofile can be what matched)
		ranks, _ := rankFollowees(entries, now, h.halfLife, len(entries), 0)
		hits, total, err := h.keywordSearch(c, meili.UsersIndex, "ccid", query, ranks, limit, offset, func(doc map[string]any, rank followeeRank) {
			doc["followeeScore"] = rank.Score
			doc["followeePostCount30d"] = rank.Count
		})
		if err != nil {
			return err
		}
		return h.viewerResponse(c, hits, query, limit, offset, total, started)
	}
	page, total := rankFollowees(entries, now, h.halfLife, limit, offset)
	if len(page) == 0 {
		return h.viewerResponse(c, nil, "", limit, offset, total, started)
	}

	ccids := make([]string, len(page))
	for i, rank := range page {
		ccids[i] = rank.Key
	}
	// a CCID has one document per profile (main and subprofiles); the page
	// is at most 100 CCIDs, so the fetch cap is generous
	docs, err := h.store.FetchDocuments(ctx, meili.UsersIndex, meili.InFilter("ccid", ccids), 1000)
	if err != nil {
		return err
	}
	byCCID := map[string]map[string]any{}
	for _, doc := range docs {
		ccid, _ := doc["ccid"].(string)
		cckv, _ := doc["cckv"].(string)
		held, ok := byCCID[ccid]
		if !ok {
			byCCID[ccid] = doc
			continue
		}
		heldKey, _ := held["cckv"].(string)
		if strings.HasSuffix(heldKey, "/main") {
			continue
		}
		if strings.HasSuffix(cckv, "/main") || cckv < heldKey {
			byCCID[ccid] = doc
		}
	}
	hits := make([]map[string]any, 0, len(page))
	for _, rank := range page {
		doc, ok := byCCID[rank.Key]
		if !ok {
			continue
		}
		doc["followeeScore"] = rank.Score
		doc["followeePostCount30d"] = rank.Count
		hits = append(hits, doc)
	}
	return h.viewerResponse(c, hits, "", limit, offset, total, started)
}

// followeeCommunities ranks the indexed communities by the recent entries
// the viewer's followees left there, naming the most active of them in
// topAuthors.
func (h *Handler) followeeCommunities(c echo.Context, viewer string) error {
	started := time.Now()
	limit, offset, err := viewerWindow(c, viewer)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	now := started.UTC()

	var rows []struct {
		CommunityCCKV string
		Author        string
		CreatedAt     time.Time
	}
	if err := h.db.WithContext(ctx).Model(&model.CommunityEntry{}).
		Select("community_cckv", "author", "created_at").
		Where("created_at > ?", now.Add(-crawler.ActivityWindow)).
		Where("author IN (?)", h.followees(viewer)).
		Where("community_cckv IN (?)", h.db.Model(&model.IndexedCommunity{}).Select("cckv")).
		Find(&rows).Error; err != nil {
		return err
	}
	entries := make([]followeeEntry, len(rows))
	for i, row := range rows {
		entries[i] = followeeEntry{Key: row.CommunityCCKV, Author: row.Author, CreatedAt: row.CreatedAt}
	}
	if query := c.QueryParam("q"); query != "" {
		ranks, _ := rankFollowees(entries, now, h.halfLife, len(entries), 0)
		hits, total, err := h.keywordSearch(c, meili.CommunitiesIndex, "cckv", query, ranks, limit, offset, func(doc map[string]any, rank followeeRank) {
			doc["followeeScore"] = rank.Score
			doc["followeePostCount30d"] = rank.Count
			doc["topAuthors"] = topAuthors(rank.Authors)
		})
		if err != nil {
			return err
		}
		return h.viewerResponse(c, hits, query, limit, offset, total, started)
	}
	page, total := rankFollowees(entries, now, h.halfLife, limit, offset)
	if len(page) == 0 {
		return h.viewerResponse(c, nil, "", limit, offset, total, started)
	}

	keys := make([]string, len(page))
	for i, rank := range page {
		keys[i] = rank.Key
	}
	docs, err := h.store.FetchDocuments(ctx, meili.CommunitiesIndex, meili.InFilter("cckv", keys), int64(len(keys)))
	if err != nil {
		return err
	}
	byKey := map[string]map[string]any{}
	for _, doc := range docs {
		cckv, _ := doc["cckv"].(string)
		byKey[cckv] = doc
	}
	hits := make([]map[string]any, 0, len(page))
	for _, rank := range page {
		doc, ok := byKey[rank.Key]
		if !ok {
			continue
		}
		doc["followeeScore"] = rank.Score
		doc["followeePostCount30d"] = rank.Count
		doc["topAuthors"] = topAuthors(rank.Authors)
		hits = append(hits, doc)
	}
	return h.viewerResponse(c, hits, "", limit, offset, total, started)
}
