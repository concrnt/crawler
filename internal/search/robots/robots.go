// Package robots parses robots.txt and answers whether a product token may
// fetch a path, following the Robots Exclusion Protocol (RFC 9309).
package robots

import (
	"bufio"
	"io"
	"strings"
)

// MaxSize is how much of a robots.txt a crawler must parse at least
// (RFC 9309 §2.5): bytes past it are ignored.
const MaxSize = 500 * 1024

// Robots is a parsed robots.txt.
type Robots struct {
	groups []group
}

// group is one block of user-agent lines followed by their rules.
type group struct {
	agents []string
	rules  []rule
}

type rule struct {
	allow   bool
	pattern string
}

// Parse reads at most MaxSize bytes of a robots.txt. It never fails: lines
// it cannot interpret are skipped, as the protocol requires.
func Parse(r io.Reader) *Robots {
	robots := &Robots{}
	scanner := bufio.NewScanner(io.LimitReader(r, MaxSize))
	scanner.Buffer(make([]byte, 0, 64*1024), MaxSize)
	var current *group
	// a user-agent line directly after another extends the same group; one
	// after a rule starts a new group (RFC 9309 §2.2.1)
	collectingAgents := false
	for scanner.Scan() {
		line := scanner.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		field, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		field = strings.ToLower(strings.TrimSpace(field))
		value = strings.TrimSpace(value)
		switch field {
		case "user-agent":
			if !collectingAgents {
				robots.groups = append(robots.groups, group{})
				current = &robots.groups[len(robots.groups)-1]
				collectingAgents = true
			}
			current.agents = append(current.agents, strings.ToLower(value))
		case "allow", "disallow":
			// rules before any user-agent line belong to no group
			if current == nil {
				continue
			}
			collectingAgents = false
			// an empty disallow allows everything and an empty allow says
			// nothing: neither needs a rule
			if value == "" {
				continue
			}
			current.rules = append(current.rules, rule{allow: field == "allow", pattern: value})
		default:
			// sitemap and unknown fields do not split a group
			if current != nil && collectingAgents && field != "" {
				collectingAgents = false
			}
		}
	}
	return robots
}

// Allowed reports whether the crawler identified by product (e.g.
// "concrnt-crawler") may fetch path. The rules of every group naming the
// product are used, else those of every "*" group, else everything is
// allowed. Among the matching rules the longest pattern wins, an allow on a
// tie (RFC 9309 §2.2.2).
func (r *Robots) Allowed(product string, path string) bool {
	rules := r.rulesFor(product)
	if path == "" {
		path = "/"
	}
	allowed := true
	longest := -1
	for _, rule := range rules {
		if !matchPattern(rule.pattern, path) {
			continue
		}
		n := len(rule.pattern)
		if n > longest || (n == longest && rule.allow) {
			longest = n
			allowed = rule.allow
		}
	}
	return allowed
}

func (r *Robots) rulesFor(product string) []rule {
	product = strings.ToLower(product)
	var specific, wildcard []rule
	found := false
	for _, g := range r.groups {
		named, star := false, false
		for _, agent := range g.agents {
			if agent == product || strings.HasPrefix(agent, product+"/") {
				named = true
			}
			if agent == "*" {
				star = true
			}
		}
		if named {
			// a group naming the product with no rules still means it is
			// allowed everywhere: it must not fall back to the "*" groups
			found = true
			specific = append(specific, g.rules...)
		} else if star {
			wildcard = append(wildcard, g.rules...)
		}
	}
	if found {
		return specific
	}
	return wildcard
}

// matchPattern matches a rule pattern against a path from the start, where
// "*" matches any run of characters and a trailing "$" anchors the end.
func matchPattern(pattern string, path string) bool {
	anchored := strings.HasSuffix(pattern, "$")
	if anchored {
		pattern = strings.TrimSuffix(pattern, "$")
	}
	parts := strings.Split(pattern, "*")
	if !strings.HasPrefix(path, parts[0]) {
		return false
	}
	rest := path[len(parts[0]):]
	for _, part := range parts[1:] {
		i := strings.Index(rest, part)
		if i < 0 {
			return false
		}
		rest = rest[i+len(part):]
	}
	if anchored {
		// with a wildcard the last part may sit anywhere; anchoring means it
		// must sit at the end
		if len(parts) == 1 {
			return rest == ""
		}
		return strings.HasSuffix(path, parts[len(parts)-1])
	}
	return true
}
