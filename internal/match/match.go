// Package match finds an instance from what the user types: an exact name, part of a
// name, several words ("shop app prod"), an instance ID or an IP.
package match

import (
	"fmt"
	"net"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/morganseznec/hop/internal/inventory"
)

type Result struct {
	// Exact: the name, ID or IP matches as is. Several exact results mean the same
	// name in several accounts, or an Auto Scaling group.
	Exact bool
	// Fuzzy: no word matches as is, so the results come from the typo-tolerant
	// search. They must never trigger a connection without confirmation: a typo would
	// open a session on the wrong machine.
	Fuzzy bool
	// Literal is set when the query is an IP address or an instance ID ("IP address"
	// or "ID"). Only an exact match counts then: "10.0.1.2" must never find 10.0.1.21.
	Literal string
	Matches []inventory.Instance
	// Close names when nothing matches, for "did you mean". For an IP or an ID, the
	// addresses or IDs that start with the query, each with its instance name.
	Suggestions []string
}

// instanceID is an EC2 instance ID (i-) or a hybrid managed node ID (mi-).
var instanceID = regexp.MustCompile(`(?i)^(i|mi)-[0-9a-f]{8,17}$`)

// literal returns what kind of identifier q is, or "" for words.
func literal(q string) string {
	switch {
	case net.ParseIP(q) != nil:
		return "IP address"
	case instanceID.MatchString(q):
		return "ID"
	}
	return ""
}

// Search ranks the instances against the query. recents gives, by Instance.Key(), the
// time of the last connection: at equal score, the machine used yesterday comes first.
// label returns the display label of a profile, which is also part of the searched text.
func Search(items []inventory.Instance, query string, recents map[string]time.Time, label func(string) string) Result {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return Result{Matches: byRecency(items, recents)}
	}

	var exact []inventory.Instance
	for _, in := range items {
		if isExact(in, q) {
			exact = append(exact, in)
		}
	}
	if len(exact) > 0 {
		return Result{Exact: true, Literal: literal(q), Matches: byRecency(exact, recents)}
	}
	if kind := literal(q); kind != "" {
		return Result{Literal: kind, Suggestions: suggestLiteral(items, q, kind)}
	}

	terms := strings.Fields(q)
	type scored struct {
		in    inventory.Instance
		score int
	}
	var hits []scored
	for _, in := range items {
		name := strings.ToLower(in.Name)
		hay := strings.ToLower(strings.Join([]string{in.Name, in.Profile, in.Region, in.Env, in.ID, in.PrivateIP, label(in.Profile)}, " "))
		score, all := 0, true
		for _, term := range terms {
			switch {
			case strings.HasPrefix(name, term):
				score += 15
			case containsToken(name, term):
				score += 12
			case strings.Contains(name, term):
				score += 8
			case strings.Contains(hay, term):
				score += 3
			default:
				all = false
			}
			if !all {
				break
			}
		}
		if all {
			hits = append(hits, scored{in, score + bonus(in, recents)})
		}
	}

	// Nothing matched by words: try a subsequence of the name ("shoptools" finds
	// acme-p-shop-tools).
	fuzzy := len(hits) == 0
	if fuzzy {
		compact := strings.ReplaceAll(q, " ", "")
		for _, in := range items {
			if gap, ok := subsequence(strings.ToLower(in.Name), compact); ok {
				hits = append(hits, scored{in, 5 - gap/4 + bonus(in, recents)})
			}
		}
	}

	if len(hits) == 0 {
		return Result{Suggestions: suggest(items, q)}
	}
	sort.SliceStable(hits, func(a, b int) bool { return hits[a].score > hits[b].score })
	out := make([]inventory.Instance, len(hits))
	for i, h := range hits {
		out[i] = h.in
	}
	return Result{Matches: out, Fuzzy: fuzzy}
}

func isExact(in inventory.Instance, q string) bool {
	return strings.EqualFold(in.ID, q) || (in.PrivateIP != "" && strings.EqualFold(in.PrivateIP, q)) || strings.EqualFold(in.Name, q)
}

// Strong reports whether in matches the query well enough to act on it without asking:
// the exact name, ID or IP, or every word of the query a whole word of the name ("shop
// app" for acme-p-shop-app). Part of a word, a profile, a region, an environment, a
// label, or part of an ID or an IP is never enough. A hybrid node (mi-) reports its own
// name and IP to SSM: only its ID, which SSM assigns, is enough.
func Strong(in inventory.Instance, query string) bool {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return false
	}
	if in.Hybrid() {
		return strings.EqualFold(in.ID, q)
	}
	if isExact(in, q) {
		return true
	}
	name := strings.ToLower(in.Name)
	for _, term := range strings.Fields(q) {
		if !containsToken(name, term) {
			return false
		}
	}
	return true
}

func tokens(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == '-' || r == '_' || r == '.' })
}

// containsToken reports whether term is a whole word of name ("app" in
// acme-p-shop-app), or a run of whole words ("shop-app").
func containsToken(name, term string) bool {
	words, want := tokens(name), tokens(term)
	if len(want) == 0 {
		return false
	}
	for i := 0; i+len(want) <= len(words); i++ {
		if slices.Equal(words[i:i+len(want)], want) {
			return true
		}
	}
	return false
}

// suggestLiteral returns up to three IPs or IDs that start with q, for a partial
// address ("10.0.1.2" suggests 10.0.1.21) or a truncated ID.
func suggestLiteral(items []inventory.Instance, q, kind string) []string {
	var out []string
	seen := map[string]bool{}
	for _, in := range items {
		v := in.PrivateIP
		if kind == "ID" {
			v = in.ID
		}
		s := fmt.Sprintf("%s (%s)", v, in.Name)
		if v == "" || !strings.HasPrefix(strings.ToLower(v), q) || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	if len(out) > 3 {
		out = out[:3]
	}
	return out
}

func bonus(in inventory.Instance, recents map[string]time.Time) int {
	b := 0
	if in.Running() {
		b += 2
	}
	if t, ok := recents[in.Key()]; ok && time.Since(t) < 7*24*time.Hour {
		b += 3
	}
	return b
}

func byRecency(items []inventory.Instance, recents map[string]time.Time) []inventory.Instance {
	out := append([]inventory.Instance(nil), items...)
	sort.SliceStable(out, func(a, b int) bool {
		return recents[out[a].Key()].After(recents[out[b].Key()])
	})
	return out
}

// subsequence reports whether q appears in s in order, and returns the total gap
// between the letters found: the smaller it is, the tighter the match.
func subsequence(s, q string) (gap int, ok bool) {
	i, last := 0, -1
	for j := 0; j < len(s) && i < len(q); j++ {
		if s[j] == q[i] {
			if last >= 0 {
				gap += j - last - 1
			}
			last = j
			i++
		}
	}
	return gap, i == len(q)
}

// suggest returns the three closest names. The distance is measured between the query
// and the best-matching part of the name: "tols" suggests acme-p-tools without paying
// for the seven letters of "acme-p-".
func suggest(items []inventory.Instance, q string) []string {
	if len(q) < 3 {
		return nil
	}
	type cand struct {
		name string
		d    int
	}
	best := map[string]int{}
	for _, in := range items {
		d := substringDistance(strings.ToLower(in.Name), q)
		if old, ok := best[in.Name]; !ok || d < old {
			best[in.Name] = d
		}
	}
	limit := max(1, len(q)/4)
	var cs []cand
	for n, d := range best {
		if d <= limit {
			cs = append(cs, cand{n, d})
		}
	}
	sort.Slice(cs, func(a, b int) bool {
		if cs[a].d != cs[b].d {
			return cs[a].d < cs[b].d
		}
		return cs[a].name < cs[b].name
	})
	var out []string
	for i := 0; i < len(cs) && i < 3; i++ {
		out = append(out, cs[i].name)
	}
	return out
}

// substringDistance is the edit distance between pattern and the part of text that
// resembles it most: skipping the start and the end of text is free.
func substringDistance(text, pattern string) int {
	t, p := []rune(text), []rune(pattern)
	// prev[j]: cost of aligning p[:i] and ending at position j of text.
	prev := make([]int, len(t)+1)
	cur := make([]int, len(t)+1)
	for i := 1; i <= len(p); i++ {
		cur[0] = i
		for j := 1; j <= len(t); j++ {
			cost := 1
			if p[i-1] == t[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	best := len(p)
	for _, v := range prev {
		best = min(best, v)
	}
	return best
}
