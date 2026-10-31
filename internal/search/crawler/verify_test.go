package crawler

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/concrnt/concrnt"
	"github.com/concrnt/concrnt-crawler/internal/search/config"
	"github.com/concrnt/concrnt-crawler/internal/search/sigtest"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func rejected(reason string) float64 {
	return testutil.ToFloat64(replicationRejected.WithLabelValues(replicationDomain, reason))
}

func postDocument(author string, key string, body string, createdAt time.Time) concrnt.Document[map[string]string] {
	return concrnt.Document[map[string]string]{Kind: "record", Key: key, Value: map[string]string{"body": body}, Author: author, Schema: config.DefaultPostSchemas[0], CreatedAt: createdAt}
}

func profileDocument(author string, owner string, username string) concrnt.Document[map[string]string] {
	return concrnt.Document[map[string]string]{Kind: "record", Key: "cckv://" + owner + "/concrnt.world/profiles/main", Value: map[string]string{"username": username}, Author: author, Schema: config.DefaultProfileSchema, CreatedAt: t0}
}

func TestApplyPageRejectsNoneProof(t *testing.T) {
	store := &manualStore{}
	c := newReplicationCrawler(t, &replicationServer{t: t}, store, config.Default().Crawl)
	resetCounters()

	// a v1 import: every kind arrives unsigned and none of it is indexed
	associate := "cckv://" + otherAuthor
	if _, err := c.applyPage(context.Background(), replicationDomain, []concrnt.SignedDocument{
		sigtest.NoneProof(t, postDocument(testAuthor, postsParent+"/p1", "unsigned", t0)),
		sigtest.NoneProof(t, profileDocument(testAuthor, testAuthor, "alice")),
		sigtest.NoneProof(t, concrnt.Document[string]{Kind: "delete", Value: postsParent + "/p0", Author: testAuthor, CreatedAt: t0}),
		sigtest.NoneProof(t, concrnt.Document[map[string]string]{Kind: "ack", Value: map[string]string{}, Author: testAuthor, Associate: &associate, Schema: config.DefaultAckSchema, CreatedAt: t0}),
	}); err != nil {
		t.Fatal(err)
	}
	if len(store.calls) != 0 {
		t.Fatalf("unsigned documents must not reach the store: %v", store.calls)
	}
	if acks := loadAcks(t, c); len(acks) != 0 {
		t.Fatalf("unsigned ack must not be stored: %+v", acks)
	}
	if got := rejected(rejectNoneProof); got != 4 {
		t.Errorf("rejected_total{reason=none_proof} = %v, want 4", got)
	}
	for _, kind := range []string{"post", "user", "delete", "ack"} {
		if got := testutil.ToFloat64(replicationCommits.WithLabelValues(replicationDomain, kind)); got != 0 {
			t.Errorf("commits_total{kind=%q} = %v, want 0", kind, got)
		}
	}
}

func TestApplyPageRejectsBadSignatures(t *testing.T) {
	store := &manualStore{}
	c := newReplicationCrawler(t, &replicationServer{t: t}, store, config.Default().Crawl)
	resetCounters()

	associate := "cckv://" + otherAuthor
	if _, err := c.applyPage(context.Background(), replicationDomain, []concrnt.SignedDocument{
		// edited after signing
		sigtest.Tamper(t, alice.Sign(t, postDocument(testAuthor, postsParent+"/p1", "hello", t0)), `"body":"hello"`, `"body":"forged"`),
		// signed by someone other than the claimed author
		bob.Sign(t, postDocument(testAuthor, postsParent+"/p2", "hello", t0)),
		// an author that is not a CCID has no key to recover
		alice.Sign(t, concrnt.Document[map[string]string]{Kind: "ack", Value: map[string]string{}, Author: "example.com", Associate: &associate, Schema: config.DefaultAckSchema, CreatedAt: t0}),
		// a well-signed post in the same page still lands
		alice.Sign(t, postDocument(testAuthor, postsParent+"/p3", "genuine", t0)),
	}); err != nil {
		t.Fatal(err)
	}
	if len(store.posts) != 1 || store.posts[0].Body != "genuine" {
		t.Fatalf("only the genuine post may be indexed, got %+v", store.posts)
	}
	if got := rejected(rejectSignature); got != 3 {
		t.Errorf("rejected_total{reason=signature} = %v, want 3", got)
	}
	if got := testutil.ToFloat64(replicationMalformed.WithLabelValues(replicationDomain, "ack")); got != 0 {
		t.Errorf("a bad signature is not a malformed ack: malformed_total{kind=ack} = %v, want 0", got)
	}
}

func TestApplyPageRejectsProfileAuthoredByAnother(t *testing.T) {
	store := &manualStore{}
	c := newReplicationCrawler(t, &replicationServer{t: t}, store, config.Default().Crawl)
	resetCounters()

	if _, err := c.applyPage(context.Background(), replicationDomain, []concrnt.SignedDocument{
		// bob's valid signature on a profile under alice's key: the index
		// would attribute it to alice
		bob.Sign(t, profileDocument(otherAuthor, testAuthor, "impostor")),
		alice.Sign(t, profileDocument(testAuthor, testAuthor, "alice")),
		// the rule is about CCID-owned keys: a community under a domain may
		// be authored by anyone
		alice.Sign(t, concrnt.Document[map[string]string]{Kind: "record", Key: general, Value: map[string]string{"name": "general"}, Author: testAuthor, Schema: config.DefaultCommunitySchema, CreatedAt: t0}),
	}); err != nil {
		t.Fatal(err)
	}
	if len(store.users) != 1 || store.users[0].Username != "alice" || store.users[0].CCID != testAuthor {
		t.Fatalf("only the owner's profile may be indexed, got %+v", store.users)
	}
	if len(store.communities) != 1 {
		t.Fatalf("domain-owned community should be indexed, got %+v", store.communities)
	}
	if got := rejected(rejectOwnerMismatch); got != 1 {
		t.Errorf("rejected_total{reason=owner_mismatch} = %v, want 1", got)
	}
}

func TestApplyPageVerifiesSubkeyProofs(t *testing.T) {
	sub := sigtest.NewSubkey(t, alice, "cckv://"+alice.CCID+"/subkeys/1", t0.Add(-24*time.Hour))
	server := &replicationServer{t: t, resources: map[string]concrnt.SignedDocument{
		"cckv://" + alice.CCID: sigtest.EntityRecord(t, alice, replicationDomain),
		sub.URI:                sub.Enact,
	}}
	store := &manualStore{}
	c := newReplicationCrawler(t, server, store, config.Default().Crawl)
	resetCounters()

	if _, err := c.applyPage(context.Background(), replicationDomain, []concrnt.SignedDocument{
		sub.Sign(t, postDocument(testAuthor, postsParent+"/p1", "from subkey", t0)),
		// signed before the subkey was enacted
		sub.Sign(t, postDocument(testAuthor, postsParent+"/p2", "too early", t0.Add(-48*time.Hour))),
		// the subkey document is not on the server
		sigtest.NewSubkey(t, alice, "cckv://"+alice.CCID+"/subkeys/unknown", t0).Sign(t, postDocument(testAuthor, postsParent+"/p3", "unknown key", t0)),
	}); err != nil {
		t.Fatal(err)
	}
	if len(store.posts) != 1 || store.posts[0].Body != "from subkey" {
		t.Fatalf("expected the subkey-signed post only, got %+v", store.posts)
	}
	// the enact document is fetched from alice's home server, found through
	// her entity record: both lookups go to the stub, none is trusted inline
	want := []string{"cckv://" + alice.CCID, sub.URI, "cckv://" + alice.CCID + "/subkeys/unknown"}
	if !slices.Equal(server.resolves, want) {
		t.Fatalf("resolved uris mismatch:\n got: %v\nwant: %v", server.resolves, want)
	}
	if got := rejected(rejectResolve); got != 2 {
		t.Errorf("rejected_total{reason=resolve} = %v, want 2", got)
	}
}

func TestApplyPageRequiresDocumentDirectProofForAcked(t *testing.T) {
	c := newReplicationCrawler(t, &replicationServer{t: t}, &manualStore{}, config.Default().Crawl)
	resetCounters()

	// an acked written by the acker itself rather than derived by the
	// ackee's server: a valid signature, but the wrong proof for the kind
	if _, err := c.applyPage(context.Background(), replicationDomain, []concrnt.SignedDocument{
		alice.Sign(t, ackDocument("acked", testAuthor, otherAuthor, config.DefaultAckSchema, t0)),
	}); err != nil {
		t.Fatal(err)
	}
	if acks := loadAcks(t, c); len(acks) != 0 {
		t.Fatalf("author-signed acked must not be stored, got %+v", acks)
	}
	if got := rejected(rejectProofType); got != 1 {
		t.Errorf("rejected_total{reason=proof_type} = %v, want 1", got)
	}
}
