package meili

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/concrnt/concrnt-crawler/internal/search/normalize"
)

func TestEarliestCreatedAt(t *testing.T) {
	older := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := older.Add(24 * time.Hour)
	cases := []struct {
		name     string
		existing map[string]time.Time
		incoming time.Time
		want     time.Time
	}{
		{"not indexed yet", map[string]time.Time{}, newer, newer},
		{"edit keeps the indexed createdAt", map[string]time.Time{"a": older}, newer, older},
		{"backdated record moves createdAt back", map[string]time.Time{"a": newer}, older, older},
		{"indexed without createdAt", map[string]time.Time{"a": {}}, newer, newer},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := earliestCreatedAt(tc.existing, "a", tc.incoming); !got.Equal(tc.want) {
				t.Fatalf("got %s want %s", got, tc.want)
			}
		})
	}
}

// TestUpsertKeepsEarliestCreatedAt runs against a live Meilisearch named by
// CRAWLER_TEST_MEILI_HOST (and CRAWLER_TEST_MEILI_KEY); it writes into the
// real users and communities indexes under test-only ids and removes them.
func TestUpsertKeepsEarliestCreatedAt(t *testing.T) {
	host := os.Getenv("CRAWLER_TEST_MEILI_HOST")
	if host == "" {
		t.Skip("CRAWLER_TEST_MEILI_HOST not set")
	}
	ctx := context.Background()
	store := New(NewClient(host, os.Getenv("CRAWLER_TEST_MEILI_KEY")), 0, slog.Default())
	if err := store.EnsureIndexes(ctx); err != nil {
		t.Fatal(err)
	}
	older := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := older.Add(24 * time.Hour)
	userID := "test-created-at-user"
	communityID := "test-created-at-community"
	t.Cleanup(func() {
		_ = store.DeleteRecords(ctx, UsersIndex, DeleteSpec{IDs: []string{userID}})
		_ = store.DeleteRecords(ctx, CommunitiesIndex, DeleteSpec{IDs: []string{communityID}})
	})

	readUser := func() normalize.UserDocument {
		var doc normalize.UserDocument
		if err := store.client.Index(UsersIndex).GetDocumentWithContext(ctx, userID, nil, &doc); err != nil {
			t.Fatal(err)
		}
		return doc
	}
	readCommunity := func() normalize.CommunityDocument {
		var doc normalize.CommunityDocument
		if err := store.client.Index(CommunitiesIndex).GetDocumentWithContext(ctx, communityID, nil, &doc); err != nil {
			t.Fatal(err)
		}
		return doc
	}

	// first sighting stores the record's own createdAt
	if err := store.UpsertUsers(ctx, []normalize.UserDocument{{ID: userID, Username: "v1", CreatedAt: older, IndexedAt: older}}); err != nil {
		t.Fatal(err)
	}
	if got := readUser(); !got.CreatedAt.Equal(older) || got.Username != "v1" {
		t.Fatalf("first upsert: got %+v", got)
	}
	// an edit (newer createdAt) updates the profile but not createdAt
	if err := store.UpsertUsers(ctx, []normalize.UserDocument{{ID: userID, Username: "v2", CreatedAt: newer, IndexedAt: newer}}); err != nil {
		t.Fatal(err)
	}
	if got := readUser(); !got.CreatedAt.Equal(older) || got.Username != "v2" || !got.IndexedAt.Equal(newer) {
		t.Fatalf("edit: got %+v", got)
	}
	// a backdated record moves createdAt back
	if err := store.UpsertUsers(ctx, []normalize.UserDocument{{ID: userID, Username: "v3", CreatedAt: older.Add(-time.Hour), IndexedAt: newer}}); err != nil {
		t.Fatal(err)
	}
	if got := readUser(); !got.CreatedAt.Equal(older.Add(-time.Hour)) || got.Username != "v3" {
		t.Fatalf("backdate: got %+v", got)
	}

	if err := store.UpsertCommunities(ctx, []normalize.CommunityDocument{{ID: communityID, Name: "v1", CreatedAt: older, IndexedAt: older}}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertCommunities(ctx, []normalize.CommunityDocument{{ID: communityID, Name: "v2", CreatedAt: newer, IndexedAt: newer}}); err != nil {
		t.Fatal(err)
	}
	if got := readCommunity(); !got.CreatedAt.Equal(older) || got.Name != "v2" {
		t.Fatalf("community edit: got %+v", got)
	}
}
