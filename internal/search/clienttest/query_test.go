package clienttest

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/concrnt/concrnt"
	"github.com/concrnt/concrnt/client"
)

func TestClientQueryUsesDiscoveredEndpoint(t *testing.T) {
	const domain = "seed.test"
	const schema = "https://schema.concrnt.world/p/main.json"
	createdAt := time.Date(2026, 5, 15, 1, 2, 3, 0, time.UTC)
	signed := signedDocument(t, schema, createdAt)
	next := createdAt.Add(time.Minute)

	cl := client.New(domain)
	cl.GetClient().Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/.well-known/concrnt":
			return jsonResponse(t, concrnt.WellKnownConcrnt{
				Version: "2.0",
				Domain:  domain,
				CSID:    "ccs012345678901234567890123456789012345678",
				Layer:   "concrnt",
				Endpoints: map[string]string{
					"net.concrnt.core.query": "/query{?prefix,schema,since,until,limit,order,parent}",
				},
			})
		case "/query":
			if r.URL.Query().Get("prefix") != "cckv://" {
				t.Errorf("prefix query mismatch: %s", r.URL.RawQuery)
			}
			if r.URL.Query().Get("schema") != schema {
				t.Errorf("schema query mismatch: %s", r.URL.RawQuery)
			}
			if r.URL.Query().Get("limit") != "100" || r.URL.Query().Get("order") != "asc" {
				t.Errorf("pagination query mismatch: %s", r.URL.RawQuery)
			}
			return jsonResponse(t, concrnt.QueryResult{Items: []concrnt.SignedDocument{signed}, Prev: &createdAt, Next: &next})
		default:
			return &http.Response{
				StatusCode: http.StatusNotFound,
				Body:       io.NopCloser(strings.NewReader("not found")),
				Header:     make(http.Header),
			}, nil
		}
	})
	got, err := cl.Query(t.Context(), domain, client.QueryParams{
		Prefix: "cckv://",
		Schema: schema,
		Limit:  100,
		Order:  "asc",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 1 || got.Items[0].Document != signed.Document {
		t.Fatalf("unexpected query results: %+v", got)
	}
	if got.Next == nil || !got.Next.Equal(next) {
		t.Fatalf("next cursor mismatch: %v", got.Next)
	}
}

func TestClientQueryRejectsPrefixAndParent(t *testing.T) {
	cl := client.New("seed.test")
	_, err := cl.Query(t.Context(), "seed.test", client.QueryParams{
		Prefix: "cckv://",
		Parent: "cckv://parent",
	})
	if err == nil {
		t.Fatal("expected prefix+parent validation error")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func signedDocument(t *testing.T, schema string, createdAt time.Time) concrnt.SignedDocument {
	t.Helper()
	doc := concrnt.Document[map[string]string]{
		Key:       "cckv://con012345678901234567890123456789012345678/profile/main",
		Value:     map[string]string{"username": "alice"},
		Author:    "con012345678901234567890123456789012345678",
		Schema:    schema,
		CreatedAt: createdAt,
	}
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return concrnt.SignedDocument{
		Document: string(body),
		Proof:    concrnt.Proof{Type: concrnt.ProofTypeNone},
	}
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
