package crawler

import (
	"context"
	"encoding/json"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/concrnt/concrnt"
	"github.com/concrnt/concrnt-crawler/internal/search/config"
	"github.com/concrnt/concrnt-crawler/internal/search/meili"
	"github.com/concrnt/concrnt-crawler/internal/search/model"
	"github.com/concrnt/concrnt-crawler/internal/search/normalize"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

const (
	general     = "cckv://example.com/concrnt.world/communities/general"
	otherAuthor = "con098765432109876543210987654321098765432"
)

func TestCommunityParent(t *testing.T) {
	cases := []struct {
		key    string
		parent string
		ok     bool
	}{
		{general + "/ref1", general, true},
		{"cckv://example.com/t", "cckv://example.com", true},
		{"cckv://example.com", "", false},
		{postsParent + "/p1", "", false}, // CCID-owned parent
		{"cckv://" + testAuthor, "", false},
		{"not a uri", "", false},
	}
	for _, tc := range cases {
		parent, ok := communityParent(tc.key)
		if ok != tc.ok || parent != tc.parent {
			t.Errorf("communityParent(%q) = %q,%v want %q,%v", tc.key, parent, ok, tc.parent, tc.ok)
		}
	}
}

func loadEntries(t *testing.T, c *Crawler) []model.CommunityEntry {
	t.Helper()
	var entries []model.CommunityEntry
	if err := c.db.Order("entry_cckv asc").Find(&entries).Error; err != nil {
		t.Fatal(err)
	}
	return entries
}

func loadMirror(t *testing.T, c *Crawler) []string {
	t.Helper()
	var keys []string
	if err := c.db.Model(&model.IndexedCommunity{}).Order("cckv asc").Pluck("cckv", &keys).Error; err != nil {
		t.Fatal(err)
	}
	return keys
}

func TestApplyPageRecordsEntriesUnderIndexedCommunities(t *testing.T) {
	c := newReplicationCrawler(t, &replicationServer{t: t}, &manualStore{}, config.Default().Crawl)
	ctx := context.Background()

	ref := func(key string) concrnt.SignedDocument {
		return commit(t, "record", key, referenceSchema, map[string]string{"href": postsParent + "/" + key[strings.LastIndex(key, "/")+1:]}, "")
	}
	page := []concrnt.SignedDocument{
		commit(t, "record", general, config.DefaultCommunitySchema, map[string]string{"name": "general"}, ""),
		ref(general + "/ref1"),
		ref(general + "/ref1"), // same key twice keeps one row
		ref("cckv://example.com/concrnt.world/communities/unknown/ref"), // parent never indexed
		ref(postsParent + "/ref"),    // CCID-owned parent
		ref(general + "/deeper/ref"), // grandchild, parent is not the community
		commit(t, "record", "cckv://"+testAuthor+"/t/mine", config.DefaultCommunitySchema, map[string]string{"name": "user-owned"}, ""),
		ref("cckv://" + testAuthor + "/t/mine/ref"),
		// a post committed directly under the community counts too, whatever its schema
		commit(t, "record", general+"/direct", config.DefaultPostSchemas[0], map[string]string{"body": "hi"}, ""),
	}
	resetCounters()
	if _, err := c.applyPage(ctx, replicationDomain, page); err != nil {
		t.Fatal(err)
	}

	if mirror := loadMirror(t, c); len(mirror) != 1 || mirror[0] != general {
		t.Fatalf("mirror should hold the domain-owned community only, got %v", mirror)
	}
	// every record keyed directly under a domain-owned parent is an entry
	// candidate: the community itself, ref1 twice, the unknown parent, the
	// grandchild, direct
	if got := testutil.ToFloat64(replicationCommits.WithLabelValues(replicationDomain, "entry")); got != 6 {
		t.Errorf("commits_total{kind=entry} = %v, want 6", got)
	}
	entries := loadEntries(t, c)
	if len(entries) != 2 || entries[0].EntryCCKV != general+"/direct" || entries[1].EntryCCKV != general+"/ref1" {
		t.Fatalf("unexpected entries: %+v", entries)
	}
	if entries[1].CommunityCCKV != general || entries[1].Author != testAuthor || !entries[1].CreatedAt.Equal(t0) || entries[1].Href != postsParent+"/ref1" {
		t.Fatalf("entry should carry the record's author, createdAt and href: %+v", entries[1])
	}
	if entries[0].Href != "" {
		t.Fatalf("a non-reference record has no href: %+v", entries[0])
	}

	// a reference arriving in a later page finds the community in the mirror
	if _, err := c.applyPage(ctx, replicationDomain, []concrnt.SignedDocument{ref(general + "/ref2")}); err != nil {
		t.Fatal(err)
	}
	if entries := loadEntries(t, c); len(entries) != 3 {
		t.Fatalf("expected 3 entries after the second page, got %+v", entries)
	}

	// a re-commit of ref1 (an edit: same key, later createdAt) refreshes the
	// row but not created_at, so the edit is not counted as new activity
	recommit := ref(general + "/ref1")
	var edited concrnt.Document[map[string]string]
	if err := json.Unmarshal([]byte(recommit.Document), &edited); err != nil {
		t.Fatal(err)
	}
	edited.CreatedAt = t0.Add(48 * time.Hour)
	edited.Author = otherAuthor
	raw, err := json.Marshal(edited)
	if err != nil {
		t.Fatal(err)
	}
	recommit.Document = string(raw)
	if _, err := c.applyPage(ctx, replicationDomain, []concrnt.SignedDocument{recommit}); err != nil {
		t.Fatal(err)
	}
	if entries := loadEntries(t, c); len(entries) != 3 || entries[1].EntryCCKV != general+"/ref1" || !entries[1].CreatedAt.Equal(t0) || entries[1].Author != otherAuthor {
		t.Fatalf("re-commit should keep created_at and refresh the rest, got %+v", entries)
	}

	// deleting the referenced post sweeps its reference (matched by href);
	// deleting a reference key directly works too
	if _, err := c.applyPage(ctx, replicationDomain, []concrnt.SignedDocument{commit(t, "delete", "", "", postsParent+"/ref1", "")}); err != nil {
		t.Fatal(err)
	}
	if entries := loadEntries(t, c); len(entries) != 2 || entries[0].EntryCCKV != general+"/direct" || entries[1].EntryCCKV != general+"/ref2" {
		t.Fatalf("post delete should sweep its reference, got %+v", entries)
	}
	if _, err := c.applyPage(ctx, replicationDomain, []concrnt.SignedDocument{ref(general + "/ref5"), commit(t, "delete", "", "", general+"/ref5", "")}); err != nil {
		t.Fatal(err)
	}
	if entries := loadEntries(t, c); len(entries) != 2 {
		t.Fatalf("exact delete of a reference key should remove it, got %+v", entries)
	}
	// a range delete over the author's posts sweeps by href as well
	if _, err := c.applyPage(ctx, replicationDomain, []concrnt.SignedDocument{commit(t, "delete", "", "", postsParent+"/*", "")}); err != nil {
		t.Fatal(err)
	}
	if entries := loadEntries(t, c); len(entries) != 1 || entries[0].EntryCCKV != general+"/direct" {
		t.Fatalf("range delete over posts should sweep their references only, got %+v", entries)
	}
	if _, err := c.applyPage(ctx, replicationDomain, []concrnt.SignedDocument{ref(general + "/ref2")}); err != nil {
		t.Fatal(err)
	}

	// subtree delete keeps the community itself but empties it
	if _, err := c.applyPage(ctx, replicationDomain, []concrnt.SignedDocument{commit(t, "delete", "", "", general+"/*", "")}); err != nil {
		t.Fatal(err)
	}
	if entries := loadEntries(t, c); len(entries) != 0 {
		t.Fatalf("subtree delete should remove every entry, got %+v", entries)
	}
	if mirror := loadMirror(t, c); len(mirror) != 1 {
		t.Fatalf("subtree delete must not drop the community, got %v", mirror)
	}

	// key-and-subtree delete drops the community; later references are ignored
	if _, err := c.applyPage(ctx, replicationDomain, []concrnt.SignedDocument{
		ref(general + "/ref3"),
		commit(t, "delete", "", "", general+"*", ""),
		ref(general + "/ref4"),
	}); err != nil {
		t.Fatal(err)
	}
	if mirror := loadMirror(t, c); len(mirror) != 0 {
		t.Fatalf("community should be gone, got %v", mirror)
	}
	if entries := loadEntries(t, c); len(entries) != 0 {
		t.Fatalf("no entries should survive the community delete, got %+v", entries)
	}
}

func TestRefreshCommunityActivity(t *testing.T) {
	store := &manualStore{}
	cfg := config.Default().Crawl
	c := newReplicationCrawler(t, &replicationServer{t: t}, store, cfg)
	ctx := context.Background()
	now := time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)
	day := 24 * time.Hour

	quiet := "cckv://example.com/concrnt.world/communities/quiet"
	stale := "cckv://example.com/concrnt.world/communities/stale"
	if err := c.mirrorCommunities(ctx, []string{general, quiet, stale}, now); err != nil {
		t.Fatal(err)
	}
	rows := []model.CommunityEntry{
		{CommunityCCKV: general, EntryCCKV: general + "/a", Author: testAuthor, CreatedAt: now.Add(-1 * day)},
		{CommunityCCKV: general, EntryCCKV: general + "/a2", Author: otherAuthor, CreatedAt: now.Add(-1*day + time.Hour)},
		{CommunityCCKV: general, EntryCCKV: general + "/b", Author: testAuthor, CreatedAt: now.Add(-7 * day)},
		{CommunityCCKV: general, EntryCCKV: general + "/c", Author: otherAuthor, CreatedAt: now.Add(-6 * day)},
		{CommunityCCKV: general, EntryCCKV: general + "/d", Author: otherAuthor, CreatedAt: now.Add(-14 * day)},
		{CommunityCCKV: general, EntryCCKV: general + "/e", Author: otherAuthor, CreatedAt: now.Add(-31 * day)},
		{CommunityCCKV: general, EntryCCKV: general + "/f", Author: testAuthor, CreatedAt: now.Add(time.Hour)},
		{CommunityCCKV: stale, EntryCCKV: stale + "/old", Author: testAuthor, CreatedAt: now.Add(-40 * day)},
		// not in the mirror: never pushed
		{CommunityCCKV: "cckv://example.com/concrnt.world/communities/gone", EntryCCKV: "cckv://example.com/concrnt.world/communities/gone/x", Author: testAuthor, CreatedAt: now},
	}
	if err := c.db.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}

	if err := c.RefreshCommunityActivity(ctx, now); err != nil {
		t.Fatal(err)
	}
	if len(store.activity) != 3 {
		t.Fatalf("expected one document per indexed community, got %+v", store.activity)
	}
	byID := map[string]int{}
	for i, doc := range store.activity {
		byID[doc.ID] = i
	}

	g := store.activity[byID[normalize.EncodeMeiliID(general)]]
	// a,a2(1d) b(7d) c(6d) d(14d) f(future=0) are within 30d; e(31d) is out
	if g.PostCount30d != 6 || g.PostCount7d != 5 || g.ActiveAuthors7d != 2 {
		t.Fatalf("unexpected counts: %+v", g)
	}
	wantScore := math.Exp2(-1.0/7) + math.Exp2(-23.0/(7*24)) + math.Exp2(-1) + math.Exp2(-6.0/7) + math.Exp2(-2) + 1
	if math.Abs(g.ActivityScore-wantScore) > 1e-9 {
		t.Fatalf("score = %v want %v", g.ActivityScore, wantScore)
	}
	// history: 30 UTC days ending today, zero-filled
	if len(g.ActivityHistory) != 30 || g.ActivityHistory[0].Date != "2026-08-24" || g.ActivityHistory[29].Date != "2026-09-22" {
		t.Fatalf("history should span 30 days ending today: %+v", g.ActivityHistory)
	}
	wantDays := map[int]meili.ActivityDay{
		28: {Date: "2026-09-21", Posts: 2, Authors: 2}, // a, a2
		22: {Date: "2026-09-15", Posts: 1, Authors: 1}, // b
		23: {Date: "2026-09-16", Posts: 1, Authors: 1}, // c
		15: {Date: "2026-09-08", Posts: 1, Authors: 1}, // d
		29: {Date: "2026-09-22", Posts: 1, Authors: 1}, // f (future lands on today)
	}
	for i, got := range g.ActivityHistory {
		want, ok := wantDays[i]
		if !ok {
			want = meili.ActivityDay{Date: got.Date}
		}
		if got != want {
			t.Fatalf("history[%d] = %+v want %+v", i, got, want)
		}
	}
	if g.LastPostAt == nil || !g.LastPostAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("lastPostAt should be the newest entry, got %v", g.LastPostAt)
	}

	q := store.activity[byID[normalize.EncodeMeiliID(quiet)]]
	if q.ActivityScore != 0 || q.PostCount30d != 0 || q.PostCount7d != 0 || q.ActiveAuthors7d != 0 || q.LastPostAt != nil {
		t.Fatalf("quiet community should be all zeros: %+v", q)
	}
	if len(q.ActivityHistory) != 30 || slices.ContainsFunc(q.ActivityHistory, func(d meili.ActivityDay) bool { return d.Posts != 0 || d.Authors != 0 }) {
		t.Fatalf("quiet community should have a zero-filled history: %+v", q.ActivityHistory)
	}

	s := store.activity[byID[normalize.EncodeMeiliID(stale)]]
	if s.PostCount30d != 0 || s.ActivityScore != 0 || s.LastPostAt == nil || !s.LastPostAt.Equal(now.Add(-40*day)) {
		t.Fatalf("stale community should keep lastPostAt beyond the window: %+v", s)
	}
	if slices.ContainsFunc(s.ActivityHistory, func(d meili.ActivityDay) bool { return d.Posts != 0 }) {
		t.Fatalf("an entry older than the history must not appear: %+v", s.ActivityHistory)
	}
	if _, ok := byID[normalize.EncodeMeiliID("cckv://example.com/concrnt.world/communities/gone")]; ok {
		t.Fatal("unindexed community must not be pushed")
	}
}
