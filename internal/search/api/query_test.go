package api

import (
	"strings"
	"testing"
)

func TestParsePostQuery(t *testing.T) {
	cases := []struct {
		name      string
		q         string
		text      string
		authors   []string
		timelines []string
		filter    string
	}{
		{name: "plain text", q: "hello world", text: "hello world"},
		{name: "author only", q: "author:con1", authors: []string{"con1"}, filter: `author IN ["con1"]`},
		{
			name:      "mixed terms and text",
			q:         "hello author:con1 in:cckv://a in:cckv://b world",
			text:      "hello world",
			authors:   []string{"con1"},
			timelines: []string{"cckv://a", "cckv://b"},
			filter:    `author IN ["con1"] AND timelines IN ["cckv://a", "cckv://b"]`,
		},
		{name: "keys are case-insensitive, values verbatim", q: "Author:con1 IN:cckv://A", authors: []string{"con1"}, timelines: []string{"cckv://A"}, filter: `author IN ["con1"] AND timelines IN ["cckv://A"]`},
		{name: "duplicates collapse, first-seen order", q: "author:con1 author:con1 author:con2", authors: []string{"con1", "con2"}, filter: `author IN ["con1", "con2"]`},
		{name: "empty values stay text", q: "author: in:", text: "author: in:"},
		{name: "whitespace runs collapse", q: "  hello\t\nauthor:con1  ", text: "hello", authors: []string{"con1"}, filter: `author IN ["con1"]`},
		{name: "glued and full-width forms stay text", q: "(author:con1) foo,in:cckv://a author：con1", text: "(author:con1) foo,in:cckv://a author：con1"},
		{name: "quotes are part of the value", q: `in:"cckv://a"`, timelines: []string{`"cckv://a"`}, filter: `timelines IN ["\"cckv://a\""]`},
		{name: "later colons belong to the value", q: "in:cckv://host/a:b", timelines: []string{"cckv://host/a:b"}, filter: `timelines IN ["cckv://host/a:b"]`},
		{name: "unknown keys stay text", q: "from:con1 hello", text: "from:con1 hello"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parsePostQuery(tc.q)
			if got.Text != tc.text {
				t.Fatalf("text = %q, want %q", got.Text, tc.text)
			}
			if got.Authors == nil || got.Timelines == nil {
				t.Fatalf("slices must never be nil: %+v", got)
			}
			if strings.Join(got.Authors, ",") != strings.Join(tc.authors, ",") {
				t.Fatalf("authors = %v, want %v", got.Authors, tc.authors)
			}
			if strings.Join(got.Timelines, ",") != strings.Join(tc.timelines, ",") {
				t.Fatalf("timelines = %v, want %v", got.Timelines, tc.timelines)
			}
			if got.filter() != tc.filter {
				t.Fatalf("filter = %q, want %q", got.filter(), tc.filter)
			}
		})
	}
}
