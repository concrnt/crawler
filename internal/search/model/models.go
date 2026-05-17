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

type CrawlCursor struct {
	ID                uint   `gorm:"primaryKey"`
	ServerDomain      string `gorm:"type:text;index:idx_cursor_scope,unique"`
	Kind              string `gorm:"type:text;index:idx_cursor_scope,unique"`
	Schema            string `gorm:"type:text;index:idx_cursor_scope,unique"`
	Prefix            string `gorm:"type:text;index:idx_cursor_scope,unique"`
	IncrementalSince  *time.Time
	BackfillUntil     *time.Time
	LastBackfillAt    *time.Time
	LastIncrementalAt *time.Time
	LastStartedAt     *time.Time
	LastFinishedAt    *time.Time
	LastErrorAt       *time.Time
	LastError         string `gorm:"type:text"`
	FailCount         int
}
