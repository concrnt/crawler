package normalize

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/concrnt/concrnt"
)

type ProfileBadge struct {
	SeriesID string `json:"seriesId"`
	BadgeID  string `json:"badgeId"`
}

type ProfileValue struct {
	Username    string         `json:"username"`
	Avatar      string         `json:"avatar"`
	Description string         `json:"description"`
	Banner      string         `json:"banner"`
	Subprofiles []string       `json:"subprofiles"`
	Badges      []ProfileBadge `json:"badges"`
}

type CommunityValue struct {
	Name        string `json:"name"`
	Shortname   string `json:"shortname"`
	Description string `json:"description"`
	Icon        string `json:"icon"`
	Banner      string `json:"banner"`
}

type PostValue struct {
	Body string `json:"body"`
}

// hostnameRe is RFC 1123 hostname syntax: dot-separated labels of letters,
// digits and inner hyphens. CCID/CSID are valid labels too, so they are
// excluded separately in IsDomainOwner.
var hostnameRe = regexp.MustCompile(`^(?i:[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)(\.(?i:[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?))*$`)

// IsDomainOwner reports whether a cckv owner is a server FQDN (CIP-0 owner
// forms: CCID, CSID, FQDN, or the alias @FQDN; only the plain FQDN counts).
func IsDomainOwner(owner string) bool {
	if concrnt.IsCCID(owner) || concrnt.IsCSID(owner) {
		return false
	}
	return len(owner) <= 253 && hostnameRe.MatchString(owner)
}

type UserDocument struct {
	ID           string         `json:"id"`
	Type         string         `json:"type"`
	CCKV         string         `json:"cckv"`
	CCFS         string         `json:"ccfs,omitempty"`
	CCID         string         `json:"ccid"`
	Owner        string         `json:"owner"`
	SourceServer string         `json:"sourceServer"`
	Schema       string         `json:"schema"`
	Username     string         `json:"username"`
	Description  string         `json:"description"`
	Avatar       string         `json:"avatar"`
	Banner       string         `json:"banner"`
	Subprofiles  []string       `json:"subprofiles"`
	Badges       []ProfileBadge `json:"badges"`
	Ancestors    []string       `json:"ancestors"`
	CreatedAt    time.Time      `json:"createdAt"`
	IndexedAt    time.Time      `json:"indexedAt"`
}

type CommunityDocument struct {
	ID           string    `json:"id"`
	Type         string    `json:"type"`
	CCKV         string    `json:"cckv"`
	CCFS         string    `json:"ccfs,omitempty"`
	Owner        string    `json:"owner"`
	SourceServer string    `json:"sourceServer"`
	Schema       string    `json:"schema"`
	Name         string    `json:"name"`
	Shortname    string    `json:"shortname"`
	Description  string    `json:"description"`
	Icon         string    `json:"icon"`
	Banner       string    `json:"banner"`
	Ancestors    []string  `json:"ancestors"`
	CreatedAt    time.Time `json:"createdAt"`
	IndexedAt    time.Time `json:"indexedAt"`
}

// PostDocument is a world message record. Body is the searchable text; Value
// keeps the whole record value so a search hit can be rendered as-is.
type PostDocument struct {
	ID           string          `json:"id"`
	Type         string          `json:"type"`
	CCKV         string          `json:"cckv"`
	CCFS         string          `json:"ccfs,omitempty"`
	Author       string          `json:"author"`
	Owner        string          `json:"owner"`
	SourceServer string          `json:"sourceServer"`
	Schema       string          `json:"schema"`
	Body         string          `json:"body"`
	Value        json.RawMessage `json:"value"`
	Ancestors    []string        `json:"ancestors"`
	CreatedAt    time.Time       `json:"createdAt"`
	IndexedAt    time.Time       `json:"indexedAt"`
}

type ParsedDocument struct {
	Document  concrnt.Document[json.RawMessage]
	CCKV      string
	CCFS      string
	Owner     string
	ID        string
	Ancestors []string
}

func EncodeCCKV(cckv string) string {
	return EncodeMeiliID(cckv)
}

func DecodeCCKV(encoded string) (string, error) {
	return DecodeMeiliID(encoded)
}

func EncodeMeiliID(value string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}

func DecodeMeiliID(encoded string) (string, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", err
	}
	return string(decoded), nil
}

// Ancestors lists every proper ancestor key of a cckv URI, root first:
// cckv://o/a/b/c -> [cckv://o, cckv://o/a, cckv://o/a/b]. A range delete
// (CIP-4 "key/*") removes exactly the documents that carry base in here.
func Ancestors(cckv string) []string {
	parsed, err := concrnt.ParseCCURI(cckv)
	if err != nil || parsed.Scheme != "cckv" || parsed.Owner == "" {
		return []string{}
	}
	key := strings.Trim(parsed.Key, "/")
	if key == "" {
		return []string{}
	}
	root := concrnt.ComposeCCURI("cckv", parsed.Owner, "")
	segments := strings.Split(key, "/")
	out := make([]string, 0, len(segments))
	out = append(out, root)
	for i := 1; i < len(segments); i++ {
		out = append(out, root+"/"+strings.Join(segments[:i], "/"))
	}
	return out
}

func ParseSignedDocument(sd concrnt.SignedDocument, expectedSchema string) (ParsedDocument, bool, error) {
	var doc concrnt.Document[json.RawMessage]
	if err := json.Unmarshal([]byte(sd.Document), &doc); err != nil {
		return ParsedDocument{}, false, fmt.Errorf("decode signed document: %w", err)
	}
	if doc.Schema != expectedSchema {
		return ParsedDocument{}, false, nil
	}
	// replication also carries entity/delete/ack commits; only records have a
	// key to index under
	if doc.Kind != "record" {
		return ParsedDocument{}, false, nil
	}

	cckv := doc.Key
	if cckv == "" && sd.CCKV != nil {
		cckv = *sd.CCKV
	}
	if cckv == "" {
		return ParsedDocument{}, false, fmt.Errorf("document has no cckv key")
	}

	parsed, err := concrnt.ParseCCURI(cckv)
	if err != nil {
		return ParsedDocument{}, false, fmt.Errorf("parse cckv: %w", err)
	}
	if parsed.Owner == "" {
		return ParsedDocument{}, false, fmt.Errorf("cckv owner is empty")
	}

	ccfs := ""
	if sd.CCFS != nil {
		ccfs = *sd.CCFS
	}

	return ParsedDocument{
		Document:  doc,
		CCKV:      cckv,
		CCFS:      ccfs,
		Owner:     parsed.Owner,
		ID:        EncodeCCKV(cckv),
		Ancestors: Ancestors(cckv),
	}, true, nil
}

func NormalizeUser(sd concrnt.SignedDocument, expectedSchema string, sourceServer string, indexedAt time.Time) (UserDocument, bool, error) {
	parsed, ok, err := ParseSignedDocument(sd, expectedSchema)
	if err != nil || !ok {
		return UserDocument{}, ok, err
	}

	var value ProfileValue
	if err := json.Unmarshal(parsed.Document.Value, &value); err != nil {
		return UserDocument{}, false, fmt.Errorf("decode profile value: %w", err)
	}

	return UserDocument{
		ID:           parsed.ID,
		Type:         "user",
		CCKV:         parsed.CCKV,
		CCFS:         parsed.CCFS,
		CCID:         parsed.Owner,
		Owner:        parsed.Owner,
		SourceServer: sourceServer,
		Schema:       parsed.Document.Schema,
		Username:     value.Username,
		Description:  value.Description,
		Avatar:       value.Avatar,
		Banner:       value.Banner,
		Subprofiles:  value.Subprofiles,
		Badges:       value.Badges,
		Ancestors:    parsed.Ancestors,
		CreatedAt:    parsed.Document.CreatedAt,
		IndexedAt:    indexedAt,
	}, true, nil
}

func NormalizeCommunity(sd concrnt.SignedDocument, expectedSchema string, sourceServer string, indexedAt time.Time) (CommunityDocument, bool, error) {
	parsed, ok, err := ParseSignedDocument(sd, expectedSchema)
	if err != nil || !ok {
		return CommunityDocument{}, ok, err
	}

	// communities must be domain-owned; user-owned ones are out of search scope
	// for now
	if !IsDomainOwner(parsed.Owner) {
		return CommunityDocument{}, false, fmt.Errorf("community owner %q is not a domain: only domain-owned communities are indexed", parsed.Owner)
	}

	var value CommunityValue
	if err := json.Unmarshal(parsed.Document.Value, &value); err != nil {
		return CommunityDocument{}, false, fmt.Errorf("decode community value: %w", err)
	}

	return CommunityDocument{
		ID:           parsed.ID,
		Type:         "community",
		CCKV:         parsed.CCKV,
		CCFS:         parsed.CCFS,
		Owner:        parsed.Owner,
		SourceServer: sourceServer,
		Schema:       parsed.Document.Schema,
		Name:         value.Name,
		Shortname:    value.Shortname,
		Description:  value.Description,
		Icon:         value.Icon,
		Banner:       value.Banner,
		Ancestors:    parsed.Ancestors,
		CreatedAt:    parsed.Document.CreatedAt,
		IndexedAt:    indexedAt,
	}, true, nil
}

func NormalizePost(sd concrnt.SignedDocument, expectedSchema string, sourceServer string, indexedAt time.Time) (PostDocument, bool, error) {
	parsed, ok, err := ParseSignedDocument(sd, expectedSchema)
	if err != nil || !ok {
		return PostDocument{}, ok, err
	}

	// body is optional on some schemas (a bare reroute); the value still gets
	// indexed so the hit can be rendered
	var value PostValue
	if err := json.Unmarshal(parsed.Document.Value, &value); err != nil {
		return PostDocument{}, false, fmt.Errorf("decode post value: %w", err)
	}

	return PostDocument{
		ID:           parsed.ID,
		Type:         "post",
		CCKV:         parsed.CCKV,
		CCFS:         parsed.CCFS,
		Author:       parsed.Document.Author,
		Owner:        parsed.Owner,
		SourceServer: sourceServer,
		Schema:       parsed.Document.Schema,
		Body:         value.Body,
		Value:        parsed.Document.Value,
		Ancestors:    parsed.Ancestors,
		CreatedAt:    parsed.Document.CreatedAt,
		IndexedAt:    indexedAt,
	}, true, nil
}
