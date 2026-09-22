package meili

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/concrnt/concrnt"
	"github.com/concrnt/concrnt-crawler/internal/search/normalize"
	meilisearch "github.com/meilisearch/meilisearch-go"
)

const (
	ServersIndex     = "concrnt_servers"
	CommunitiesIndex = "concrnt_communities"
	UsersIndex       = "concrnt_users"
	PostsIndex       = "concrnt_posts"
)

// RecordIndexes are the indexes populated from record commits; a delete
// commit is applied to every one of them.
var RecordIndexes = []string{UsersIndex, CommunitiesIndex, PostsIndex}

// DeleteSpec is what a delete commit removes from one index: exact keys by
// primary key (Meilisearch string filters compare case-insensitively, ids do
// not) and a subtree by the ancestors attribute.
type DeleteSpec struct {
	IDs      []string
	Ancestor string
}

func (d DeleteSpec) IsEmpty() bool {
	return len(d.IDs) == 0 && d.Ancestor == ""
}

type ServerDocument struct {
	ID                string         `json:"id"`
	Type              string         `json:"type"`
	FQDN              string         `json:"fqdn"`
	CSID              string         `json:"csid"`
	Version           string         `json:"version"`
	SoftwareVersion   string         `json:"softwareVersion"`
	SoftwareBuildTime string         `json:"softwareBuildTime"`
	Meta              map[string]any `json:"meta"`
	LastSeenAt        time.Time      `json:"lastSeenAt"`
	LastCrawledAt     *time.Time     `json:"lastCrawledAt,omitempty"`
	Status            string         `json:"status"`
}

// ActivityDay is one UTC day of a community's activity history.
type ActivityDay struct {
	Date    string `json:"date"` // YYYY-MM-DD
	Posts   int    `json:"posts"`
	Authors int    `json:"authors"` // distinct authors that day
}

// CommunityActivityDocument is the precomputed activity of one community,
// merged into its document in the communities index. A community with no
// entries carries zeros and no lastPostAt (Meilisearch sorts documents missing
// a sortable field last). ActivityHistory runs oldest day first and ends with
// the current (partial) day; every day in the window is present, zero-filled.
type CommunityActivityDocument struct {
	ID              string        `json:"id"`
	ActivityScore   float64       `json:"activityScore"`
	PostCount7d     int           `json:"postCount7d"`
	PostCount30d    int           `json:"postCount30d"`
	ActiveAuthors7d int           `json:"activeAuthors7d"`
	LastPostAt      *time.Time    `json:"lastPostAt,omitempty"`
	ActivityHistory []ActivityDay `json:"activityHistory"`
}

type Store struct {
	client      meilisearch.ServiceManager
	taskTimeout time.Duration
	logger      *slog.Logger
}

func New(client meilisearch.ServiceManager, taskTimeout time.Duration, logger *slog.Logger) *Store {
	if taskTimeout <= 0 {
		taskTimeout = 2 * time.Minute
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Store{client: client, taskTimeout: taskTimeout, logger: logger}
}

func NewClient(host string, apiKey string) meilisearch.ServiceManager {
	opts := []meilisearch.Option{}
	if apiKey != "" {
		opts = append(opts, meilisearch.WithAPIKey(apiKey))
	}
	return meilisearch.New(host, opts...)
}

func (s *Store) EnsureIndexes(ctx context.Context) error {
	specs := []struct {
		uid        string
		searchable []string
		filterable []string
		sortable   []string
	}{
		{
			uid:        ServersIndex,
			searchable: []string{"fqdn", "csid", "softwareVersion"},
			filterable: []string{"status"},
			sortable:   []string{"lastSeenAt", "lastCrawledAt"},
		},
		{
			uid:        CommunitiesIndex,
			searchable: []string{"name", "shortname", "description", "owner", "cckv", "sourceServer"},
			filterable: []string{"owner", "sourceServer", "schema", "ancestors", "cckv"},
			sortable:   []string{"createdAt", "indexedAt", "name", "activityScore", "postCount7d", "postCount30d", "activeAuthors7d", "lastPostAt"},
		},
		{
			uid:        UsersIndex,
			searchable: []string{"username", "description", "ccid", "owner", "cckv", "sourceServer"},
			filterable: []string{"ccid", "owner", "sourceServer", "schema", "ancestors"},
			sortable:   []string{"createdAt", "indexedAt", "username"},
		},
		{
			uid:        PostsIndex,
			searchable: []string{"body", "author", "cckv", "sourceServer"},
			filterable: []string{"author", "owner", "sourceServer", "schema", "ancestors"},
			sortable:   []string{"createdAt", "indexedAt"},
		},
	}

	for _, spec := range specs {
		if err := s.ensureIndex(ctx, spec.uid); err != nil {
			return err
		}
		index := s.client.Index(spec.uid)
		task, err := index.UpdateSearchableAttributesWithContext(ctx, &spec.searchable)
		if err := s.waitTask(ctx, task, err); err != nil {
			return fmt.Errorf("update searchable attributes for %s: %w", spec.uid, err)
		}
		task, err = index.UpdateFilterableAttributesWithContext(ctx, &spec.filterable)
		if err := s.waitTask(ctx, task, err); err != nil {
			return fmt.Errorf("update filterable attributes for %s: %w", spec.uid, err)
		}
		task, err = index.UpdateSortableAttributesWithContext(ctx, &spec.sortable)
		if err := s.waitTask(ctx, task, err); err != nil {
			return fmt.Errorf("update sortable attributes for %s: %w", spec.uid, err)
		}
	}
	return nil
}

func (s *Store) ensureIndex(ctx context.Context, uid string) error {
	if _, err := s.client.GetIndexWithContext(ctx, uid); err == nil {
		return nil
	}
	task, err := s.client.CreateIndexWithContext(ctx, &meilisearch.IndexConfig{
		Uid:        uid,
		PrimaryKey: "id",
	})
	if err != nil {
		return fmt.Errorf("create index %s: %w", uid, err)
	}
	return s.waitTask(ctx, task, nil)
}

func (s *Store) UpsertServers(ctx context.Context, docs []ServerDocument) error {
	if len(docs) == 0 {
		return nil
	}
	index := s.client.Index(ServersIndex)
	task, err := index.AddDocumentsWithContext(ctx, docs, "id")
	return s.waitTask(ctx, task, err)
}

func (s *Store) UpsertUsers(ctx context.Context, docs []normalize.UserDocument) error {
	if len(docs) == 0 {
		return nil
	}
	index := s.client.Index(UsersIndex)
	task, err := index.AddDocumentsWithContext(ctx, docs, "id")
	return s.waitTask(ctx, task, err)
}

// UpsertCommunities merges (PUT) rather than replaces so the activity fields
// written by UpdateCommunityActivity survive a re-commit of the community
// record. CommunityDocument emits every field, so the merge is a full overwrite
// of the record-derived part.
func (s *Store) UpsertCommunities(ctx context.Context, docs []normalize.CommunityDocument) error {
	if len(docs) == 0 {
		return nil
	}
	index := s.client.Index(CommunitiesIndex)
	task, err := index.UpdateDocumentsWithContext(ctx, docs, "id")
	return s.waitTask(ctx, task, err)
}

// UpdateCommunityActivity merges activity fields into existing community
// documents. Callers pass only communities that are in the index: a merge into
// an unknown id would create a document holding nothing but activity.
func (s *Store) UpdateCommunityActivity(ctx context.Context, docs []CommunityActivityDocument) error {
	if len(docs) == 0 {
		return nil
	}
	index := s.client.Index(CommunitiesIndex)
	task, err := index.UpdateDocumentsWithContext(ctx, docs, "id")
	return s.waitTask(ctx, task, err)
}

func (s *Store) UpsertPosts(ctx context.Context, docs []normalize.PostDocument) error {
	if len(docs) == 0 {
		return nil
	}
	index := s.client.Index(PostsIndex)
	task, err := index.AddDocumentsWithContext(ctx, docs, "id")
	return s.waitTask(ctx, task, err)
}

// DeleteRecords applies a delete commit to one index. Each task is awaited so
// a failed delete surfaces before the replication cursor moves past it.
func (s *Store) DeleteRecords(ctx context.Context, indexUID string, spec DeleteSpec) error {
	index := s.client.Index(indexUID)
	if len(spec.IDs) > 0 {
		task, err := index.DeleteDocumentsWithContext(ctx, spec.IDs)
		if err := s.waitTask(ctx, task, err); err != nil {
			return err
		}
	}
	if spec.Ancestor != "" {
		task, err := index.DeleteDocumentsByFilterWithContext(ctx, fmt.Sprintf("ancestors = \"%s\"", escapeFilterValue(spec.Ancestor)))
		if err := s.waitTask(ctx, task, err); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) Search(ctx context.Context, indexUID string, query string, limit int64, offset int64, filter string, sort []string) (*meilisearch.SearchResponse, error) {
	req := &meilisearch.SearchRequest{
		Limit:  limit,
		Offset: offset,
	}
	if filter != "" {
		req.Filter = filter
	}
	if len(sort) > 0 {
		req.Sort = sort
	}
	return s.client.Index(indexUID).SearchWithContext(ctx, query, req)
}

func (s *Store) Stats(ctx context.Context) (*meilisearch.Stats, error) {
	return s.client.GetStatsWithContext(ctx)
}

func (s *Store) waitTask(ctx context.Context, task *meilisearch.TaskInfo, err error) error {
	if err != nil {
		return err
	}
	if task == nil {
		return nil
	}
	waitCtx, cancel := context.WithTimeout(ctx, s.taskTimeout)
	defer cancel()
	done, err := s.client.WaitForTaskWithContext(waitCtx, task.TaskUID, 100*time.Millisecond)
	if err != nil {
		return err
	}
	if done.Status != meilisearch.TaskStatusSucceeded {
		if done.Status == meilisearch.TaskStatusFailed {
			return fmt.Errorf("meilisearch task %d failed: %s", task.TaskUID, taskErrorSummary(done))
		}
		return fmt.Errorf("meilisearch task %d ended with status %s: %s", task.TaskUID, done.Status, taskErrorSummary(done))
	}
	return nil
}

func taskErrorSummary(task *meilisearch.Task) string {
	raw, err := json.Marshal(task)
	if err != nil {
		return fmt.Sprintf("%+v", task)
	}
	return string(raw)
}

func ServerDocFromWellKnown(wkc concrnt.WellKnownConcrnt, lastSeenAt time.Time, lastCrawledAt *time.Time, disabled bool) ServerDocument {
	status := "active"
	if disabled {
		status = "disabled"
	}
	return ServerDocument{
		ID:                normalize.EncodeMeiliID(wkc.Domain),
		Type:              "server",
		FQDN:              wkc.Domain,
		CSID:              wkc.CSID,
		Version:           wkc.Version,
		SoftwareVersion:   wkc.SoftwareInfo.Version,
		SoftwareBuildTime: wkc.SoftwareInfo.BuildTime,
		Meta:              wkc.Meta,
		LastSeenAt:        lastSeenAt,
		LastCrawledAt:     lastCrawledAt,
		Status:            status,
	}
}

func BuildFilter(params map[string]string, allowed map[string]bool) string {
	keys := make([]string, 0, len(params))
	for key := range params {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(params))
	for _, key := range keys {
		value := params[key]
		if value == "" || !allowed[key] {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s = \"%s\"", key, escapeFilterValue(value)))
	}
	return strings.Join(parts, " AND ")
}

// BuildSort validates a "field:asc|desc" expression ("field" alone means desc)
// against the index's sortable attributes and returns it in Meilisearch form.
func BuildSort(param string, allowed map[string]bool) ([]string, error) {
	if param == "" {
		return nil, nil
	}
	field, direction, hasDirection := strings.Cut(param, ":")
	if !hasDirection {
		direction = "desc"
	}
	if !allowed[field] {
		return nil, fmt.Errorf("unsupported sort field: %s", field)
	}
	if direction != "asc" && direction != "desc" {
		return nil, fmt.Errorf("unsupported sort direction: %s", direction)
	}
	return []string{field + ":" + direction}, nil
}

// DeleteSpecForTarget maps a delete commit target (CIP-4) onto index
// documents: "key/*" is the subtree only, "key*" is the key and its subtree,
// anything else is the single key. A ccfs target has no key to map to and
// yields an empty spec.
func DeleteSpecForTarget(target string) DeleteSpec {
	switch {
	case strings.HasSuffix(target, "/*"):
		return DeleteSpec{Ancestor: strings.TrimSuffix(target, "/*")}
	case strings.HasSuffix(target, "*"):
		base := strings.TrimSuffix(target, "*")
		return DeleteSpec{IDs: []string{normalize.EncodeMeiliID(base)}, Ancestor: base}
	case strings.HasPrefix(target, "cckv://"):
		return DeleteSpec{IDs: []string{normalize.EncodeMeiliID(target)}}
	default:
		return DeleteSpec{}
	}
}

func escapeFilterValue(value string) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, "\"", "\\\"")
	return value
}
