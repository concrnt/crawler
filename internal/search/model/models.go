package model

import (
	"database/sql/driver"
	"fmt"
	"time"
)

type JSONB string

func (j JSONB) Value() (driver.Value, error) {
	if j == "" {
		return "{}", nil
	}
	return string(j), nil
}

func (j *JSONB) Scan(value any) error {
	switch v := value.(type) {
	case nil:
		*j = ""
		return nil
	case []byte:
		*j = JSONB(string(v))
		return nil
	case string:
		*j = JSONB(v)
		return nil
	default:
		return fmt.Errorf("unsupported JSONB scan type %T", value)
	}
}

type ServerState struct {
	Domain        string `gorm:"primaryKey;type:text"`
	CSID          string `gorm:"column:cs_id;type:text;index"`
	Layer         string `gorm:"type:text"`
	Version       string `gorm:"type:text"`
	WellKnownJSON JSONB  `gorm:"type:jsonb"`
	FirstSeenAt   time.Time
	LastSeenAt    time.Time
	LastCrawledAt *time.Time
	LastErrorAt   *time.Time
	LastError     string `gorm:"type:text"`
	FailCount     int
	Disabled      bool
}

// ReplicationCursor is the per-server position in the CIP-16 replication
// feed. CursorAt only ever holds values taken from the server's own cursors
// (prev/next, i.e. commit receipt time), never the crawler's clock.
type ReplicationCursor struct {
	ServerDomain   string `gorm:"primaryKey;type:text"`
	CursorAt       *time.Time
	CaughtUpAt     *time.Time
	LastStartedAt  *time.Time
	LastFinishedAt *time.Time
	LastErrorAt    *time.Time
	LastError      string `gorm:"type:text"`
	FailCount      int
}

// IndexedCommunity mirrors the community keys held in the communities index.
// An incoming record is counted as community activity when its parent key is
// in here; the mirror is updated in log order alongside the index, so a
// community's creation commit is seen before the references delivered to it.
type IndexedCommunity struct {
	CCKV      string `gorm:"primaryKey;type:text"`
	IndexedAt time.Time
}

// CommunityEntry is one record committed directly under a community key,
// normally the reference record a distribution (CIP-7) leaves there. CreatedAt
// is the record's own createdAt: for a reference that is the delivering
// server's clock, not the author's. Href is the referenced document's key
// (empty for a non-reference record): deleting the original document sweeps
// its references server-side without a commit of their own (CIP-4 §6.1), so
// the delete commit for the original is matched against it here.
type CommunityEntry struct {
	CommunityCCKV string    `gorm:"primaryKey;type:text"`
	EntryCCKV     string    `gorm:"primaryKey;type:text"`
	Author        string    `gorm:"type:text"`
	Href          string    `gorm:"type:text;index"`
	CreatedAt     time.Time `gorm:"index"`
}
