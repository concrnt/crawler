package crawler

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/concrnt/concrnt"
	"github.com/concrnt/concrnt-crawler/internal/search/config"
)

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

func TestReplicationURITemplateExpansion(t *testing.T) {
	since := "2026-09-15T12:00:00.123456Z"
	path, err := concrnt.RenderURITemplate(
		"/api/v2/replication{?owner,since,until,limit,order}",
		map[string]string{
			"since": since,
			"limit": "100",
			"order": "asc",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Path != "/api/v2/replication" {
		t.Fatalf("path mismatch: %s", parsed.Path)
	}
	query := parsed.Query()
	if query.Get("since") != since || query.Get("limit") != "100" || query.Get("order") != "asc" || query.Has("owner") || query.Has("until") {
		t.Fatalf("query mismatch: %s", parsed.RawQuery)
	}
}

func testSignedDocument(t *testing.T, createdAt time.Time) concrnt.SignedDocument {
	t.Helper()
	doc := concrnt.Document[map[string]string]{
		Kind:      "record",
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

func testPostDocument(t *testing.T, key string, body string, createdAt time.Time) concrnt.SignedDocument {
	t.Helper()
	doc := concrnt.Document[map[string]string]{
		Kind:      "record",
		Key:       key,
		Value:     map[string]string{"body": body},
		Author:    "con012345678901234567890123456789012345678",
		Schema:    "https://schema.concrnt.world/m/markdown.json",
		CreatedAt: createdAt,
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	ccfs := "ccfs://con012345678901234567890123456789012345678/concrnt/" + strings.ReplaceAll(key[strings.LastIndex(key, "/")+1:], "-", "")
	return concrnt.SignedDocument{
		CCFS:     &ccfs,
		Document: string(raw),
		Proof:    concrnt.Proof{Type: concrnt.ProofTypeNone},
	}
}

func configWithLayer(layer string) config.Crawl {
	cfg := config.Default().Crawl
	cfg.Layer = layer
	return cfg
}
