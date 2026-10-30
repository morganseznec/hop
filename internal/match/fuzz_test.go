package match

import (
	"strings"
	"testing"
)

// FuzzSearch runs arbitrary queries against a fixed fleet. Search must never panic or
// return an instance twice, and an IP address or instance ID must only ever match
// exactly: a partial one must not pick another host.
func FuzzSearch(f *testing.F) {
	for _, seed := range []string{
		"", "  ", "acme-p-tools", "shop app", "ACME proxy", "tols", "shop-tols",
		"10.0.0.1", "10.0.0", "I-05", "i-0123456789abcdef0", "mi-0abcdef12345678",
		"acme-prod us-east", "é", "\x00", "a b c d e f g",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, query string) {
		fl := fleet()
		r := Search(fl, query, nil, noLabel)
		if len(r.Matches) > len(fl) {
			t.Fatalf("%d matches for %d instances", len(r.Matches), len(fl))
		}
		seen := map[string]bool{}
		for _, m := range r.Matches {
			if seen[m.Key()] {
				t.Fatalf("%q: %s returned twice", query, m.Key())
			}
			seen[m.Key()] = true
		}
		q := strings.ToLower(strings.TrimSpace(query))
		if literal(q) == "" {
			return
		}
		for _, m := range r.Matches {
			if !isExact(m, q) {
				t.Fatalf("literal query %q matched %s (%s, %s) without an exact match", query, m.Name, m.ID, m.PrivateIP)
			}
		}
	})
}
