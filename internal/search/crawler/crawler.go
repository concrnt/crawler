package crawler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strconv"
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
	KindPost      = "post"
)

const replicationEndpoint = "net.concrnt.core.replication"

// referenceSchema is the schema of the reference records a distribution
// (CIP-7 §4.1) leaves under its destination.
const referenceSchema = "https://schema.concrnt.net/reference.json"

type Store interface {
	UpsertServers(ctx context.Context, docs []meili.ServerDocument) error
	UpsertUsers(ctx context.Context, docs []normalize.UserDocument) error
	UpsertCommunities(ctx context.Context, docs []normalize.CommunityDocument) error
	UpsertPosts(ctx context.Context, docs []normalize.PostDocument) error
	DeleteRecords(ctx context.Context, indexUID string, spec meili.DeleteSpec) error
	UpdateCommunityActivity(ctx context.Context, docs []meili.CommunityActivityDocument) error
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
	go func() {
		// populate server_states before the first crawl; otherwise the first
		// crawl finds no servers and nothing happens until the next tick
		c.runDiscovery(ctx)
		go c.discoveryLoop(ctx)
		go c.activityLoop(ctx)
		c.crawlLoop(ctx)
	}()
}

func (c *Crawler) discoveryLoop(ctx context.Context) {
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
		discoveryRuns.WithLabelValues(resultError).Inc()
		return
	}
	discoveryRuns.WithLabelValues(resultOK).Inc()
}

func (c *Crawler) runCrawl(ctx context.Context) {
	started := time.Now()
	err := c.CrawlOnce(ctx)
	crawlRunDuration.Observe(time.Since(started).Seconds())
	if err != nil {
		c.logger.Warn("crawl failed", slog.String("error", err.Error()), slog.String("elapsed", time.Since(started).Round(time.Millisecond).String()))
		crawlRuns.WithLabelValues(resultError).Inc()
		return
	}
	c.logger.Info("crawl completed", slog.String("elapsed", time.Since(started).Round(time.Millisecond).String()))
	crawlRuns.WithLabelValues(resultOK).Inc()
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
		if err := c.mirrorCommunities(ctx, []string{community.CCKV}, time.Now().UTC()); err != nil {
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

	for _, schema := range c.cfg.PostSchemas {
		if doc.Schema != schema {
			continue
		}
		post, ok, err := normalize.NormalizePost(sd, schema, sourceServer, time.Now().UTC())
		if err != nil {
			return ManualCrawlResult{}, err
		}
		if !ok {
			return ManualCrawlResult{}, fmt.Errorf("post schema did not match")
		}
		if err := c.store.UpsertPosts(ctx, []normalize.PostDocument{post}); err != nil {
			return ManualCrawlResult{}, err
		}
		return ManualCrawlResult{
			Kind:         KindPost,
			Schema:       schema,
			ID:           post.ID,
			CCKV:         post.CCKV,
			CCFS:         post.CCFS,
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
		c.logger.Warn("no servers to crawl", slog.String("layer", c.cfg.Layer))
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
	if _, ok := wkc.Endpoints[replicationEndpoint]; !ok {
		c.logger.Warn("replication endpoint missing; skipping crawl", slog.String("server", wkc.Domain))
		return nil
	}

	cursor, err := c.getOrCreateReplicationCursor(ctx, state.Domain)
	if err != nil {
		return err
	}
	if ShouldBackoff(cursor.FailCount, cursor.LastErrorAt, now) {
		return nil
	}

	c.logger.Info("server crawl started", slog.String("server", state.Domain))
	var joined error
	// keep reading until the feed is drained: a run is capped at maxPagesPerRun
	// so progress lands in the cursor in slices, but waiting a whole tick
	// between slices would take days to get through a long log
	for {
		caughtUp, err := c.replicate(ctx, wkc, &cursor)
		if err != nil {
			var statusErr *replicationStatusError
			if errors.As(err, &statusErr) && statusErr.Transient() {
				// the server is asking us to slow down (CIP-16 §4); the cursor
				// already holds every page applied so far, so just try again
				// next tick without counting it as a failure
				c.logger.Info("replication paused by server", slog.String("server", state.Domain), slog.Int("status", statusErr.Status))
				break
			}
			c.markCursorFailure(ctx, state.Domain, err)
			joined = fmt.Errorf("%s replication: %w", state.Domain, err)
			break
		}
		if caughtUp || ctx.Err() != nil {
			break
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
	serverCrawlDuration.Observe(finished.Sub(now).Seconds())
	if joined != nil {
		c.logger.Warn("server crawl failed", slog.String("server", state.Domain), slog.String("elapsed", finished.Sub(now).Round(time.Millisecond).String()), slog.String("error", joined.Error()))
		serverCrawls.WithLabelValues(state.Domain, resultError).Inc()
	} else {
		c.logger.Info("server crawl completed", slog.String("server", state.Domain), slog.String("elapsed", finished.Sub(now).Round(time.Millisecond).String()))
		serverCrawls.WithLabelValues(state.Domain, resultOK).Inc()
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

func (c *Crawler) getOrCreateReplicationCursor(ctx context.Context, serverDomain string) (model.ReplicationCursor, error) {
	var cursor model.ReplicationCursor
	err := c.db.WithContext(ctx).Where("server_domain = ?", serverDomain).First(&cursor).Error
	if err == nil {
		return cursor, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return model.ReplicationCursor{}, err
	}

	cursor = model.ReplicationCursor{ServerDomain: serverDomain}
	if err := c.db.WithContext(ctx).Create(&cursor).Error; err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			err = c.db.WithContext(ctx).Where("server_domain = ?", serverDomain).First(&cursor).Error
		}
		if err != nil {
			return model.ReplicationCursor{}, err
		}
	}
	return cursor, nil
}

type replicationStatusError struct {
	Domain string
	Status int
}

func (e *replicationStatusError) Error() string {
	return fmt.Sprintf("replication %s failed: status %d", e.Domain, e.Status)
}

// Transient reports a rate-limit or overload response: the run stops but the
// server is not marked as failing.
func (e *replicationStatusError) Transient() bool {
	return e.Status == http.StatusTooManyRequests || e.Status == http.StatusServiceUnavailable
}

func (c *Crawler) fetchReplication(ctx context.Context, wkc concrnt.WellKnownConcrnt, params map[string]string) (concrnt.QueryResult, error) {
	tmpl, ok := wkc.Endpoints[replicationEndpoint]
	if !ok {
		return concrnt.QueryResult{}, fmt.Errorf("replication endpoint missing on %s", wkc.Domain)
	}
	path, err := concrnt.RenderURITemplate(tmpl, params)
	if err != nil {
		return concrnt.QueryResult{}, fmt.Errorf("render replication endpoint: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+wkc.Domain+path, nil)
	if err != nil {
		return concrnt.QueryResult{}, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.client.GetClient().Do(req)
	if err != nil {
		replicationRequests.WithLabelValues(wkc.Domain, resultError).Inc()
		return concrnt.QueryResult{}, fmt.Errorf("request replication: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		statusErr := &replicationStatusError{Domain: wkc.Domain, Status: resp.StatusCode}
		result := resultError
		if statusErr.Transient() {
			result = resultTransient
		}
		replicationRequests.WithLabelValues(wkc.Domain, result).Inc()
		return concrnt.QueryResult{}, statusErr
	}

	var out concrnt.QueryResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		replicationRequests.WithLabelValues(wkc.Domain, resultError).Inc()
		return concrnt.QueryResult{}, fmt.Errorf("decode replication: %w", err)
	}
	replicationRequests.WithLabelValues(wkc.Domain, resultOK).Inc()
	return out, nil
}

// replicate follows the server's commit log (CIP-16) forward from the stored
// cursor for up to maxPagesPerRun pages. It reports whether the feed was
// drained (next == null) in this run.
func (c *Crawler) replicate(ctx context.Context, wkc concrnt.WellKnownConcrnt, cursor *model.ReplicationCursor) (bool, error) {
	started := time.Now().UTC()
	if err := c.db.WithContext(ctx).Model(cursor).Updates(map[string]any{"last_started_at": started}).Error; err != nil {
		return false, err
	}

	// receipt times are stamped mid-commit, so a row with a smaller sort key
	// can surface after the cursor passed it: re-read a little behind the
	// cursor on the first page of a run (CIP-16 §3.3) and rely on idempotent
	// upserts for the rows seen twice
	var since *time.Time
	if cursor.CursorAt != nil {
		s := cursor.CursorAt.Add(-c.cfg.Overlap.Duration())
		since = &s
	}

	for page := 0; page < c.cfg.MaxPagesPerRun; page++ {
		params := map[string]string{
			"limit": strconv.Itoa(c.cfg.PageLimit),
			"order": "asc",
		}
		if since != nil {
			params["since"] = since.UTC().Format(time.RFC3339Nano)
		}
		result, err := c.fetchReplication(ctx, wkc, params)
		if err != nil {
			return false, err
		}
		if err := c.applyPage(ctx, wkc.Domain, result.Items); err != nil {
			return false, err
		}
		replicationPages.WithLabelValues(wkc.Domain).Inc()
		// cursors are computed before read-access filtering, so items may be
		// short or empty while next is still non-nil. next == nil is the only
		// end-of-feed signal.
		if result.Next == nil {
			// the feed is drained. The cursor only ever takes the server's own
			// sort key: prev is the receipt time of this page's first row, so
			// the next run re-reads at most this page. Stamping the crawler's
			// clock instead would skip commits whenever it runs ahead of the
			// server's.
			cursorAt := cursor.CursorAt
			if result.Prev != nil && (cursorAt == nil || result.Prev.After(*cursorAt)) {
				cursorAt = result.Prev
			}
			now := time.Now().UTC()
			cursor.CursorAt = cursorAt
			cursor.CaughtUpAt = &now
			return true, c.db.WithContext(ctx).Model(cursor).Updates(map[string]any{
				"cursor_at":        cursorAt,
				"caught_up_at":     now,
				"last_finished_at": now,
				"fail_count":       0,
				"last_error":       "",
				"last_error_at":    nil,
			}).Error
		}
		// since is inclusive and next is the sort key of the first row past
		// the window, so echo it back unmodified. When more than one page of
		// rows share the same receipt time the cursor cannot advance
		// (server-side limitation): step past that instant so the rest of the
		// log is still reached, giving up only the rows at that instant
		// (1µs: server timestamps are stored with microsecond precision).
		next := *result.Next
		if since != nil {
			if next.Before(*since) {
				return false, fmt.Errorf("replication cursor went backwards: since %s next %s", since.Format(time.RFC3339Nano), next.Format(time.RFC3339Nano))
			}
			if next.Equal(*since) {
				c.logger.Warn("replication pagination did not progress; skipping remaining rows at this receipt time", slog.String("server", wkc.Domain), slog.Time("next", next))
				next = next.Add(time.Microsecond)
			}
		}
		cursor.CursorAt = &next
		if err := c.db.WithContext(ctx).Model(cursor).Updates(map[string]any{
			"cursor_at":        next,
			"last_finished_at": time.Now().UTC(),
			"fail_count":       0,
			"last_error":       "",
			"last_error_at":    nil,
		}).Error; err != nil {
			return false, err
		}
		since = &next
	}

	c.logger.Info("replication run reached maxPagesPerRun", slog.String("server", wkc.Domain), slog.Int("maxPagesPerRun", c.cfg.MaxPagesPerRun))
	return false, nil
}

// applyPage applies one page of commits in log order. Upserts are batched per
// index and flushed before any delete so a record deleted later in the same
// page does not survive, and a key committed twice keeps its last document.
func (c *Crawler) applyPage(ctx context.Context, sourceServer string, items []concrnt.SignedDocument) error {
	indexedAt := time.Now().UTC()
	users := map[string]normalize.UserDocument{}
	communities := map[string]normalize.CommunityDocument{}
	posts := map[string]normalize.PostDocument{}
	entries := map[string]model.CommunityEntry{}
	flush := func() error {
		if len(users) > 0 {
			if err := c.store.UpsertUsers(ctx, slices.Collect(maps.Values(users))); err != nil {
				return err
			}
			clear(users)
		}
		if len(communities) > 0 {
			if err := c.store.UpsertCommunities(ctx, slices.Collect(maps.Values(communities))); err != nil {
				return err
			}
			keys := make([]string, 0, len(communities))
			for _, community := range communities {
				keys = append(keys, community.CCKV)
			}
			if err := c.mirrorCommunities(ctx, keys, indexedAt); err != nil {
				return err
			}
			clear(communities)
		}
		if len(posts) > 0 {
			if err := c.store.UpsertPosts(ctx, slices.Collect(maps.Values(posts))); err != nil {
				return err
			}
			clear(posts)
		}
		if len(entries) > 0 {
			// the mirror was updated above, so a community created earlier in
			// this page already counts its references
			if err := c.recordCommunityEntries(ctx, slices.Collect(maps.Values(entries))); err != nil {
				return err
			}
			clear(entries)
		}
		return nil
	}

	for _, sd := range items {
		var doc concrnt.Document[json.RawMessage]
		if err := json.Unmarshal([]byte(sd.Document), &doc); err != nil {
			c.logger.Warn("skipping malformed commit", slog.String("server", sourceServer), slog.String("error", err.Error()))
			replicationMalformed.WithLabelValues(sourceServer, "commit").Inc()
			continue
		}
		switch doc.Kind {
		case "record":
			// any record landing directly under a community key is activity
			// there, whatever its schema: a distribution (CIP-7) arrives as a
			// reference record keyed <community>/<cdid>. Whether the parent is
			// an indexed community is settled against the mirror at flush, so
			// within one page a community counts entries that preceded its
			// own creation commit.
			if parent, ok := communityParent(doc.Key); ok {
				entry := model.CommunityEntry{CommunityCCKV: parent, EntryCCKV: doc.Key, Author: doc.Author, CreatedAt: doc.CreatedAt}
				if doc.Schema == referenceSchema {
					var ref struct {
						Href string `json:"href"`
					}
					if err := json.Unmarshal(doc.Value, &ref); err == nil {
						entry.Href = ref.Href
					}
				}
				entries[doc.Key] = entry
				replicationCommits.WithLabelValues(sourceServer, "entry").Inc()
			}
			switch {
			case slices.Contains(c.cfg.ProfileSchemas, doc.Schema):
				user, ok, err := normalize.NormalizeUser(sd, doc.Schema, sourceServer, indexedAt)
				if err != nil {
					c.logger.Warn("skipping malformed profile", slog.String("server", sourceServer), slog.String("schema", doc.Schema), slog.String("error", err.Error()))
					replicationMalformed.WithLabelValues(sourceServer, "profile").Inc()
					continue
				}
				if ok {
					users[user.ID] = user
					replicationCommits.WithLabelValues(sourceServer, "user").Inc()
				} else {
					replicationCommits.WithLabelValues(sourceServer, "ignored").Inc()
				}
			case slices.Contains(c.cfg.CommunitySchemas, doc.Schema):
				community, ok, err := normalize.NormalizeCommunity(sd, doc.Schema, sourceServer, indexedAt)
				if err != nil {
					c.logger.Warn("skipping malformed community", slog.String("server", sourceServer), slog.String("schema", doc.Schema), slog.String("error", err.Error()))
					replicationMalformed.WithLabelValues(sourceServer, "community").Inc()
					continue
				}
				if ok {
					communities[community.ID] = community
					replicationCommits.WithLabelValues(sourceServer, "community").Inc()
				} else {
					replicationCommits.WithLabelValues(sourceServer, "ignored").Inc()
				}
			case slices.Contains(c.cfg.PostSchemas, doc.Schema):
				post, ok, err := normalize.NormalizePost(sd, doc.Schema, sourceServer, indexedAt)
				if err != nil {
					c.logger.Warn("skipping malformed post", slog.String("server", sourceServer), slog.String("schema", doc.Schema), slog.String("error", err.Error()))
					replicationMalformed.WithLabelValues(sourceServer, "post").Inc()
					continue
				}
				if ok {
					posts[post.ID] = post
					replicationCommits.WithLabelValues(sourceServer, "post").Inc()
				} else {
					replicationCommits.WithLabelValues(sourceServer, "ignored").Inc()
				}
			default:
				replicationCommits.WithLabelValues(sourceServer, "ignored").Inc()
			}
		case "delete":
			var target string
			if err := json.Unmarshal(doc.Value, &target); err != nil || target == "" {
				c.logger.Warn("skipping malformed delete", slog.String("server", sourceServer))
				replicationMalformed.WithLabelValues(sourceServer, "delete").Inc()
				continue
			}
			spec := meili.DeleteSpecForTarget(target)
			if spec.IsEmpty() {
				replicationCommits.WithLabelValues(sourceServer, "ignored").Inc()
				continue
			}
			replicationCommits.WithLabelValues(sourceServer, "delete").Inc()
			if err := flush(); err != nil {
				return err
			}
			for _, indexUID := range meili.RecordIndexes {
				if err := c.store.DeleteRecords(ctx, indexUID, spec); err != nil {
					return err
				}
			}
			if err := c.deleteCommunityEntries(ctx, target); err != nil {
				return err
			}
		default:
			replicationCommits.WithLabelValues(sourceServer, "ignored").Inc()
		}
	}
	return flush()
}

func (c *Crawler) markCursorFailure(ctx context.Context, serverDomain string, err error) {
	now := time.Now().UTC()
	if updateErr := c.db.WithContext(ctx).Model(&model.ReplicationCursor{}).Where("server_domain = ?", serverDomain).Updates(map[string]any{
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
