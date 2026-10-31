package crawler

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/concrnt/concrnt"
	"github.com/concrnt/concrnt-crawler/internal/search/config"
	"github.com/concrnt/concrnt-crawler/internal/search/meili"
	"github.com/concrnt/concrnt-crawler/internal/search/model"
	"github.com/concrnt/concrnt-crawler/internal/search/normalize"
	"github.com/concrnt/concrnt-crawler/internal/search/sigtest"
	"github.com/concrnt/concrnt/client"
)

func TestCrawlCCFSIndexesProfile(t *testing.T) {
	const domain = "manual.test"
	const ccfs = "ccfs://manual.test/bafyexample"

	sd := testSignedDocument(t, time.Date(2026, 5, 15, 1, 0, 0, 0, time.UTC))
	store := &manualStore{}
	cl := client.New(domain)
	cl.GetClient().Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/.well-known/concrnt":
			return jsonResponse(t, concrnt.WellKnownConcrnt{
				Domain: domain,
				Endpoints: map[string]string{
					"net.concrnt.core.resolve": "/resolve?uri={uri}",
				},
			})
		case "/resolve":
			uri, err := url.QueryUnescape(r.URL.Query().Get("uri"))
			if err != nil {
				t.Fatal(err)
			}
			if uri != ccfs {
				t.Fatalf("resolve uri mismatch: %s", r.URL.RawQuery)
			}
			return jsonResponse(t, sd)
		default:
			return &http.Response{
				StatusCode: http.StatusNotFound,
				Body:       io.NopCloser(strings.NewReader("not found")),
				Header:     make(http.Header),
			}, nil
		}
	})

	c := New(newTestDB(t), store, cl, config.Default().Crawl, nil)
	result, err := c.CrawlCCFS(context.Background(), ccfs)
	if err != nil {
		t.Fatal(err)
	}
	if result.Kind != KindProfile || result.CCFS != ccfs || result.SourceServer != domain {
		t.Fatalf("unexpected result: %+v", result)
	}
	if len(store.users) != 1 {
		t.Fatalf("expected one user upsert, got %d", len(store.users))
	}
	if store.users[0].CCFS != ccfs {
		t.Fatalf("ccfs mismatch: %s", store.users[0].CCFS)
	}
	var mirror []model.IndexedUser
	if err := c.db.Find(&mirror).Error; err != nil {
		t.Fatal(err)
	}
	if len(mirror) != 1 || mirror[0].CCKV != store.users[0].CCKV || mirror[0].CCID != store.users[0].CCID {
		t.Fatalf("manual crawl should mirror the profile, got %+v", mirror)
	}
}

func TestCrawlCCFSIndexesPost(t *testing.T) {
	const domain = "manual.test"

	sd := testPostDocument(t, postsParent+"/p1", "hello world", time.Date(2026, 5, 15, 1, 0, 0, 0, time.UTC))
	// a domain-owned ccfs resolves to its host without an entity lookup
	ccfs := "ccfs://manual.test/concrnt/p1"
	sd.CCFS = &ccfs
	store := &manualStore{}
	cl := client.New(domain)
	cl.GetClient().Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/.well-known/concrnt":
			return jsonResponse(t, concrnt.WellKnownConcrnt{
				Domain: domain,
				Endpoints: map[string]string{
					"net.concrnt.core.resolve": "/resolve?uri={uri}",
				},
			})
		case "/resolve":
			return jsonResponse(t, sd)
		default:
			return &http.Response{
				StatusCode: http.StatusNotFound,
				Body:       io.NopCloser(strings.NewReader("not found")),
				Header:     make(http.Header),
			}, nil
		}
	})

	c := New(newTestDB(t), store, cl, config.Default().Crawl, nil)
	result, err := c.CrawlCCFS(context.Background(), ccfs)
	if err != nil {
		t.Fatal(err)
	}
	if result.Kind != KindPost || result.CCFS != ccfs || result.SourceServer != domain {
		t.Fatalf("unexpected result: %+v", result)
	}
	if len(store.posts) != 1 || store.posts[0].Body != "hello world" {
		t.Fatalf("expected one post upsert with body, got %+v", store.posts)
	}
	if store.posts[0].Timelines == nil {
		t.Fatalf("timelines must never be null in the index: %+v", store.posts[0])
	}
}

func TestCrawlCCFSRejectsUnverifiedDocuments(t *testing.T) {
	const domain = "manual.test"
	const ccfs = "ccfs://manual.test/bafyexample"

	cases := map[string]concrnt.SignedDocument{
		"none proof": sigtest.NoneProof(t, profileDocument(testAuthor, testAuthor, "alice")),
		"tampered":   sigtest.Tamper(t, testSignedDocument(t, t0), `"username":"alice"`, `"username":"mallory"`),
		"impostor":   bob.Sign(t, profileDocument(otherAuthor, testAuthor, "impostor")),
	}
	for name, sd := range cases {
		t.Run(name, func(t *testing.T) {
			store := &manualStore{}
			cl := client.New(domain)
			cl.GetClient().Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				switch r.URL.Path {
				case "/.well-known/concrnt":
					return jsonResponse(t, concrnt.WellKnownConcrnt{Domain: domain, Endpoints: map[string]string{resolveEndpoint: "/resolve?uri={uri}"}})
				case "/resolve":
					return jsonResponse(t, sd)
				default:
					return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader("not found")), Header: make(http.Header)}, nil
				}
			})
			c := New(newTestDB(t), store, cl, config.Default().Crawl, nil)
			_, err := c.CrawlCCFS(context.Background(), ccfs)
			if err == nil || !strings.Contains(err.Error(), "signature verification failed") {
				t.Fatalf("expected a verification error, got %v", err)
			}
			if len(store.calls) != 0 {
				t.Fatalf("an unverified document must not reach the store: %v", store.calls)
			}
		})
	}
}

// manualStore records every store call in order so tests can assert that
// deletes are applied after the upserts that precede them in the log.
type manualStore struct {
	users        []normalize.UserDocument
	communities  []normalize.CommunityDocument
	posts        []normalize.PostDocument
	deletes      []meili.DeleteSpec
	activity     []meili.CommunityActivityDocument
	userActivity []meili.UserActivityDocument
	calls        []string
}

func (s *manualStore) UpsertServers(context.Context, []meili.ServerDocument) error {
	return nil
}

func (s *manualStore) UpsertUsers(_ context.Context, docs []normalize.UserDocument) error {
	s.users = append(s.users, docs...)
	s.calls = append(s.calls, "users")
	return nil
}

func (s *manualStore) UpsertCommunities(_ context.Context, docs []normalize.CommunityDocument) error {
	s.communities = append(s.communities, docs...)
	s.calls = append(s.calls, "communities")
	return nil
}

func (s *manualStore) UpsertPosts(_ context.Context, docs []normalize.PostDocument) error {
	s.posts = append(s.posts, docs...)
	s.calls = append(s.calls, "posts")
	return nil
}

func (s *manualStore) DeleteRecords(_ context.Context, indexUID string, spec meili.DeleteSpec) error {
	s.deletes = append(s.deletes, spec)
	s.calls = append(s.calls, "delete:"+indexUID)
	return nil
}

func (s *manualStore) UpdateCommunityActivity(_ context.Context, docs []meili.CommunityActivityDocument) error {
	s.activity = append(s.activity, docs...)
	s.calls = append(s.calls, "activity")
	return nil
}

func (s *manualStore) UpdateUserActivity(_ context.Context, docs []meili.UserActivityDocument) error {
	s.userActivity = append(s.userActivity, docs...)
	s.calls = append(s.calls, "userActivity")
	return nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func jsonResponse(t *testing.T, value any) (*http.Response, error) {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(string(body))),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}, nil
}
