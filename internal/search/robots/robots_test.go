package robots

import (
	"strings"
	"testing"
)

func TestAllowedPicksTheGroupForTheProductToken(t *testing.T) {
	robots := Parse(strings.NewReader(`
# comment
User-agent: *
Disallow: /private

User-Agent: Concrnt-Crawler
Disallow: /api/v2/replication

User-agent: other-bot
Disallow: /
`))
	cases := []struct {
		product string
		path    string
		want    bool
	}{
		// the named group applies, not the wildcard one
		{"concrnt-crawler", "/api/v2/replication", false},
		{"concrnt-crawler", "/private", true},
		{"concrnt-crawler", "/", true},
		// unnamed crawlers get the wildcard group
		{"unknown-bot", "/private/x", false},
		{"unknown-bot", "/api/v2/replication", true},
		{"other-bot", "/", false},
	}
	for _, tc := range cases {
		if got := robots.Allowed(tc.product, tc.path); got != tc.want {
			t.Errorf("Allowed(%q, %q) = %v, want %v", tc.product, tc.path, got, tc.want)
		}
	}
}

func TestAllowedMergesGroupsAndSharesAgentLines(t *testing.T) {
	robots := Parse(strings.NewReader(`
User-agent: concrnt-crawler
User-agent: other-bot
Disallow: /a

User-agent: concrnt-crawler/1.0
Disallow: /b
Sitemap: https://example.com/sitemap.xml
Disallow: /c
`))
	for _, path := range []string{"/a", "/b", "/c"} {
		if robots.Allowed("concrnt-crawler", path) {
			t.Errorf("%s must be disallowed by the merged groups", path)
		}
	}
	if robots.Allowed("other-bot", "/a") || !robots.Allowed("other-bot", "/b") {
		t.Error("other-bot shares only the first group")
	}
}

func TestAllowedLongestMatchWinsAndAllowOnTie(t *testing.T) {
	robots := Parse(strings.NewReader(`
User-agent: *
Disallow: /api
Allow: /api/v2/replication
Disallow: /tie
Allow: /tie
`))
	if !robots.Allowed("x", "/api/v2/replication") {
		t.Error("the longer allow must beat the shorter disallow")
	}
	if robots.Allowed("x", "/api/v1/other") {
		t.Error("/api is disallowed")
	}
	if !robots.Allowed("x", "/tie") {
		t.Error("equal length rules resolve to allow")
	}
}

func TestAllowedWildcardsAndAnchors(t *testing.T) {
	robots := Parse(strings.NewReader(`
User-agent: *
Disallow: /*/replication
Disallow: /exact$
Disallow: /pre*fix$
`))
	cases := map[string]bool{
		"/api/v2/replication":      false,
		"/api/v2/replication/more": false,
		"/replication":             true,
		"/exact":                   false,
		"/exactly":                 true,
		"/pre-anything-fix":        false,
		"/pre-anything-fix/":       true,
		"/":                        true,
	}
	for path, want := range cases {
		if got := robots.Allowed("x", path); got != want {
			t.Errorf("Allowed(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestAllowedWithoutRules(t *testing.T) {
	cases := map[string]string{
		"empty file":                "",
		"rules before any group":    "Disallow: /\n",
		"other agents only":         "User-agent: other\nDisallow: /\n",
		"empty disallow":            "User-agent: *\nDisallow:\n",
		"named group with no rules": "User-agent: *\nDisallow: /\n\nUser-agent: concrnt-crawler\nDisallow:\n",
	}
	for name, body := range cases {
		if !Parse(strings.NewReader(body)).Allowed("concrnt-crawler", "/api/v2/replication") {
			t.Errorf("%s: must allow everything", name)
		}
	}
	if Parse(strings.NewReader("User-agent: *\nDisallow: /\n")).Allowed("concrnt-crawler", "/") {
		t.Error("Disallow: / blocks the root")
	}
}

func TestParseIgnoresBytesPastMaxSize(t *testing.T) {
	body := "User-agent: *\n" + strings.Repeat("# padding padding padding\n", MaxSize/20) + "Disallow: /\n"
	if !Parse(strings.NewReader(body)).Allowed("x", "/") {
		t.Error("a rule past MaxSize must be ignored")
	}
}
