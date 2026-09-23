package crawler

import (
	"context"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/concrnt/concrnt"
	"github.com/concrnt/concrnt-crawler/internal/search/config"
	"github.com/concrnt/concrnt-crawler/internal/search/meili"
	"github.com/concrnt/concrnt-crawler/internal/search/model"
	"github.com/concrnt/concrnt-crawler/internal/search/normalize"
)

const (
	mainProfile = "cckv://" + testAuthor + "/concrnt.world/profiles/main"
	subProfile  = "cckv://" + testAuthor + "/concrnt.world/profiles/work"
)

func loadUserEntries(t *testing.T, c *Crawler) []model.UserEntry {
	t.Helper()
	var entries []model.UserEntry
	if err := c.db.Order("entry_cckv asc").Find(&entries).Error; err != nil {
		t.Fatal(err)
	}
	return entries
}

func loadUserMirror(t *testing.T, c *Crawler) []model.IndexedUser {
	t.Helper()
	var rows []model.IndexedUser
	if err := c.db.Order("cckv asc").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestApplyPageRecordsUserEntriesAndMirror(t *testing.T) {
	c := newReplicationCrawler(t, &replicationServer{t: t}, &manualStore{}, config.Default().Crawl)
	ctx := context.Background()
	subPosts := subProfile + "/posts"
	apply := func(items ...concrnt.SignedDocument) {
		t.Helper()
		if err := c.applyPage(ctx, replicationDomain, items); err != nil {
			t.Fatal(err)
		}
	}

	apply(
		commit(t, "record", mainProfile, config.DefaultProfileSchema, map[string]string{"username": "alice"}, ""),
		commit(t, "record", subProfile, config.DefaultProfileSchema, map[string]string{"username": "alice at work"}, ""),
		commit(t, "record", postsParent+"/p1", config.DefaultPostSchemas[0], map[string]string{"body": "one"}, ""),
		commit(t, "record", subPosts+"/p2", config.DefaultPostSchemas[1], map[string]string{"body": "two"}, ""),
		// a post committed straight into a community is the author's activity too
		commit(t, "record", general+"/direct", config.DefaultPostSchemas[0], map[string]string{"body": "hi"}, ""),
		// a reference is not a post record
		commit(t, "record", general+"/ref1", referenceSchema, map[string]string{"href": postsParent + "/p1"}, ""),
		commit(t, "record", postsParent+"/list", "https://schema.concrnt.world/s/list.json", map[string]string{"name": "x"}, ""),
	)
	mirror := loadUserMirror(t, c)
	if len(mirror) != 2 || mirror[0].CCKV != mainProfile || mirror[1].CCKV != subProfile || mirror[0].CCID != testAuthor || mirror[1].CCID != testAuthor {
		t.Fatalf("both profiles should be mirrored under the same ccid, got %+v", mirror)
	}
	entries := loadUserEntries(t, c)
	if len(entries) != 3 || entries[0].EntryCCKV != postsParent+"/p1" || entries[1].EntryCCKV != subPosts+"/p2" || entries[2].EntryCCKV != general+"/direct" {
		t.Fatalf("unexpected user entries: %+v", entries)
	}
	if entries[0].Author != testAuthor || !entries[0].CreatedAt.Equal(t0) {
		t.Fatalf("entry should carry author and createdAt: %+v", entries[0])
	}

	// exact delete of a post, then a range delete over the sub profile's posts
	apply(commit(t, "delete", "", "", postsParent+"/p1", ""))
	if entries := loadUserEntries(t, c); len(entries) != 2 || entries[0].EntryCCKV != subPosts+"/p2" {
		t.Fatalf("exact delete should remove the entry, got %+v", entries)
	}
	apply(commit(t, "delete", "", "", subPosts+"/*", ""))
	if entries := loadUserEntries(t, c); len(entries) != 1 || entries[0].EntryCCKV != general+"/direct" {
		t.Fatalf("range delete should remove the sub profile's posts, got %+v", entries)
	}
	// deleting the sub profile key (and its subtree) drops it from the mirror
	apply(commit(t, "delete", "", "", subProfile+"*", ""))
	if mirror := loadUserMirror(t, c); len(mirror) != 1 || mirror[0].CCKV != mainProfile {
		t.Fatalf("profile delete should drop the mirror row, got %+v", mirror)
	}
}

func TestRefreshUserActivity(t *testing.T) {
	store := &manualStore{}
	c := newReplicationCrawler(t, &replicationServer{t: t}, store, config.Default().Crawl)
	ctx := context.Background()
	now := time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)
	day := 24 * time.Hour

	quietProfile := "cckv://" + otherAuthor + "/concrnt.world/profiles/main"
	if err := c.db.Create([]model.IndexedUser{
		{CCKV: mainProfile, CCID: testAuthor, IndexedAt: now},
		{CCKV: subProfile, CCID: testAuthor, IndexedAt: now},
		{CCKV: quietProfile, CCID: otherAuthor, IndexedAt: now},
	}).Error; err != nil {
		t.Fatal(err)
	}
	unindexed := "con111111111111111111111111111111111111111"
	rows := []model.UserEntry{
		{Author: testAuthor, EntryCCKV: postsParent + "/a", CreatedAt: now.Add(-1 * day)},
		{Author: testAuthor, EntryCCKV: postsParent + "/b", CreatedAt: now.Add(-7 * day)},
		{Author: testAuthor, EntryCCKV: postsParent + "/c", CreatedAt: now.Add(-14 * day)},
		{Author: testAuthor, EntryCCKV: postsParent + "/d", CreatedAt: now.Add(-31 * day)},
		{Author: testAuthor, EntryCCKV: postsParent + "/e", CreatedAt: now.Add(time.Hour)},
		// no mirror row: never pushed
		{Author: unindexed, EntryCCKV: "cckv://" + unindexed + "/p", CreatedAt: now},
	}
	if err := c.db.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}

	if err := c.RefreshUserActivity(ctx, now); err != nil {
		t.Fatal(err)
	}
	if len(store.userActivity) != 3 {
		t.Fatalf("expected one document per mirrored profile, got %+v", store.userActivity)
	}
	byID := map[string]meili.UserActivityDocument{}
	for _, doc := range store.userActivity {
		byID[doc.ID] = doc
	}

	main := byID[normalize.EncodeMeiliID(mainProfile)]
	// a(1d) b(7d) c(14d) e(future=0) are within 30d; d(31d) is out
	if main.PostCount30d != 4 || main.PostCount7d != 3 {
		t.Fatalf("unexpected counts: %+v", main)
	}
	wantScore := math.Exp2(-1.0/7) + math.Exp2(-1) + math.Exp2(-2) + 1
	if math.Abs(main.ActivityScore-wantScore) > 1e-9 {
		t.Fatalf("score = %v want %v", main.ActivityScore, wantScore)
	}
	if len(main.ActivityHistory) != 30 || main.ActivityHistory[0].Date != "2026-08-24" || main.ActivityHistory[29].Date != "2026-09-22" {
		t.Fatalf("history should span 30 days ending today: %+v", main.ActivityHistory)
	}
	wantDays := map[int]int{28: 1, 22: 1, 15: 1, 29: 1}
	for i, got := range main.ActivityHistory {
		if got.Posts != wantDays[i] {
			t.Fatalf("history[%d] = %+v want %d posts", i, got, wantDays[i])
		}
	}
	if main.LastPostAt == nil || !main.LastPostAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("lastPostAt should be the newest entry, got %v", main.LastPostAt)
	}

	// the sub profile carries the same activity as main
	sub := byID[normalize.EncodeMeiliID(subProfile)]
	sub.ID = main.ID
	if sub.PostCount30d != main.PostCount30d || sub.ActivityScore != main.ActivityScore || !slices.Equal(sub.ActivityHistory, main.ActivityHistory) {
		t.Fatalf("sub profile should carry the ccid's activity: %+v vs %+v", sub, main)
	}

	quiet := byID[normalize.EncodeMeiliID(quietProfile)]
	if quiet.ActivityScore != 0 || quiet.PostCount30d != 0 || quiet.LastPostAt != nil || slices.ContainsFunc(quiet.ActivityHistory, func(d meili.UserActivityDay) bool { return d.Posts != 0 }) {
		t.Fatalf("quiet user should be all zeros: %+v", quiet)
	}
	if _, ok := byID[normalize.EncodeMeiliID("cckv://"+unindexed+"/concrnt.world/profiles/main")]; ok {
		t.Fatal("unindexed user must not be pushed")
	}
}
