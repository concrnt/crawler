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
