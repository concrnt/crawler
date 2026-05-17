package crawler

import (
	"encoding/json"
	"net/url"
	"testing"
	"time"

	"github.com/concrnt/concrnt"
	"github.com/concrnt/concrnt-search/internal/search/config"
)

func TestPageBoundary(t *testing.T) {
	base := time.Date(2026, 5, 15, 1, 0, 0, 0, time.UTC)
	docs := []concrnt.SignedDocument{
		testSignedDocument(t, base.Add(2*time.Minute)),
		testSignedDocument(t, base),
		testSignedDocument(t, base.Add(time.Minute)),
	}

	asc, err := PageBoundary(docs, "asc")
	if err != nil {
		t.Fatal(err)
	}
	if !asc.Equal(base.Add(2 * time.Minute)) {
		t.Fatalf("asc boundary mismatch: got %s", asc)
	}

	desc, err := PageBoundary(docs, "desc")
	if err != nil {
		t.Fatal(err)
	}
	if !desc.Equal(base) {
		t.Fatalf("desc boundary mismatch: got %s", desc)
	}

	nextBackfillUntil := desc.Add(-time.Nanosecond)
	if !nextBackfillUntil.Before(desc) {
		t.Fatal("backfill cursor must move backward")
	}
	nextIncrementalSince := asc.Add(time.Nanosecond)
	if !nextIncrementalSince.After(asc) {
		t.Fatal("incremental cursor must move forward")
	}
}

func TestShouldBackoff(t *testing.T) {
	now := time.Date(2026, 5, 15, 1, 0, 0, 0, time.UTC)
	last := now.Add(-4 * time.Minute)
	if ShouldBackoff(1, &last, now) {
		t.Fatal("first failure should wait until the next scheduled interval only")
	}
	if !ShouldBackoff(2, &last, now) {
		t.Fatal("second failure should back off for five minutes")
	}
	last = now.Add(-6 * time.Minute)
	if ShouldBackoff(2, &last, now) {
		t.Fatal("second failure backoff should expire after five minutes")
	}
}

func TestMatchesLayer(t *testing.T) {
	c := New(nil, nil, nil, configWithLayer("concrnt-mainnet"), nil)
	if !c.matchesLayer(concrnt.WellKnownConcrnt{Layer: "concrnt-mainnet"}) {
		t.Fatal("expected matching layer")
	}
	if c.matchesLayer(concrnt.WellKnownConcrnt{Layer: "concrnt-testnet"}) {
		t.Fatal("expected mismatched layer to be rejected")
	}

	c = New(nil, nil, nil, configWithLayer(""), nil)
	if !c.matchesLayer(concrnt.WellKnownConcrnt{Layer: "anything"}) {
		t.Fatal("empty target layer should accept all layers")
	}
}

func TestQueryURITemplateExpansion(t *testing.T) {
	path, err := concrnt.RenderURITemplate(
		"/query{?prefix,schema,since,until,limit,order,parent}",
		map[string]string{
			"prefix": "cckv://",
			"schema": "https://schema.concrnt.world/p/main.json",
			"limit":  "100",
			"order":  "asc",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Path != "/query" {
		t.Fatalf("path mismatch: %s", parsed.Path)
	}
	query := parsed.Query()
	if query.Get("prefix") != "cckv://" || query.Get("schema") != "https://schema.concrnt.world/p/main.json" || query.Get("limit") != "100" || query.Get("order") != "asc" {
		t.Fatalf("query mismatch: %s", parsed.RawQuery)
	}
}

func testSignedDocument(t *testing.T, createdAt time.Time) concrnt.SignedDocument {
	t.Helper()
	doc := concrnt.Document[map[string]string]{
		Key:       "cckv://con012345678901234567890123456789012345678/profile/main",
		Value:     map[string]string{"username": "alice"},
		Author:    "con012345678901234567890123456789012345678",
		Schema:    "https://schema.concrnt.world/p/main.json",
		CreatedAt: createdAt,
	}
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return concrnt.SignedDocument{
		Document: string(body),
		Proof:    concrnt.Proof{Type: concrnt.ProofTypeNone},
	}
}

func configWithLayer(layer string) config.Crawl {
	cfg := config.Default().Crawl
	cfg.Layer = layer
	return cfg
}
