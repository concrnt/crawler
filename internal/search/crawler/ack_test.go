package crawler

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/concrnt/concrnt"
	"github.com/concrnt/concrnt-crawler/internal/search/config"
	"github.com/concrnt/concrnt-crawler/internal/search/model"
	"github.com/concrnt/concrnt-crawler/internal/search/sigtest"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// commitAck builds an ack-kind commit: no key, the target as a bare entity
// URI in associate (CIP-10 §3). ack/unack are signed by the acker; acked/
// unacked are derived from the signed ack/unack with the document-direct
// proof the ackee's server ships them with (CIP-10 §5.2).
func commitAck(t *testing.T, kind string, acker sigtest.Identity, target string, schema string, createdAt time.Time) concrnt.SignedDocument {
	t.Helper()
	original := kind
	switch kind {
	case "acked":
		original = "ack"
	case "unacked":
		original = "unack"
	}
	sd := acker.Sign(t, ackDocument(original, acker.CCID, target, schema, createdAt))
	if original != kind {
		sd = sigtest.Acked(t, sd)
	}
	ccfs := "ccfs://" + acker.CCID + "/concrnt/" + kind
	sd.CCFS = &ccfs
	return sd
}

func ackDocument(kind string, author string, target string, schema string, createdAt time.Time) concrnt.Document[map[string]string] {
	associate := "cckv://" + target
	return concrnt.Document[map[string]string]{
		Kind:      kind,
		Value:     map[string]string{},
		Author:    author,
		Associate: &associate,
		Schema:    schema,
		CreatedAt: createdAt,
	}
}

func loadAcks(t *testing.T, c *Crawler) []model.Ack {
	t.Helper()
	var acks []model.Ack
	if err := c.db.Order("acker asc, ackee asc, schema asc").Find(&acks).Error; err != nil {
		t.Fatal(err)
	}
	return acks
}

func TestApplyPageIngestsAcks(t *testing.T) {
	c := newReplicationCrawler(t, &replicationServer{t: t}, &manualStore{}, config.Default().Crawl)
	ctx := context.Background()
	follow := config.DefaultAckSchema
	apply := func(items ...concrnt.SignedDocument) {
		t.Helper()
		if _, err := c.applyPage(ctx, replicationDomain, items); err != nil {
			t.Fatal(err)
		}
	}

	// the acker's ack and the ackee's derived acked carry the same createdAt
	// and land in the same page: one valid row, no batch conflict
	resetCounters()
	apply(
		commitAck(t, "ack", alice, otherAuthor, follow, t0),
		commitAck(t, "acked", alice, otherAuthor, follow, t0),
		commitAck(t, "ack", alice, otherAuthor, "https://schema.concrnt.world/ack/other.json", t0),
	)
	acks := loadAcks(t, c)
	if len(acks) != 1 || acks[0].Acker != testAuthor || acks[0].Ackee != otherAuthor || acks[0].Schema != follow || !acks[0].Valid || !acks[0].CreatedAt.Equal(t0) {
		t.Fatalf("expected one valid follow row, got %+v", acks)
	}
	if got := testutil.ToFloat64(replicationCommits.WithLabelValues(replicationDomain, "ack")); got != 2 {
		t.Errorf("commits_total{kind=ack} = %v, want 2", got)
	}
	if got := testutil.ToFloat64(replicationCommits.WithLabelValues(replicationDomain, "ignored")); got != 1 {
		t.Errorf("an ack of another schema is ignored: commits_total{kind=ignored} = %v, want 1", got)
	}

	// unack with a newer createdAt invalidates; the acked side of the same
	// transition is a no-op
	apply(commitAck(t, "unack", alice, otherAuthor, follow, t1))
	apply(commitAck(t, "unacked", alice, otherAuthor, follow, t1))
	if acks := loadAcks(t, c); len(acks) != 1 || acks[0].Valid || !acks[0].CreatedAt.Equal(t1) {
		t.Fatalf("unack should invalidate the row, got %+v", acks)
	}

	// a replayed older ack cannot roll the state back (CIP-10 §4)
	apply(commitAck(t, "ack", alice, otherAuthor, follow, t0))
	if acks := loadAcks(t, c); len(acks) != 1 || acks[0].Valid || !acks[0].CreatedAt.Equal(t1) {
		t.Fatalf("stale ack must not apply, got %+v", acks)
	}
	// a same-createdAt transition is a no-op too
	apply(commitAck(t, "ack", alice, otherAuthor, follow, t1))
	if acks := loadAcks(t, c); len(acks) != 1 || acks[0].Valid {
		t.Fatalf("equal-createdAt ack must not apply, got %+v", acks)
	}

	// a newer ack re-follows; within one page the newest transition wins
	// whatever the order
	apply(
		commitAck(t, "unack", alice, otherAuthor, follow, t2.Add(time.Second)),
		commitAck(t, "ack", alice, otherAuthor, follow, t2),
	)
	if acks := loadAcks(t, c); len(acks) != 1 || acks[0].Valid || !acks[0].CreatedAt.Equal(t2.Add(time.Second)) {
		t.Fatalf("newest transition in a page should win, got %+v", acks)
	}
	apply(commitAck(t, "ack", alice, otherAuthor, follow, t2.Add(2*time.Second)))
	if acks := loadAcks(t, c); len(acks) != 1 || !acks[0].Valid {
		t.Fatalf("re-follow should be valid, got %+v", acks)
	}

	// the reverse direction is its own triple
	apply(commitAck(t, "acked", bob, testAuthor, follow, t0))
	acks = loadAcks(t, c)
	reverse := slices.IndexFunc(acks, func(a model.Ack) bool { return a.Acker == otherAuthor })
	if len(acks) != 2 || reverse < 0 || acks[reverse].Ackee != testAuthor || !acks[reverse].Valid {
		t.Fatalf("reverse follow should be a second row, got %+v", acks)
	}
}

func TestApplyPageRejectsMalformedAcks(t *testing.T) {
	c := newReplicationCrawler(t, &replicationServer{t: t}, &manualStore{}, config.Default().Crawl)
	follow := config.DefaultAckSchema

	keyedDoc := ackDocument("ack", testAuthor, otherAuthor, follow, t0)
	keyedDoc.Key = "cckv://" + testAuthor + "/x"
	keyed := alice.Sign(t, keyedDoc)

	noAssociate := commit(t, "ack", "", follow, map[string]string{}, "")

	resetCounters()
	if _, err := c.applyPage(context.Background(), replicationDomain, []concrnt.SignedDocument{
		keyed,
		noAssociate,
		commitAck(t, "ack", alice, otherAuthor+"/concrnt.world/profiles/main", follow, t0), // associate with a key
		commitAck(t, "ack", alice, "example.com", follow, t0),                              // associate owner is not a CCID
	}); err != nil {
		t.Fatal(err)
	}
	if acks := loadAcks(t, c); len(acks) != 0 {
		t.Fatalf("malformed acks must not be stored, got %+v", acks)
	}
	if got := testutil.ToFloat64(replicationMalformed.WithLabelValues(replicationDomain, "ack")); got != 4 {
		t.Errorf("malformed_total{kind=ack} = %v, want 4", got)
	}
}
