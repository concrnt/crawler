package normalize

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/concrnt/concrnt"
)

const (
	profileSchema   = "https://schema.concrnt.world/p/main.json"
	communitySchema = "https://schema.concrnt.world/t/community.json"
)

func TestEncodeCCKVIsStableAndReversible(t *testing.T) {
	cckv := "cckv://con012345678901234567890123456789012345678/profile/main"
	encoded := EncodeCCKV(cckv)
	decoded, err := DecodeCCKV(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if decoded != cckv {
		t.Fatalf("decoded cckv mismatch: got %q want %q", decoded, cckv)
	}
	if encoded != EncodeCCKV(cckv) {
		t.Fatal("cckv encoding is not stable")
	}
	if strings.ContainsAny(encoded, ".%/+:=") {
		t.Fatalf("encoded id contains characters rejected by meilisearch: %q", encoded)
	}
}

func TestEncodeMeiliIDForServerDomain(t *testing.T) {
	domain := "denken.concrnt.net"
	encoded := EncodeMeiliID(domain)
	decoded, err := DecodeMeiliID(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if decoded != domain {
		t.Fatalf("decoded domain mismatch: got %q want %q", decoded, domain)
	}
	if strings.ContainsAny(encoded, ".%/+:=") {
		t.Fatalf("encoded id contains characters rejected by meilisearch: %q", encoded)
	}
}

func TestNormalizeUser(t *testing.T) {
	createdAt := time.Date(2026, 5, 15, 1, 2, 3, 0, time.UTC)
	indexedAt := createdAt.Add(time.Minute)
	cckv := "cckv://con012345678901234567890123456789012345678/profile/main"
	sd := signedDocument(t, cckv, profileSchema, ProfileValue{
		Username:    "alice",
		Avatar:      "ccfs://avatar",
		Description: "hello",
		Banner:      "ccfs://banner",
		Subprofiles: []string{"dev"},
		Badges:      []ProfileBadge{{SeriesID: "series", BadgeID: "badge"}},
	}, createdAt)

	doc, ok, err := NormalizeUser(sd, profileSchema, "example.net", indexedAt)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected document to match schema")
	}
	if doc.ID != EncodeCCKV(cckv) || doc.CCKV != cckv {
		t.Fatalf("unexpected ids: id=%q cckv=%q", doc.ID, doc.CCKV)
	}
	if doc.Owner != "con012345678901234567890123456789012345678" || doc.CCID != doc.Owner {
		t.Fatalf("unexpected owner/ccid: owner=%q ccid=%q", doc.Owner, doc.CCID)
	}
	if doc.Username != "alice" || doc.SourceServer != "example.net" || !doc.CreatedAt.Equal(createdAt) || !doc.IndexedAt.Equal(indexedAt) {
		t.Fatalf("unexpected normalized document: %+v", doc)
	}
}

func TestNormalizeCommunity(t *testing.T) {
	createdAt := time.Date(2026, 5, 15, 1, 2, 3, 0, time.UTC)
	cckv := "cckv://example.net/community/general"
	sd := signedDocument(t, cckv, communitySchema, CommunityValue{
		Name:        "General",
		Shortname:   "general",
		Description: "All topics",
		Icon:        "ccfs://icon",
		Banner:      "ccfs://banner",
	}, createdAt)

	doc, ok, err := NormalizeCommunity(sd, communitySchema, "example.net", createdAt)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected document to match schema")
	}
	if doc.Name != "General" || doc.Shortname != "general" || doc.Owner != "example.net" {
		t.Fatalf("unexpected normalized community: %+v", doc)
	}
}

func TestNormalizeCommunityRejectsNonDomainOwner(t *testing.T) {
	for _, owner := range []string{
		"con012345678901234567890123456789012345678",
		"ccs012345678901234567890123456789012345678",
		"@example.net",
		"example.net:8000",
		"-bad.example.net",
	} {
		sd := signedDocument(t, "cckv://"+owner+"/community/general", communitySchema, CommunityValue{Name: "General"}, time.Now())
		_, ok, err := NormalizeCommunity(sd, communitySchema, "example.net", time.Now())
		if err == nil || ok {
			t.Fatalf("owner %q should be rejected: ok=%v err=%v", owner, ok, err)
		}
	}
}

func TestIsDomainOwner(t *testing.T) {
	for owner, want := range map[string]bool{
		"example.net":       true,
		"v2dev.concrnt.net": true,
		"localhost":         true,
		"Example.NET":       true,
		"con012345678901234567890123456789012345678": false,
		"ccs012345678901234567890123456789012345678": false,
		"@example.net":     false,
		"example.net:8000": false,
		"":                 false,
		"example..net":     false,
	} {
		if got := IsDomainOwner(owner); got != want {
			t.Fatalf("IsDomainOwner(%q) = %v, want %v", owner, got, want)
		}
	}
}

func TestSchemaMismatchIsSkipped(t *testing.T) {
	sd := signedDocument(t, "cckv://con012345678901234567890123456789012345678/profile/main", profileSchema, ProfileValue{}, time.Now())
	_, ok, err := NormalizeUser(sd, "https://schema.example.invalid/other.json", "example.net", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("schema mismatch should be skipped")
	}
}

func TestSameCCKVUpdateKeepsSameID(t *testing.T) {
	cckv := "cckv://con012345678901234567890123456789012345678/profile/main"
	first := signedDocument(t, cckv, profileSchema, ProfileValue{Username: "alice"}, time.Now())
	second := signedDocument(t, cckv, profileSchema, ProfileValue{Username: "alice2"}, time.Now().Add(time.Minute))

	firstDoc, ok, err := NormalizeUser(first, profileSchema, "example.net", time.Now())
	if err != nil || !ok {
		t.Fatalf("first normalize failed: ok=%v err=%v", ok, err)
	}
	secondDoc, ok, err := NormalizeUser(second, profileSchema, "example.net", time.Now())
	if err != nil || !ok {
		t.Fatalf("second normalize failed: ok=%v err=%v", ok, err)
	}
	if firstDoc.ID != secondDoc.ID {
		t.Fatalf("same cckv must produce same meilisearch id: %q != %q", firstDoc.ID, secondDoc.ID)
	}
	if secondDoc.Username != "alice2" {
		t.Fatalf("expected updated content, got %q", secondDoc.Username)
	}
}

func signedDocument(t *testing.T, key string, schema string, value any, createdAt time.Time) concrnt.SignedDocument {
	t.Helper()
	doc := concrnt.Document[any]{
		Kind:      "record",
		Key:       key,
		Value:     value,
		Author:    "con012345678901234567890123456789012345678",
		Schema:    schema,
		CreatedAt: createdAt,
	}
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	cckv := key
	return concrnt.SignedDocument{
		CCKV:     &cckv,
		Document: string(body),
		Proof:    concrnt.Proof{Type: concrnt.ProofTypeNone},
	}
}

func TestAncestors(t *testing.T) {
	cases := []struct {
		cckv string
		want []string
	}{
		{cckv: "cckv://con0123/a/b/c", want: []string{"cckv://con0123", "cckv://con0123/a", "cckv://con0123/a/b"}},
		{cckv: "cckv://con0123/a", want: []string{"cckv://con0123"}},
		{cckv: "cckv://con0123", want: []string{}},
		{cckv: "cckv://con0123@hint.example/x/y", want: []string{"cckv://con0123", "cckv://con0123/x"}},
		{cckv: "ccfs://con0123/concrnt/abc", want: []string{}},
	}
	for _, tc := range cases {
		got := Ancestors(tc.cckv)
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Fatalf("Ancestors(%q) = %v, want %v", tc.cckv, got, tc.want)
		}
	}
}

func TestNormalizePost(t *testing.T) {
	const postSchema = "https://schema.concrnt.world/m/markdown.json"
	createdAt := time.Date(2026, 9, 15, 1, 2, 3, 0, time.UTC)
	indexedAt := createdAt.Add(time.Minute)
	cckv := "cckv://con012345678901234567890123456789012345678/concrnt.world/profiles/main/posts/p1"
	sd := signedDocument(t, cckv, postSchema, map[string]any{
		"body":   "hello **world**",
		"emojis": map[string]any{"wave": map[string]string{"imageURL": "https://example.com/wave.png"}},
	}, createdAt)

	post, ok, err := NormalizePost(sd, postSchema, "example.net", indexedAt)
	if err != nil || !ok {
		t.Fatalf("NormalizePost failed: ok=%v err=%v", ok, err)
	}
	if post.Type != "post" || post.Body != "hello **world**" || post.Author != "con012345678901234567890123456789012345678" || post.Owner != "con012345678901234567890123456789012345678" {
		t.Fatalf("unexpected post: %+v", post)
	}
	if post.ID != EncodeCCKV(cckv) || post.CCKV != cckv || post.SourceServer != "example.net" || !post.CreatedAt.Equal(createdAt) {
		t.Fatalf("unexpected post identity: %+v", post)
	}
	var value map[string]any
	if err := json.Unmarshal(post.Value, &value); err != nil || value["emojis"] == nil {
		t.Fatalf("value must keep the whole record value: %s", post.Value)
	}
	if strings.Join(post.Ancestors, ",") != "cckv://con012345678901234567890123456789012345678,cckv://con012345678901234567890123456789012345678/concrnt.world,cckv://con012345678901234567890123456789012345678/concrnt.world/profiles,cckv://con012345678901234567890123456789012345678/concrnt.world/profiles/main,cckv://con012345678901234567890123456789012345678/concrnt.world/profiles/main/posts" {
		t.Fatalf("unexpected ancestors: %v", post.Ancestors)
	}

	// a bare reroute has no body but is still indexed for rendering
	reroute, ok, err := NormalizePost(signedDocument(t, cckv, postSchema, map[string]any{"targetURI": "cckv://x/y"}, createdAt), postSchema, "example.net", indexedAt)
	if err != nil || !ok || reroute.Body != "" {
		t.Fatalf("bodyless post should normalize: ok=%v err=%v body=%q", ok, err, reroute.Body)
	}

	// replication also carries non-record kinds under any schema
	if _, ok, err := NormalizePost(signedDocumentOfKind(t, "delete", cckv, postSchema, createdAt), postSchema, "example.net", indexedAt); err != nil || ok {
		t.Fatalf("non-record kind must not match: ok=%v err=%v", ok, err)
	}
}

func signedDocumentOfKind(t *testing.T, kind string, key string, schema string, createdAt time.Time) concrnt.SignedDocument {
	t.Helper()
	doc := concrnt.Document[any]{
		Kind:      kind,
		Key:       key,
		Value:     key,
		Author:    "con012345678901234567890123456789012345678",
		Schema:    schema,
		CreatedAt: createdAt,
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return concrnt.SignedDocument{Document: string(raw), Proof: concrnt.Proof{Type: concrnt.ProofTypeNone}}
}

// UpsertUsers merges into the index (meili.UpsertUsers), which only overwrites
// the fields present in the payload: every record-derived field must be
// emitted even when zero, or a cleared field would survive a re-commit
func TestUserDocumentEmitsEveryField(t *testing.T) {
	raw, err := json.Marshal(UserDocument{})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"id", "type", "cckv", "ccid", "owner", "sourceServer", "schema", "username", "description", "avatar", "banner", "subprofiles", "badges", "ancestors", "createdAt", "indexedAt"} {
		if _, ok := fields[key]; !ok {
			t.Errorf("field %q missing from a zero UserDocument: %s", key, raw)
		}
	}
	if _, ok := fields["ccfs"]; ok {
		t.Errorf("ccfs is the one optional field: %s", raw)
	}
}
