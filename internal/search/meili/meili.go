package meili

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/concrnt/concrnt"
	"github.com/concrnt/concrnt-crawler/internal/search/normalize"
	meilisearch "github.com/meilisearch/meilisearch-go"
	"github.com/prometheus/client_golang/prometheus"
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

// UserActivityDay is one UTC day of a user's activity history: only the
// posts, since a user is a single author.
type UserActivityDay struct {
	Date  string `json:"date"` // YYYY-MM-DD
	Posts int    `json:"posts"`
}

// UserActivityDocument is the precomputed activity of one user (aggregated
// per CCID from the user's own post records), merged into every profile
// document of that CCID in the users index. Same shape and conventions as
// CommunityActivityDocument, minus activeAuthors7d.
type UserActivityDocument struct {
	ID              string            `json:"id"`
	ActivityScore   float64           `json:"activityScore"`
	PostCount7d     int               `json:"postCount7d"`
	PostCount30d    int               `json:"postCount30d"`
	LastPostAt      *time.Time        `json:"lastPostAt,omitempty"`
	ActivityHistory []UserActivityDay `json:"activityHistory"`
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
			sortable:   []string{"createdAt", "indexedAt", "username", "activityScore", "postCount7d", "postCount30d", "lastPostAt"},
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
	defer prometheus.NewTimer(meiliWriteDuration.WithLabelValues("servers")).ObserveDuration()
	index := s.client.Index(ServersIndex)
	task, err := index.AddDocumentsWithContext(ctx, docs, "id")
	return s.waitTask(ctx, task, err)
}

// UpsertUsers merges (PUT) rather than replaces so the activity fields
// written by UpdateUserActivity survive a re-commit of the profile record.
// UserDocument emits every field, so the merge is a full overwrite of the
// record-derived part, except createdAt, which keeps the earliest value the
// index has seen for the key (see existingCreatedAt), so an edited profile
// does not surface as new.
func (s *Store) UpsertUsers(ctx context.Context, docs []normalize.UserDocument) error {
	if len(docs) == 0 {
		return nil
	}
	defer prometheus.NewTimer(meiliWriteDuration.WithLabelValues("users")).ObserveDuration()
	ids := make([]string, len(docs))
	for i, doc := range docs {
		ids[i] = doc.ID
	}
	existing, err := s.existingCreatedAt(ctx, UsersIndex, ids)
	if err != nil {
		return err
	}
	for i := range docs {
		docs[i].CreatedAt = earliestCreatedAt(existing, docs[i].ID, docs[i].CreatedAt)
	}
	index := s.client.Index(UsersIndex)
	task, err := index.UpdateDocumentsWithContext(ctx, docs, "id")
	return s.waitTask(ctx, task, err)
}

// UpsertCommunities merges (PUT) rather than replaces so the activity fields
// written by UpdateCommunityActivity survive a re-commit of the community
// record. CommunityDocument emits every field, so the merge is a full overwrite
// of the record-derived part, except createdAt, which keeps the earliest value
// the index has seen for the key (see existingCreatedAt).
func (s *Store) UpsertCommunities(ctx context.Context, docs []normalize.CommunityDocument) error {
	if len(docs) == 0 {
		return nil
	}
	defer prometheus.NewTimer(meiliWriteDuration.WithLabelValues("communities")).ObserveDuration()
	ids := make([]string, len(docs))
	for i, doc := range docs {
		ids[i] = doc.ID
	}
	existing, err := s.existingCreatedAt(ctx, CommunitiesIndex, ids)
	if err != nil {
		return err
	}
	for i := range docs {
		docs[i].CreatedAt = earliestCreatedAt(existing, docs[i].ID, docs[i].CreatedAt)
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
	defer prometheus.NewTimer(meiliWriteDuration.WithLabelValues("activity")).ObserveDuration()
	index := s.client.Index(CommunitiesIndex)
	task, err := index.UpdateDocumentsWithContext(ctx, docs, "id")
	return s.waitTask(ctx, task, err)
}

// existingCreatedAt returns the createdAt of the documents already in an
// index, keyed by id; ids not in the index are absent. A record is
// re-committed with a fresh createdAt on every edit, and a server coming back
// from a long outage replays its whole log, so "newest" sorted on the latest
// commit's createdAt would fill up with old users and communities. The index
// therefore keeps the earliest createdAt it has seen for a key. Posts are
// never edited, so only users and communities go through this.
func (s *Store) existingCreatedAt(ctx context.Context, indexUID string, ids []string) (map[string]time.Time, error) {
	index := s.client.Index(indexUID)
	existing := make(map[string]time.Time, len(ids))
	for _, id := range ids {
		var doc struct {
			CreatedAt time.Time `json:"createdAt"`
		}
		err := index.GetDocumentWithContext(ctx, id, &meilisearch.DocumentQuery{Fields: []string{"createdAt"}}, &doc)
		if err != nil {
			var merr *meilisearch.Error
			if errors.As(err, &merr) && merr.StatusCode == http.StatusNotFound {
				continue
			}
			return nil, fmt.Errorf("get %s/%s: %w", indexUID, id, err)
		}
		existing[id] = doc.CreatedAt
	}
	return existing, nil
}

// earliestCreatedAt picks the createdAt to store for id: the indexed one when
// it is earlier than the incoming one. A missing or zero indexed value (a
// document written before createdAt was kept) yields the incoming one.
func earliestCreatedAt(existing map[string]time.Time, id string, incoming time.Time) time.Time {
	if at, ok := existing[id]; ok && !at.IsZero() && at.Before(incoming) {
		return at
	}
	return incoming
}

// UpdateUserActivity merges activity fields into existing user documents.
// Callers pass only documents that are in the index (see the users mirror):
// a merge into an unknown id would create a document holding nothing but
// activity.
func (s *Store) UpdateUserActivity(ctx context.Context, docs []UserActivityDocument) error {
	if len(docs) == 0 {
		return nil
	}
	defer prometheus.NewTimer(meiliWriteDuration.WithLabelValues("activity")).ObserveDuration()
	index := s.client.Index(UsersIndex)
	task, err := index.UpdateDocumentsWithContext(ctx, docs, "id")
	return s.waitTask(ctx, task, err)
}

func (s *Store) UpsertPosts(ctx context.Context, docs []normalize.PostDocument) error {
	if len(docs) == 0 {
		return nil
	}
	defer prometheus.NewTimer(meiliWriteDuration.WithLabelValues("posts")).ObserveDuration()
	index := s.client.Index(PostsIndex)
	task, err := index.AddDocumentsWithContext(ctx, docs, "id")
	return s.waitTask(ctx, task, err)
}

// DeleteRecords applies a delete commit to one index. Each task is awaited so
// a failed delete surfaces before the replication cursor moves past it.
func (s *Store) DeleteRecords(ctx context.Context, indexUID string, spec DeleteSpec) error {
	defer prometheus.NewTimer(meiliWriteDuration.WithLabelValues("delete")).ObserveDuration()
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

// FetchDocuments returns the documents of an index matching a filter, without
// ranking or a query, up to limit. Used to load a page of documents by key
// (see InFilter) once their order was decided elsewhere.
func (s *Store) FetchDocuments(ctx context.Context, indexUID string, filter string, limit int64) ([]map[string]any, error) {
	var result meilisearch.DocumentsResult
	if err := s.client.Index(indexUID).GetDocumentsWithContext(ctx, &meilisearch.DocumentsQuery{Filter: filter, Limit: limit}, &result); err != nil {
		return nil, err
	}
	return result.Results, nil
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

// InFilter builds a `field IN ["a", "b"]` filter over a filterable attribute.
// An empty list yields an empty filter (which would match everything), so
// callers must handle that case before searching.
func InFilter(field string, values []string) string {
	if len(values) == 0 {
		return ""
	}
	quoted := make([]string, len(values))
	for i, value := range values {
		quoted[i] = "\"" + escapeFilterValue(value) + "\""
	}
	return field + " IN [" + strings.Join(quoted, ", ") + "]"
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
