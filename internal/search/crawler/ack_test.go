package crawler

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/concrnt/concrnt"
	"github.com/concrnt/concrnt-crawler/internal/search/config"
	"github.com/concrnt/concrnt-crawler/internal/search/model"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// commitAck builds an ack-kind commit: no key, the target as a bare entity
// URI in associate (CIP-10 §3).
func commitAck(t *testing.T, kind string, author string, target string, schema string, createdAt time.Time) concrnt.SignedDocument {
	t.Helper()
	associate := "cckv://" + target
	doc := concrnt.Document[map[string]string]{
		Kind:      kind,
		Value:     map[string]string{},
		Author:    author,
		Associate: &associate,
		Schema:    schema,
		CreatedAt: createdAt,
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	ccfs := "ccfs://" + author + "/concrnt/" + kind
	return concrnt.SignedDocument{CCFS: &ccfs, Document: string(raw), Proof: concrnt.Proof{Type: concrnt.ProofTypeNone}}
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
		commitAck(t, "ack", testAuthor, otherAuthor, follow, t0),
		commitAck(t, "acked", testAuthor, otherAuthor, follow, t0),
		commitAck(t, "ack", testAuthor, otherAuthor, "https://schema.concrnt.world/ack/other.json", t0),
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
	apply(commitAck(t, "unack", testAuthor, otherAuthor, follow, t1))
	apply(commitAck(t, "unacked", testAuthor, otherAuthor, follow, t1))
	if acks := loadAcks(t, c); len(acks) != 1 || acks[0].Valid || !acks[0].CreatedAt.Equal(t1) {
		t.Fatalf("unack should invalidate the row, got %+v", acks)
	}

	// a replayed older ack cannot roll the state back (CIP-10 §4)
	apply(commitAck(t, "ack", testAuthor, otherAuthor, follow, t0))
	if acks := loadAcks(t, c); len(acks) != 1 || acks[0].Valid || !acks[0].CreatedAt.Equal(t1) {
		t.Fatalf("stale ack must not apply, got %+v", acks)
	}
	// a same-createdAt transition is a no-op too
	apply(commitAck(t, "ack", testAuthor, otherAuthor, follow, t1))
	if acks := loadAcks(t, c); len(acks) != 1 || acks[0].Valid {
		t.Fatalf("equal-createdAt ack must not apply, got %+v", acks)
	}

	// a newer ack re-follows; within one page the newest transition wins
	// whatever the order
	apply(
		commitAck(t, "unack", testAuthor, otherAuthor, follow, t2.Add(time.Second)),
		commitAck(t, "ack", testAuthor, otherAuthor, follow, t2),
	)
	if acks := loadAcks(t, c); len(acks) != 1 || acks[0].Valid || !acks[0].CreatedAt.Equal(t2.Add(time.Second)) {
		t.Fatalf("newest transition in a page should win, got %+v", acks)
	}
	apply(commitAck(t, "ack", testAuthor, otherAuthor, follow, t2.Add(2*time.Second)))
	if acks := loadAcks(t, c); len(acks) != 1 || !acks[0].Valid {
		t.Fatalf("re-follow should be valid, got %+v", acks)
	}

	// the reverse direction is its own triple
	apply(commitAck(t, "acked", otherAuthor, testAuthor, follow, t0))
	if acks := loadAcks(t, c); len(acks) != 2 || acks[1].Acker != otherAuthor || acks[1].Ackee != testAuthor || !acks[1].Valid {
		t.Fatalf("reverse follow should be a second row, got %+v", acks)
	}
}

func TestApplyPageRejectsMalformedAcks(t *testing.T) {
	c := newReplicationCrawler(t, &replicationServer{t: t}, &manualStore{}, config.Default().Crawl)
	follow := config.DefaultAckSchema

	keyed := commitAck(t, "ack", testAuthor, otherAuthor, follow, t0)
	var doc concrnt.Document[map[string]string]
	if err := json.Unmarshal([]byte(keyed.Document), &doc); err != nil {
		t.Fatal(err)
	}
	doc.Key = "cckv://" + testAuthor + "/x"
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	keyed.Document = string(raw)

	noAssociate := commit(t, "ack", "", follow, map[string]string{}, "")

	resetCounters()
	if _, err := c.applyPage(context.Background(), replicationDomain, []concrnt.SignedDocument{
		keyed,
		noAssociate,
		commitAck(t, "ack", testAuthor, otherAuthor+"/concrnt.world/profiles/main", follow, t0), // associate with a key
		commitAck(t, "ack", testAuthor, "example.com", follow, t0),                              // associate owner is not a CCID
		commitAck(t, "ack", "example.com", otherAuthor, follow, t0),                             // author is not a CCID
	}); err != nil {
		t.Fatal(err)
	}
	if acks := loadAcks(t, c); len(acks) != 0 {
		t.Fatalf("malformed acks must not be stored, got %+v", acks)
	}
	if got := testutil.ToFloat64(replicationMalformed.WithLabelValues(replicationDomain, "ack")); got != 5 {
		t.Errorf("malformed_total{kind=ack} = %v, want 5", got)
	}
}
