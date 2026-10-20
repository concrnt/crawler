package api

import (
	"strings"

	"github.com/concrnt/concrnt-crawler/internal/search/meili"
)

// searchTerm is one `key:` prefix of the search syntax and the index
// attribute it filters on.
type searchTerm struct {
	Key   string // the prefix in q, e.g. "author"
	Field string // the filterable attribute, e.g. "ccid"
}

// Terms accepted per endpoint. A key that an endpoint does not list stays in
// the text query, so `in:` on users is just a word.
var (
	postTerms      = []searchTerm{{Key: "author", Field: "author"}, {Key: "in", Field: "timelines"}}
	userTerms      = []searchTerm{{Key: "author", Field: "ccid"}} // a profile's author is its owner, the ccid
	communityTerms = []searchTerm{{Key: "author", Field: "author"}}
)

// searchQuery is the search syntax of an endpoint split into the free text
// handed to Meilisearch and the field terms turned into filters.
//
// A term is a whitespace-separated token starting with `<key>:` (key
// case-insensitive, split at the first ASCII colon). Values are CCIDs and
// timeline keys, which never contain whitespace, so there is no quoting:
// anything else, including an empty value, stays in the text.
type searchQuery struct {
	Text   string              // remaining tokens, single-space joined
	terms  []searchTerm        // accepted terms, in filter order
	values map[string][]string // by key: deduplicated values, first-seen order
}

func parseSearchQuery(q string, terms []searchTerm) searchQuery {
	parsed := searchQuery{terms: terms, values: map[string][]string{}}
	for _, term := range terms {
		parsed.values[term.Key] = []string{}
	}
	seen := map[string]map[string]bool{}
	text := []string{}
	for _, token := range strings.Fields(q) {
		key, value, ok := strings.Cut(token, ":")
		matched := false
		if ok && value != "" {
			for _, term := range terms {
				if !strings.EqualFold(key, term.Key) {
					continue
				}
				matched = true
				if seen[term.Key] == nil {
					seen[term.Key] = map[string]bool{}
				}
				if !seen[term.Key][value] {
					seen[term.Key][value] = true
					parsed.values[term.Key] = append(parsed.values[term.Key], value)
				}
				break
			}
		}
		if !matched {
			text = append(text, token)
		}
	}
	parsed.Text = strings.Join(text, " ")
	return parsed
}

// get returns the values of one term key (never nil for an accepted key).
func (p searchQuery) get(key string) []string {
	return p.values[key]
}

// filter is the Meilisearch filter for the terms: values of one term are
// ORed (IN), the terms are ANDed, in the endpoint's term order.
func (p searchQuery) filter() string {
	parts := make([]string, 0, len(p.terms))
	for _, term := range p.terms {
		parts = append(parts, meili.InFilter(term.Field, p.values[term.Key]))
	}
	return meili.JoinFilters(parts...)
}

// echo is the `filters` object of the response, so a client can show what
// was filtered on without re-implementing the syntax. Every accepted key is
// present, never null.
func (p searchQuery) echo() map[string]any {
	out := make(map[string]any, len(p.terms))
	for _, term := range p.terms {
		out[term.Key] = p.values[term.Key]
	}
	return out
}
