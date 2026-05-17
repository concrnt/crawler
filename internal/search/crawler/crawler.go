package crawler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/concrnt/concrnt"
	"github.com/concrnt/concrnt-crawler/internal/search/config"
	"github.com/concrnt/concrnt-crawler/internal/search/meili"
	"github.com/concrnt/concrnt-crawler/internal/search/model"
	"github.com/concrnt/concrnt-crawler/internal/search/normalize"
	"github.com/concrnt/concrnt/client"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	KindProfile   = "profile"
	KindCommunity = "community"
)

type Store interface {
	UpsertServers(ctx context.Context, docs []meili.ServerDocument) error
	UpsertUsers(ctx context.Context, docs []normalize.UserDocument) error
	UpsertCommunities(ctx context.Context, docs []normalize.CommunityDocument) error
}

type Crawler struct {
	db     *gorm.DB
	store  Store
	client *client.Client
	cfg    config.Crawl
	logger *slog.Logger
}

type ManualCrawlResult struct {
	Kind         string `json:"kind"`
	Schema       string `json:"schema"`
	ID           string `json:"id"`
	CCKV         string `json:"cckv"`
	CCFS         string `json:"ccfs"`
	SourceServer string `json:"sourceServer"`
}

func New(db *gorm.DB, store Store, client *client.Client, cfg config.Crawl, logger *slog.Logger) *Crawler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Crawler{
		db:     db,
		store:  store,
		client: client,
		cfg:    cfg,
		logger: logger,
	}
}

func (c *Crawler) Start(ctx context.Context) {
	go c.discoveryLoop(ctx)
	go c.crawlLoop(ctx)
}

func (c *Crawler) discoveryLoop(ctx context.Context) {
	c.runDiscovery(ctx)
	ticker := time.NewTicker(c.cfg.KnownServersInterval.Duration())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.runDiscovery(ctx)
		}
	}
}

func (c *Crawler) crawlLoop(ctx context.Context) {
	c.runCrawl(ctx)
	ticker := time.NewTicker(c.cfg.IncrementalInterval.Duration())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.runCrawl(ctx)
		}
	}
}

func (c *Crawler) runDiscovery(ctx context.Context) {
	if err := c.DiscoverOnce(ctx); err != nil {
		c.logger.Warn("server discovery failed", slog.String("error", err.Error()))
	}
}

func (c *Crawler) runCrawl(ctx context.Context) {
	if err := c.CrawlOnce(ctx); err != nil {
		c.logger.Warn("crawl failed", slog.String("error", err.Error()))
	}
}

func (c *Crawler) DiscoverOnce(ctx context.Context) error {
	now := time.Now().UTC()
	seed, err := c.client.GetServer(ctx, c.cfg.Seed, nil)
	if err != nil {
		return fmt.Errorf("get seed server: %w", err)
	}

	servers, err := c.fetchKnownServers(ctx, seed)
	if err != nil {
		return err
	}
	servers = append(servers, seed)
	servers = dedupeServers(servers)

	serverDocs := make([]meili.ServerDocument, 0, len(servers))
	skippedByLayer := 0
	for _, wkc := range servers {
		if wkc.Domain == "" {
			c.logger.Warn("skipping known server with empty domain", slog.String("csid", wkc.CSID))
			continue
		}
		if !c.matchesLayer(wkc) {
			skippedByLayer++
			continue
		}
		state, err := c.upsertServerState(ctx, wkc, now)
		if err != nil {
			return err
		}
		serverDocs = append(serverDocs, meili.ServerDocFromWellKnown(wkc, state.LastSeenAt, state.LastCrawledAt, state.Disabled))
	}
	if err := c.store.UpsertServers(ctx, serverDocs); err != nil {
		return fmt.Errorf("upsert server index: %w", err)
	}
	c.logger.Info("server discovery completed", slog.Int("servers", len(serverDocs)), slog.Int("skippedByLayer", skippedByLayer), slog.String("layer", c.cfg.Layer))
	return nil
}

func (c *Crawler) CrawlCCFS(ctx context.Context, ccfs string) (ManualCrawlResult, error) {
	parsed, err := concrnt.ParseCCURI(ccfs)
	if err != nil {
		return ManualCrawlResult{}, fmt.Errorf("invalid ccfs: %w", err)
	}
	if parsed.Scheme != "ccfs" {
		return ManualCrawlResult{}, fmt.Errorf("uri must use ccfs scheme")
	}

	sourceServer, err := c.client.ResolveResourceHost(ctx, ccfs)
	if err != nil {
		c.logger.Warn("failed to resolve ccfs source host", slog.String("ccfs", ccfs), slog.String("error", err.Error()))
		sourceServer = parsed.Owner
	}
	if err := c.ensureSourceLayer(ctx, sourceServer); err != nil {
		return ManualCrawlResult{}, err
	}

	var sd concrnt.SignedDocument
	if err := c.client.GetResource(ctx, ccfs, "application/json", nil, &sd); err != nil {
		return ManualCrawlResult{}, fmt.Errorf("fetch ccfs: %w", err)
	}
	if sd.CCFS == nil {
		sd.CCFS = &ccfs
	}

	var doc concrnt.Document[json.RawMessage]
	if err := json.Unmarshal([]byte(sd.Document), &doc); err != nil {
		return ManualCrawlResult{}, fmt.Errorf("decode signed document: %w", err)
	}

	for _, schema := range c.cfg.ProfileSchemas {
		if doc.Schema != schema {
			continue
		}
		user, ok, err := normalize.NormalizeUser(sd, schema, sourceServer, time.Now().UTC())
		if err != nil {
			return ManualCrawlResult{}, err
		}
		if !ok {
			return ManualCrawlResult{}, fmt.Errorf("profile schema did not match")
		}
		if err := c.store.UpsertUsers(ctx, []normalize.UserDocument{user}); err != nil {
			return ManualCrawlResult{}, err
		}
		return ManualCrawlResult{
			Kind:         KindProfile,
			Schema:       schema,
			ID:           user.ID,
			CCKV:         user.CCKV,
			CCFS:         user.CCFS,
			SourceServer: sourceServer,
		}, nil
	}

	for _, schema := range c.cfg.CommunitySchemas {
		if doc.Schema != schema {
			continue
		}
		community, ok, err := normalize.NormalizeCommunity(sd, schema, sourceServer, time.Now().UTC())
		if err != nil {
			return ManualCrawlResult{}, err
		}
		if !ok {
			return ManualCrawlResult{}, fmt.Errorf("community schema did not match")
		}
		if err := c.store.UpsertCommunities(ctx, []normalize.CommunityDocument{community}); err != nil {
			return ManualCrawlResult{}, err
		}
		return ManualCrawlResult{
			Kind:         KindCommunity,
			Schema:       schema,
			ID:           community.ID,
			CCKV:         community.CCKV,
			CCFS:         community.CCFS,
			SourceServer: sourceServer,
		}, nil
	}

	return ManualCrawlResult{}, fmt.Errorf("unsupported schema: %s", doc.Schema)
}

func (c *Crawler) fetchKnownServers(ctx context.Context, seed concrnt.WellKnownConcrnt) ([]concrnt.WellKnownConcrnt, error) {
	tmpl, ok := seed.Endpoints["net.concrnt.core.known-servers"]
	if !ok {
		return nil, fmt.Errorf("known-servers endpoint missing on %s", seed.Domain)
	}
	path, err := concrnt.RenderURITemplate(tmpl, map[string]string{})
	if err != nil {
		return nil, fmt.Errorf("render known-servers endpoint: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+seed.Domain+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.client.GetClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("request known-servers: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("known-servers %s failed: status %d", seed.Domain, resp.StatusCode)
	}

	var out []concrnt.WellKnownConcrnt
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode known-servers: %w", err)
	}
	return out, nil
}

func dedupeServers(in []concrnt.WellKnownConcrnt) []concrnt.WellKnownConcrnt {
	seenDomains := map[string]bool{}
	seenCSIDs := map[string]bool{}
	out := make([]concrnt.WellKnownConcrnt, 0, len(in))
	for _, wkc := range in {
		if wkc.Domain == "" {
			continue
		}
		if seenDomains[wkc.Domain] {
			continue
		}
		if wkc.CSID != "" && seenCSIDs[wkc.CSID] {
			continue
		}
		seenDomains[wkc.Domain] = true
		if wkc.CSID != "" {
			seenCSIDs[wkc.CSID] = true
		}
		out = append(out, wkc)
	}
	return out
}

func (c *Crawler) upsertServerState(ctx context.Context, wkc concrnt.WellKnownConcrnt, seenAt time.Time) (model.ServerState, error) {
	raw, err := json.Marshal(wkc)
	if err != nil {
		return model.ServerState{}, err
	}
	state := model.ServerState{
		Domain:        wkc.Domain,
		CSID:          wkc.CSID,
		Layer:         wkc.Layer,
		Version:       wkc.Version,
		WellKnownJSON: model.JSONB(string(raw)),
		FirstSeenAt:   seenAt,
		LastSeenAt:    seenAt,
	}
	err = c.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "domain"}},
		DoUpdates: clause.Assignments(map[string]any{
			"cs_id":           wkc.CSID,
			"layer":           wkc.Layer,
			"version":         wkc.Version,
			"well_known_json": model.JSONB(string(raw)),
			"last_seen_at":    seenAt,
		}),
	}).Create(&state).Error
	if err != nil {
		return model.ServerState{}, err
	}
	if err := c.db.WithContext(ctx).First(&state, "domain = ?", wkc.Domain).Error; err != nil {
		return model.ServerState{}, err
	}
	return state, nil
}

func (c *Crawler) CrawlOnce(ctx context.Context) error {
	var servers []model.ServerState
	query := c.db.WithContext(ctx).Where("disabled = ?", false)
	if c.cfg.Layer != "" {
		query = query.Where("layer = ?", c.cfg.Layer)
	}
	if err := query.Order("domain asc").Find(&servers).Error; err != nil {
		return err
	}
	if len(servers) == 0 {
		return nil
	}

	workers := c.cfg.GlobalConcurrency
	if workers <= 0 {
		workers = 1
	}
	jobs := make(chan model.ServerState)
	errs := make(chan error, len(servers))
	var wg sync.WaitGroup

	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for state := range jobs {
				if err := c.crawlServer(ctx, state); err != nil {
					errs <- err
				}
			}
		}()
	}

	for _, state := range servers {
		select {
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return ctx.Err()
		case jobs <- state:
		}
	}
	close(jobs)
	wg.Wait()
	close(errs)

	var joined error
	for err := range errs {
		joined = errors.Join(joined, err)
	}
	return joined
}

func (c *Crawler) crawlServer(ctx context.Context, state model.ServerState) error {
	now := time.Now().UTC()
	if ShouldBackoff(state.FailCount, state.LastErrorAt, now) {
		return nil
	}

	wkc, err := c.client.GetServer(ctx, state.Domain, nil)
	if err != nil {
		c.markServerFailure(ctx, state.Domain, err)
		return fmt.Errorf("refresh server %s: %w", state.Domain, err)
	}
	state, err = c.upsertServerState(ctx, wkc, now)
	if err != nil {
		return err
	}
	if !c.matchesLayer(wkc) {
		c.logger.Info("skipping server with unmatched layer", slog.String("server", state.Domain), slog.String("serverLayer", wkc.Layer), slog.String("targetLayer", c.cfg.Layer))
		return nil
	}
	if _, ok := wkc.Endpoints["net.concrnt.core.query"]; !ok {
		c.logger.Warn("query endpoint missing; skipping crawl", slog.String("server", wkc.Domain))
		return nil
	}

	var joined error
	for _, schema := range c.cfg.ProfileSchemas {
		if err := c.crawlScope(ctx, state.Domain, KindProfile, schema); err != nil {
			joined = errors.Join(joined, err)
		}
	}
	for _, schema := range c.cfg.CommunitySchemas {
		if err := c.crawlScope(ctx, state.Domain, KindCommunity, schema); err != nil {
			joined = errors.Join(joined, err)
		}
	}

	finished := time.Now().UTC()
	updates := map[string]any{"last_crawled_at": finished}
	if joined == nil {
		updates["fail_count"] = 0
		updates["last_error"] = ""
		updates["last_error_at"] = nil
	} else {
		c.markServerFailure(ctx, state.Domain, joined)
	}
	if err := c.db.WithContext(ctx).Model(&model.ServerState{}).Where("domain = ?", state.Domain).Updates(updates).Error; err != nil {
		return err
	}
	return joined
}

func (c *Crawler) matchesLayer(wkc concrnt.WellKnownConcrnt) bool {
	return c.cfg.Layer == "" || wkc.Layer == c.cfg.Layer
}

func (c *Crawler) ensureSourceLayer(ctx context.Context, sourceServer string) error {
	if c.cfg.Layer == "" {
		return nil
	}
	wkc, err := c.client.GetServer(ctx, sourceServer, nil)
	if err != nil {
		return fmt.Errorf("get source server for layer check: %w", err)
	}
	if !c.matchesLayer(wkc) {
		return fmt.Errorf("source server layer mismatch: got %q want %q", wkc.Layer, c.cfg.Layer)
	}
	return nil
}

func (c *Crawler) crawlScope(ctx context.Context, serverDomain string, kind string, schema string) error {
	cursor, err := c.getOrCreateCursor(ctx, serverDomain, kind, schema)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if ShouldBackoff(cursor.FailCount, cursor.LastErrorAt, now) {
		return nil
	}

	if cursor.LastBackfillAt == nil {
		if err := c.runBackfill(ctx, &cursor); err != nil {
			c.markCursorFailure(ctx, cursor.ID, err)
			return fmt.Errorf("%s %s %s backfill: %w", serverDomain, kind, schema, err)
		}
		if cursor.LastBackfillAt == nil {
			return nil
		}
	}

	if err := c.runIncremental(ctx, &cursor); err != nil {
		c.markCursorFailure(ctx, cursor.ID, err)
		return fmt.Errorf("%s %s %s incremental: %w", serverDomain, kind, schema, err)
	}
	return nil
}

func (c *Crawler) getOrCreateCursor(ctx context.Context, serverDomain string, kind string, schema string) (model.CrawlCursor, error) {
	var cursor model.CrawlCursor
	err := c.db.WithContext(ctx).Where(
		"server_domain = ? AND kind = ? AND schema = ? AND prefix = ?",
		serverDomain,
		kind,
		schema,
		c.cfg.Prefix,
	).First(&cursor).Error
	if err == nil {
		return cursor, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return model.CrawlCursor{}, err
	}

	cursor = model.CrawlCursor{
		ServerDomain: serverDomain,
		Kind:         kind,
		Schema:       schema,
		Prefix:       c.cfg.Prefix,
	}
	if err := c.db.WithContext(ctx).Create(&cursor).Error; err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			err = c.db.WithContext(ctx).Where(
				"server_domain = ? AND kind = ? AND schema = ? AND prefix = ?",
				serverDomain,
				kind,
				schema,
				c.cfg.Prefix,
			).First(&cursor).Error
		}
		if err != nil {
			return model.CrawlCursor{}, err
		}
	}
	return cursor, nil
}

func (c *Crawler) runBackfill(ctx context.Context, cursor *model.CrawlCursor) error {
	started := time.Now().UTC()
	updates := map[string]any{"last_started_at": started}
	if cursor.BackfillUntil == nil {
		until := started
		cursor.BackfillUntil = &until
		updates["backfill_until"] = until
	}
	if err := c.db.WithContext(ctx).Model(cursor).Updates(updates).Error; err != nil {
		return err
	}

	var previousBoundary *time.Time
	for page := 0; page < c.cfg.MaxPagesPerRun; page++ {
		until := *cursor.BackfillUntil
		docs, err := c.client.Query(ctx, cursor.ServerDomain, client.QueryParams{
			Prefix: c.cfg.Prefix,
			Schema: cursor.Schema,
			Until:  &until,
			Limit:  c.cfg.PageLimit,
			Order:  "desc",
		})
		if err != nil {
			return err
		}
		if err := c.upsertPage(ctx, cursor.Kind, cursor.Schema, cursor.ServerDomain, docs); err != nil {
			return err
		}
		if len(docs) == 0 {
			return c.completeBackfill(ctx, cursor, started)
		}

		boundary, err := PageBoundary(docs, "desc")
		if err != nil {
			c.logger.Warn("backfill page has no usable createdAt", slog.String("server", cursor.ServerDomain), slog.String("schema", cursor.Schema), slog.String("error", err.Error()))
			if len(docs) < c.cfg.PageLimit {
				return c.completeBackfill(ctx, cursor, started)
			}
			return err
		}
		if previousBoundary != nil && !boundary.Before(*previousBoundary) {
			c.logger.Warn("backfill pagination did not progress", slog.String("server", cursor.ServerDomain), slog.String("schema", cursor.Schema), slog.Time("boundary", boundary))
			return c.markCursorSuccess(ctx, cursor)
		}

		if len(docs) < c.cfg.PageLimit {
			return c.completeBackfill(ctx, cursor, started)
		}

		nextUntil := boundary.Add(-time.Nanosecond)
		cursor.BackfillUntil = &nextUntil
		if err := c.db.WithContext(ctx).Model(cursor).Updates(map[string]any{
			"backfill_until":   nextUntil,
			"last_finished_at": time.Now().UTC(),
			"fail_count":       0,
			"last_error":       "",
			"last_error_at":    nil,
		}).Error; err != nil {
			return err
		}
		previousBoundary = &boundary
	}

	c.logger.Warn("backfill reached maxPagesPerRun", slog.String("server", cursor.ServerDomain), slog.String("schema", cursor.Schema), slog.Int("maxPagesPerRun", c.cfg.MaxPagesPerRun))
	return c.markCursorSuccess(ctx, cursor)
}

func (c *Crawler) completeBackfill(ctx context.Context, cursor *model.CrawlCursor, started time.Time) error {
	now := time.Now().UTC()
	incrementalSince := started.Add(-c.cfg.Overlap.Duration())
	cursor.LastBackfillAt = &now
	cursor.IncrementalSince = &incrementalSince
	cursor.BackfillUntil = nil
	return c.db.WithContext(ctx).Model(cursor).Updates(map[string]any{
		"last_backfill_at":  now,
		"incremental_since": incrementalSince,
		"backfill_until":    nil,
		"last_finished_at":  now,
		"fail_count":        0,
		"last_error":        "",
		"last_error_at":     nil,
	}).Error
}

func (c *Crawler) runIncremental(ctx context.Context, cursor *model.CrawlCursor) error {
	started := time.Now().UTC()
	if err := c.db.WithContext(ctx).Model(cursor).Updates(map[string]any{"last_started_at": started}).Error; err != nil {
		return err
	}

	since := started.Add(-c.cfg.Overlap.Duration())
	if cursor.IncrementalSince != nil {
		since = cursor.IncrementalSince.Add(-c.cfg.Overlap.Duration())
	}
	until := started

	for page := 0; page < c.cfg.MaxPagesPerRun; page++ {
		docs, err := c.client.Query(ctx, cursor.ServerDomain, client.QueryParams{
			Prefix: c.cfg.Prefix,
			Schema: cursor.Schema,
			Since:  &since,
			Until:  &until,
			Limit:  c.cfg.PageLimit,
			Order:  "asc",
		})
		if err != nil {
			return err
		}
		if err := c.upsertPage(ctx, cursor.Kind, cursor.Schema, cursor.ServerDomain, docs); err != nil {
			return err
		}
		if len(docs) < c.cfg.PageLimit {
			return c.completeIncremental(ctx, cursor, started)
		}

		boundary, err := PageBoundary(docs, "asc")
		if err != nil {
			c.logger.Warn("incremental page has no usable createdAt", slog.String("server", cursor.ServerDomain), slog.String("schema", cursor.Schema), slog.String("error", err.Error()))
			return err
		}
		nextSince := boundary.Add(time.Nanosecond)
		if !nextSince.After(since) {
			c.logger.Warn("incremental pagination did not progress", slog.String("server", cursor.ServerDomain), slog.String("schema", cursor.Schema), slog.Time("boundary", boundary))
			return c.markCursorSuccess(ctx, cursor)
		}
		since = nextSince
	}

	c.logger.Warn("incremental reached maxPagesPerRun", slog.String("server", cursor.ServerDomain), slog.String("schema", cursor.Schema), slog.Int("maxPagesPerRun", c.cfg.MaxPagesPerRun))
	return c.markCursorSuccess(ctx, cursor)
}

func (c *Crawler) completeIncremental(ctx context.Context, cursor *model.CrawlCursor, crawlStartedAt time.Time) error {
	now := time.Now().UTC()
	cursor.IncrementalSince = &crawlStartedAt
	cursor.LastIncrementalAt = &now
	return c.db.WithContext(ctx).Model(cursor).Updates(map[string]any{
		"incremental_since":   crawlStartedAt,
		"last_incremental_at": now,
		"last_finished_at":    now,
		"fail_count":          0,
		"last_error":          "",
		"last_error_at":       nil,
	}).Error
}

func (c *Crawler) markCursorSuccess(ctx context.Context, cursor *model.CrawlCursor) error {
	now := time.Now().UTC()
	return c.db.WithContext(ctx).Model(cursor).Updates(map[string]any{
		"last_finished_at": now,
		"fail_count":       0,
		"last_error":       "",
		"last_error_at":    nil,
	}).Error
}

func (c *Crawler) upsertPage(ctx context.Context, kind string, schema string, sourceServer string, docs []concrnt.SignedDocument) error {
	indexedAt := time.Now().UTC()
	switch kind {
	case KindProfile:
		out := make([]normalize.UserDocument, 0, len(docs))
		seen := map[string]bool{}
		for _, sd := range docs {
			doc, ok, err := normalize.NormalizeUser(sd, schema, sourceServer, indexedAt)
			if err != nil {
				c.logger.Warn("skipping malformed profile", slog.String("server", sourceServer), slog.String("schema", schema), slog.String("error", err.Error()))
				continue
			}
			if !ok || seen[doc.ID] {
				continue
			}
			seen[doc.ID] = true
			out = append(out, doc)
		}
		return c.store.UpsertUsers(ctx, out)
	case KindCommunity:
		out := make([]normalize.CommunityDocument, 0, len(docs))
		seen := map[string]bool{}
		for _, sd := range docs {
			doc, ok, err := normalize.NormalizeCommunity(sd, schema, sourceServer, indexedAt)
			if err != nil {
				c.logger.Warn("skipping malformed community", slog.String("server", sourceServer), slog.String("schema", schema), slog.String("error", err.Error()))
				continue
			}
			if !ok || seen[doc.ID] {
				continue
			}
			seen[doc.ID] = true
			out = append(out, doc)
		}
		return c.store.UpsertCommunities(ctx, out)
	default:
		return fmt.Errorf("unknown crawl kind: %s", kind)
	}
}

func PageBoundary(docs []concrnt.SignedDocument, order string) (time.Time, error) {
	var boundary time.Time
	for _, sd := range docs {
		createdAt, err := normalize.CreatedAt(sd)
		if err != nil || createdAt.IsZero() {
			continue
		}
		if boundary.IsZero() {
			boundary = createdAt
			continue
		}
		if order == "asc" && createdAt.After(boundary) {
			boundary = createdAt
		}
		if order == "desc" && createdAt.Before(boundary) {
			boundary = createdAt
		}
	}
	if boundary.IsZero() {
		return time.Time{}, fmt.Errorf("no createdAt found")
	}
	return boundary, nil
}

func (c *Crawler) markCursorFailure(ctx context.Context, cursorID uint, err error) {
	now := time.Now().UTC()
	if updateErr := c.db.WithContext(ctx).Model(&model.CrawlCursor{}).Where("id = ?", cursorID).Updates(map[string]any{
		"last_error_at": now,
		"last_error":    truncateError(err),
		"fail_count":    gorm.Expr("fail_count + 1"),
	}).Error; updateErr != nil {
		c.logger.Warn("failed to mark cursor failure", slog.String("error", updateErr.Error()))
	}
}

func (c *Crawler) markServerFailure(ctx context.Context, domain string, err error) {
	now := time.Now().UTC()
	if updateErr := c.db.WithContext(ctx).Model(&model.ServerState{}).Where("domain = ?", domain).Updates(map[string]any{
		"last_error_at": now,
		"last_error":    truncateError(err),
		"fail_count":    gorm.Expr("fail_count + 1"),
	}).Error; updateErr != nil {
		c.logger.Warn("failed to mark server failure", slog.String("server", domain), slog.String("error", updateErr.Error()))
	}
}

func BackoffDuration(failCount int) time.Duration {
	switch {
	case failCount <= 1:
		return 0
	case failCount == 2:
		return 5 * time.Minute
	case failCount == 3:
		return 15 * time.Minute
	default:
		return time.Hour
	}
}

func ShouldBackoff(failCount int, lastErrorAt *time.Time, now time.Time) bool {
	if lastErrorAt == nil {
		return false
	}
	duration := BackoffDuration(failCount)
	if duration <= 0 {
		return false
	}
	return now.Before(lastErrorAt.Add(duration))
}

func truncateError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if len(msg) > 2048 {
		return msg[:2048]
	}
	return msg
}
