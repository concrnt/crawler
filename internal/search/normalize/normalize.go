package normalize

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
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
	CreatedAt    time.Time `json:"createdAt"`
	IndexedAt    time.Time `json:"indexedAt"`
}

type ParsedDocument struct {
	Document concrnt.Document[json.RawMessage]
	CCKV     string
	CCFS     string
	Owner    string
	ID       string
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

func ParseSignedDocument(sd concrnt.SignedDocument, expectedSchema string) (ParsedDocument, bool, error) {
	var doc concrnt.Document[json.RawMessage]
	if err := json.Unmarshal([]byte(sd.Document), &doc); err != nil {
		return ParsedDocument{}, false, fmt.Errorf("decode signed document: %w", err)
	}
	if doc.Schema != expectedSchema {
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
		Document: doc,
		CCKV:     cckv,
		CCFS:     ccfs,
		Owner:    parsed.Owner,
		ID:       EncodeCCKV(cckv),
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
		CreatedAt:    parsed.Document.CreatedAt,
		IndexedAt:    indexedAt,
	}, true, nil
}

func NormalizeCommunity(sd concrnt.SignedDocument, expectedSchema string, sourceServer string, indexedAt time.Time) (CommunityDocument, bool, error) {
	parsed, ok, err := ParseSignedDocument(sd, expectedSchema)
	if err != nil || !ok {
		return CommunityDocument{}, ok, err
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
		CreatedAt:    parsed.Document.CreatedAt,
		IndexedAt:    indexedAt,
	}, true, nil
}
