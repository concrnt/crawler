package crawler

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/concrnt/concrnt-crawler/internal/search/meili"
	"github.com/concrnt/concrnt-crawler/internal/search/model"
	"github.com/concrnt/concrnt-crawler/internal/search/normalize"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// activityWindow bounds the entries that feed the score and postCount30d;
// postCount7d and activeAuthors7d use the shorter window.
const (
	activityWindow      = 30 * 24 * time.Hour
	activityShortWindow = 7 * 24 * time.Hour
)

// communityParent returns the key one level above a record key when that
// parent is domain-owned, i.e. could be an indexed community. User-owned
// timelines (home, notification, activity) are CCID-owned and never qualify.
func communityParent(key string) (string, bool) {
	ancestors := normalize.Ancestors(key)
	if len(ancestors) == 0 {
		return "", false
	}
	parent := ancestors[len(ancestors)-1]
	owner := strings.TrimPrefix(parent, "cckv://")
	if i := strings.Index(owner, "/"); i >= 0 {
		owner = owner[:i]
	}
	if !normalize.IsDomainOwner(owner) {
		return "", false
	}
	return parent, true
}

func (c *Crawler) mirrorCommunities(ctx context.Context, keys []string, indexedAt time.Time) error {
	rows := make([]model.IndexedCommunity, 0, len(keys))
	for _, key := range keys {
		rows = append(rows, model.IndexedCommunity{CCKV: key, IndexedAt: indexedAt})
	}
	return c.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&rows).Error
}

// recordCommunityEntries keeps the entries whose parent is an indexed
// community and upserts them; a re-committed reference keeps its key (CIP-7
// §4.1) and simply refreshes its row.
func (c *Crawler) recordCommunityEntries(ctx context.Context, entries []model.CommunityEntry) error {
	parents := map[string]bool{}
	for _, entry := range entries {
		parents[entry.CommunityCCKV] = true
	}
	var known []string
	if err := c.db.WithContext(ctx).Model(&model.IndexedCommunity{}).Where("cckv IN ?", keysOf(parents)).Pluck("cckv", &known).Error; err != nil {
		return err
	}
	if len(known) == 0 {
		return nil
	}
	indexed := map[string]bool{}
	for _, key := range known {
		indexed[key] = true
	}
	rows := make([]model.CommunityEntry, 0, len(entries))
	for _, entry := range entries {
		if indexed[entry.CommunityCCKV] {
			rows = append(rows, entry)
		}
	}
	if len(rows) == 0 {
		return nil
	}
	return c.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "community_cckv"}, {Name: "entry_cckv"}},
		DoUpdates: clause.AssignmentColumns([]string{"author", "href", "created_at"}),
	}).Create(&rows).Error
}

// deleteCommunityEntries applies a delete commit target (CIP-4) to the mirror
// and the entries, with the same three shapes as meili.DeleteSpecForTarget:
// "key/*" is the subtree, "key*" the key and its subtree, anything else the
// single key. Entries sit under their community, so a subtree match on the
// entry key also covers every entry of a deleted community. The target is
// matched against href as well: the sweep of a deleted post's references is
// server-internal (CIP-4 §6.1) and never surfaces as a delete of the reference
// keys themselves.
func (c *Crawler) deleteCommunityEntries(ctx context.Context, target string) error {
	var exact, base string
	switch {
	case strings.HasSuffix(target, "/*"):
		base = strings.TrimSuffix(target, "/*")
	case strings.HasSuffix(target, "*"):
		base = strings.TrimSuffix(target, "*")
		exact = base
	case strings.HasPrefix(target, "cckv://"):
		exact = target
	default:
		return nil
	}
	return c.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if exact != "" {
			if err := tx.Where("cckv = ?", exact).Delete(&model.IndexedCommunity{}).Error; err != nil {
				return err
			}
			if err := tx.Where("entry_cckv = ? OR community_cckv = ? OR href = ?", exact, exact, exact).Delete(&model.CommunityEntry{}).Error; err != nil {
				return err
			}
		}
		if base != "" {
			prefix := base + "/"
			if err := tx.Where("substr(cckv, 1, ?) = ?", len(prefix), prefix).Delete(&model.IndexedCommunity{}).Error; err != nil {
				return err
			}
			if err := tx.Where("substr(entry_cckv, 1, ?) = ? OR substr(href, 1, ?) = ?", len(prefix), prefix, len(prefix), prefix).Delete(&model.CommunityEntry{}).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func (c *Crawler) activityLoop(ctx context.Context) {
	c.runActivityRefresh(ctx)
	ticker := time.NewTicker(c.cfg.ActivityInterval.Duration())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.runActivityRefresh(ctx)
		}
	}
}

func (c *Crawler) runActivityRefresh(ctx context.Context) {
	started := time.Now()
	err := c.RefreshCommunityActivity(ctx, started.UTC())
	activityRefreshDuration.Observe(time.Since(started).Seconds())
	if err != nil {
		c.logger.Warn("community activity refresh failed", slog.String("error", err.Error()), slog.String("elapsed", time.Since(started).Round(time.Millisecond).String()))
		activityRefreshes.WithLabelValues(resultError).Inc()
		return
	}
	activityRefreshes.WithLabelValues(resultOK).Inc()
}

// RefreshCommunityActivity recomputes the activity of every indexed community
// from its entries and merges the result into the communities index. The
// score is a sum over the last 30 days of 2^(-age/halfLife), so it decays on
// its own between refreshes only in the sense that each refresh re-ages the
// entries; the arithmetic runs in Go because the test database (sqlite) has no
// exp(). The same pass buckets the entries into a per-UTC-day history of
// activityHistoryDays days ending today.
func (c *Crawler) RefreshCommunityActivity(ctx context.Context, now time.Time) error {
	var communities []string
	if err := c.db.WithContext(ctx).Model(&model.IndexedCommunity{}).Order("cckv asc").Pluck("cckv", &communities).Error; err != nil {
		return err
	}
	if len(communities) == 0 {
		return nil
	}

	type recent struct {
		CommunityCCKV string
		Author        string
		CreatedAt     time.Time
	}
	historyDays := c.cfg.ActivityHistoryDays
	historyStart := now.UTC().Truncate(24*time.Hour).AddDate(0, 0, -(historyDays - 1))
	// the fetch covers whichever of the score window and the history is longer
	since := now.Add(-activityWindow)
	if historyStart.Before(since) {
		since = historyStart
	}
	var entries []recent
	if err := c.db.WithContext(ctx).Model(&model.CommunityEntry{}).
		Select("community_cckv", "author", "created_at").
		Where("created_at > ?", since).
		Find(&entries).Error; err != nil {
		return err
	}
	// newest entry per community, older than the window or not. Selecting
	// the column itself (not MAX()) keeps its declared type, which sqlite
	// needs to hand back a time
	var latests []model.CommunityEntry
	if err := c.db.WithContext(ctx).Model(&model.CommunityEntry{}).
		Select("community_cckv", "created_at").
		Where("created_at = (SELECT MAX(created_at) FROM community_entries AS newest WHERE newest.community_cckv = community_entries.community_cckv)").
		Find(&latests).Error; err != nil {
		return err
	}

	halfLife := c.cfg.ActivityHalfLife.Duration().Seconds()
	docs := make(map[string]*meili.CommunityActivityDocument, len(communities))
	for _, key := range communities {
		history := make([]meili.ActivityDay, historyDays)
		for i := range history {
			history[i].Date = historyStart.AddDate(0, 0, i).Format(time.DateOnly)
		}
		docs[key] = &meili.CommunityActivityDocument{ID: normalize.EncodeMeiliID(key), ActivityHistory: history}
	}
	authors := map[string]map[string]bool{}
	dayAuthors := map[string]map[int]map[string]bool{}
	for _, entry := range entries {
		doc, ok := docs[entry.CommunityCCKV]
		if !ok {
			continue
		}
		// a future-dated entry counts as brand new rather than inflating
		// beyond a weight of 1
		age := max(now.Sub(entry.CreatedAt), 0)
		if age <= activityWindow {
			doc.ActivityScore += math.Exp2(-age.Seconds() / halfLife)
			doc.PostCount30d++
		}
		if age <= activityShortWindow {
			doc.PostCount7d++
			if authors[entry.CommunityCCKV] == nil {
				authors[entry.CommunityCCKV] = map[string]bool{}
			}
			authors[entry.CommunityCCKV][entry.Author] = true
		}
		// future-dated entries land on today, matching age = 0 above
		day := min(int(entry.CreatedAt.UTC().Truncate(24*time.Hour).Sub(historyStart)/(24*time.Hour)), historyDays-1)
		if day >= 0 {
			doc.ActivityHistory[day].Posts++
			if dayAuthors[entry.CommunityCCKV] == nil {
				dayAuthors[entry.CommunityCCKV] = map[int]map[string]bool{}
			}
			if dayAuthors[entry.CommunityCCKV][day] == nil {
				dayAuthors[entry.CommunityCCKV][day] = map[string]bool{}
			}
			dayAuthors[entry.CommunityCCKV][day][entry.Author] = true
		}
	}
	for key, set := range authors {
		docs[key].ActiveAuthors7d = len(set)
	}
	for key, days := range dayAuthors {
		for day, set := range days {
			docs[key].ActivityHistory[day].Authors = len(set)
		}
	}
	for _, row := range latests {
		if doc, ok := docs[row.CommunityCCKV]; ok {
			at := row.CreatedAt.UTC()
			doc.LastPostAt = &at
		}
	}

	out := make([]meili.CommunityActivityDocument, 0, len(communities))
	for _, key := range communities {
		out = append(out, *docs[key])
	}
	if err := c.store.UpdateCommunityActivity(ctx, out); err != nil {
		return fmt.Errorf("update community activity: %w", err)
	}
	c.logger.Info("community activity refreshed", slog.Int("communities", len(out)), slog.Int("entries", len(entries)), slog.Int("historyDays", historyDays), slog.String("elapsed", time.Since(now).Round(time.Millisecond).String()))
	return nil
}

func keysOf(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	return out
}
