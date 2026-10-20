package api

import (
	"strings"

	"github.com/concrnt/concrnt-crawler/internal/search/meili"
)

// postQuery is the search syntax of the posts endpoint split into the free
// text handed to Meilisearch and the field terms turned into filters.
//
// A term is a whitespace-separated token starting with `author:` or `in:`
// (key case-insensitive, split at the first ASCII colon). Values are CCIDs
// and timeline keys, which never contain whitespace, so there is no quoting:
// anything else, including an empty value, stays in the text.
type postQuery struct {
	Text      string   // remaining tokens, single-space joined
	Authors   []string // author:<ccid>, deduplicated, first-seen order
	Timelines []string // in:<timeline cckv>, deduplicated, first-seen order
}

func parsePostQuery(q string) postQuery {
	parsed := postQuery{Authors: []string{}, Timelines: []string{}}
	seenAuthors := map[string]bool{}
	seenTimelines := map[string]bool{}
	text := []string{}
	for _, token := range strings.Fields(q) {
		key, value, ok := strings.Cut(token, ":")
		switch {
		case !ok || value == "":
			text = append(text, token)
		case strings.EqualFold(key, "author"):
			if !seenAuthors[value] {
				seenAuthors[value] = true
				parsed.Authors = append(parsed.Authors, value)
			}
		case strings.EqualFold(key, "in"):
			if !seenTimelines[value] {
				seenTimelines[value] = true
				parsed.Timelines = append(parsed.Timelines, value)
			}
		default:
			text = append(text, token)
		}
	}
	parsed.Text = strings.Join(text, " ")
	return parsed
}

// filter is the Meilisearch filter for the field terms: values of one field
// are ORed (IN), the fields are ANDed.
func (p postQuery) filter() string {
	return meili.JoinFilters(meili.InFilter("author", p.Authors), meili.InFilter("timelines", p.Timelines))
}

// echo is the `filters` object of the response, so a client can show what
// was filtered on without re-implementing the syntax. Never null.
func (p postQuery) echo() map[string]any {
	return map[string]any{"author": p.Authors, "in": p.Timelines}
}
