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
)

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
			filterable: []string{"owner", "sourceServer", "schema"},
			sortable:   []string{"createdAt", "indexedAt", "name"},
		},
		{
			uid:        UsersIndex,
			searchable: []string{"username", "description", "ccid", "owner", "cckv", "sourceServer"},
			filterable: []string{"ccid", "owner", "sourceServer", "schema"},
			sortable:   []string{"createdAt", "indexedAt", "username"},
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

func (s *Store) UpsertCommunities(ctx context.Context, docs []normalize.CommunityDocument) error {
	if len(docs) == 0 {
		return nil
	}
	index := s.client.Index(CommunitiesIndex)
	task, err := index.AddDocumentsWithContext(ctx, docs, "id")
	return s.waitTask(ctx, task, err)
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

func escapeFilterValue(value string) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, "\"", "\\\"")
	return value
}
