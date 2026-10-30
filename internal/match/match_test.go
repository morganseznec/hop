package match

import (
	"strings"
	"testing"
	"time"

	"github.com/morganseznec/hop/internal/inventory"
)

func fleet() []inventory.Instance {
	mk := func(name, id, profile, region, env string) inventory.Instance {
		return inventory.Instance{ID: id, Name: name, Profile: profile, Region: region, Env: env, State: "running", PrivateIP: "10.0.0." + id[len(id)-1:]}
	}
	return []inventory.Instance{
		mk("acme-p-tools", "i-01", "acme-prod", "eu-west-1", "prod"),
		mk("acme-p-proxy", "i-02", "acme-prod", "eu-west-1", "prod"),
		mk("acme-t-app-v2", "i-03", "acme-prod", "us-east-1", "test"),
		mk("acme-p-shop-app", "i-04", "acme-shop", "eu-west-1", "prod"),
		mk("acme-p-shop-tools", "i-05", "acme-shop", "eu-west-1", "prod"),
		mk("acme-p-shop-retail-app", "i-06", "acme-shop-retail", "eu-west-1", "prod"),
		mk("tools", "i-07", "globex", "af-south-1", ""),
		mk("tools", "i-08", "initech", "eu-west-1", ""),
	}
}

func noLabel(string) string { return "" }

func names(r Result) []string {
	var out []string
	for _, m := range r.Matches {
		out = append(out, m.Name)
	}
	return out
}

func TestExactName(t *testing.T) {
	r := Search(fleet(), "acme-p-tools", nil, noLabel)
	if !r.Exact || len(r.Matches) != 1 || r.Matches[0].ID != "i-01" {
		t.Errorf("%+v", r)
	}
}

func TestSameNameInTwoAccounts(t *testing.T) {
	r := Search(fleet(), "tools", nil, noLabel)
	if !r.Exact || len(r.Matches) != 2 {
		t.Errorf("two accounts have an instance named \"tools\": %v", names(r))
	}
}

func TestIDAndIP(t *testing.T) {
	if r := Search(fleet(), "I-05", nil, noLabel); len(r.Matches) != 1 || r.Matches[0].Name != "acme-p-shop-tools" {
		t.Errorf("by ID: %v", names(r))
	}
	if r := Search(fleet(), "10.0.0.3", nil, noLabel); len(r.Matches) != 1 || r.Matches[0].ID != "i-03" {
		t.Errorf("by IP: %v", names(r))
	}
}

func TestTerms(t *testing.T) {
	r := Search(fleet(), "shop app", nil, noLabel)
	if len(r.Matches) != 2 || r.Matches[0].Name != "acme-p-shop-app" {
		t.Errorf("shop app: %v", names(r))
	}
	// A term can target the region or the profile.
	r = Search(fleet(), "acme-prod us-east", nil, noLabel)
	if len(r.Matches) != 1 || r.Matches[0].ID != "i-03" {
		t.Errorf("acme-prod us-east: %v", names(r))
	}
	r = Search(fleet(), "test", nil, noLabel)
	if len(r.Matches) != 1 || r.Matches[0].ID != "i-03" {
		t.Errorf("test: %v", names(r))
	}
}

func TestRecentFirst(t *testing.T) {
	f := fleet()
	recents := map[string]time.Time{f[4].Key(): time.Now().Add(-time.Hour)}
	r := Search(f, "shop", recents, noLabel)
	if r.Matches[0].ID != "i-05" {
		t.Errorf("%v", names(r))
	}
	r = Search(f, "", recents, noLabel)
	if r.Matches[0].ID != "i-05" {
		t.Errorf("without a query, the most recent comes first: %v", names(r))
	}
}

func TestSubsequence(t *testing.T) {
	r := Search(fleet(), "shopretail", nil, noLabel)
	if len(r.Matches) == 0 || r.Matches[0].ID != "i-06" || !r.Fuzzy {
		t.Errorf("subsequence: %v fuzzy=%v", names(r), r.Fuzzy)
	}
	// A typo finds a candidate, but it is flagged as approximate.
	r = Search(fleet(), "shop-tols", nil, noLabel)
	if len(r.Matches) != 1 || !r.Fuzzy {
		t.Errorf("typo: %v fuzzy=%v", names(r), r.Fuzzy)
	}
	if r := Search(fleet(), "shop app", nil, noLabel); r.Fuzzy {
		t.Error("a word search must not be fuzzy")
	}
}

func TestSuggestions(t *testing.T) {
	r := Search(fleet(), "acme-o-toolz", nil, noLabel)
	if len(r.Matches) != 0 || len(r.Suggestions) == 0 {
		t.Fatalf("%+v", r)
	}
	if r.Suggestions[0] != "acme-p-tools" {
		t.Errorf("suggestions: %v", r.Suggestions)
	}
}

func TestLabel(t *testing.T) {
	label := func(p string) string {
		if p == "acme-prod" {
			return "ACME"
		}
		return ""
	}
	r := Search(fleet(), "ACME proxy", nil, label)
	if len(r.Matches) != 1 || r.Matches[0].ID != "i-02" {
		t.Errorf("%v", names(r))
	}
}

func TestEmptyQuery(t *testing.T) {
	f := fleet()
	for _, q := range []string{"", "   ", "\t"} {
		r := Search(f, q, nil, noLabel)
		if r.Exact || r.Fuzzy || len(r.Matches) != len(f) || len(r.Suggestions) != 0 {
			t.Errorf("%q: %+v", q, r)
		}
		// Without recents, the original order is kept.
		for i := range f {
			if r.Matches[i].ID != f[i].ID {
				t.Errorf("%q: order changed at %d: %v", q, i, names(r))
				break
			}
		}
	}
	if r := Search(nil, "", nil, noLabel); len(r.Matches) != 0 {
		t.Errorf("empty fleet: %+v", r)
	}
	if r := Search(nil, "tools", nil, noLabel); len(r.Matches) != 0 || len(r.Suggestions) != 0 {
		t.Errorf("empty fleet with a query: %+v", r)
	}
}

func TestEmptyQueryDoesNotReorderInput(t *testing.T) {
	f := fleet()
	recents := map[string]time.Time{f[7].Key(): time.Now()}
	Search(f, "", recents, noLabel)
	if f[0].ID != "i-01" || f[7].ID != "i-08" {
		t.Error("Search sorted the caller's slice in place")
	}
}

func TestCaseInsensitive(t *testing.T) {
	cases := []struct {
		query string
		exact bool
		first string
	}{
		{"ACME-P-TOOLS", true, "i-01"},
		{"  acme-p-tools  ", true, "i-01"},
		{"Shop TOOLS", false, "i-05"},
		{"I-03", true, "i-03"},
	}
	for _, c := range cases {
		r := Search(fleet(), c.query, nil, noLabel)
		if len(r.Matches) == 0 || r.Exact != c.exact || r.Matches[0].ID != c.first {
			t.Errorf("%q: exact=%v %v", c.query, r.Exact, names(r))
		}
	}
}

func TestStoppedInstances(t *testing.T) {
	f := fleet()
	f[4].State = "stopped" // acme-p-shop-tools
	// Search does not filter by state: the caller decides what to do with a stopped
	// instance.
	r := Search(f, "acme-p-shop-tools", nil, noLabel)
	if !r.Exact || len(r.Matches) != 1 || r.Matches[0].State != "stopped" {
		t.Errorf("stopped exact match: %+v", r)
	}
	// At equal score, a running instance comes first.
	f[3].State = "stopped" // acme-p-shop-app
	r = Search(f, "shop", nil, noLabel)
	if len(r.Matches) != 3 || r.Matches[0].ID != "i-06" {
		t.Errorf("running first: %v", names(r))
	}
}

func TestOldRecentsGiveNoBonus(t *testing.T) {
	f := fleet()
	recents := map[string]time.Time{
		f[3].Key(): time.Now().Add(-8 * 24 * time.Hour), // acme-p-shop-app, too old
		f[4].Key(): time.Now().Add(-6 * 24 * time.Hour), // acme-p-shop-tools
	}
	r := Search(f, "shop", recents, noLabel)
	if r.Matches[0].ID != "i-05" {
		t.Errorf("a connection six days ago must win: %v", names(r))
	}
	recents[f[4].Key()] = time.Now().Add(-8 * 24 * time.Hour)
	r = Search(f, "shop", recents, noLabel)
	if r.Matches[0].ID != "i-04" {
		t.Errorf("with both connections too old, the original order applies: %v", names(r))
	}
}

func TestScoring(t *testing.T) {
	f := []inventory.Instance{
		{ID: "i-1", Name: "acme-backup-app", Profile: "acme-prod", Region: "eu-west-1", State: "running"},
		{ID: "i-2", Name: "acme-app", Profile: "acme-prod", Region: "eu-west-1", State: "running"},
		{ID: "i-3", Name: "app-gateway", Profile: "acme-prod", Region: "eu-west-1", State: "running"},
		{ID: "i-4", Name: "acme-mapper", Profile: "acme-prod", Region: "eu-west-1", State: "running"},
		{ID: "i-5", Name: "acme-db", Profile: "app-team", Region: "eu-west-1", State: "running"},
	}
	r := Search(f, "app", nil, noLabel)
	// Prefix of the name, then a whole word, then a substring, then another field.
	want := []string{"app-gateway", "acme-backup-app", "acme-app", "acme-mapper", "acme-db"}
	if got := names(r); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("ranking:\n got %v\nwant %v", got, want)
	}
	if r.Exact || r.Fuzzy {
		t.Errorf("flags: %+v", r)
	}
}

func TestEveryTermMustMatch(t *testing.T) {
	r := Search(fleet(), "shop proxy", nil, noLabel)
	// No instance holds both words: the word search fails, the fuzzy search takes
	// over, and the result must be flagged.
	if len(r.Matches) > 0 && !r.Fuzzy {
		t.Errorf("partial word match not flagged: %v", names(r))
	}
}

func TestFuzzyFlag(t *testing.T) {
	cases := []struct {
		query string
		fuzzy bool
		some  bool
	}{
		{"acme-p-tools", false, true},
		{"tools", false, true},
		{"shop", false, true},
		{"aptls", true, true}, // subsequence of acme-p-tools
		{"zzz", false, false}, // nothing at all
		{"tools zzz", false, false},
	}
	for _, c := range cases {
		r := Search(fleet(), c.query, nil, noLabel)
		if r.Fuzzy != c.fuzzy || (len(r.Matches) > 0) != c.some {
			t.Errorf("%q: fuzzy=%v matches=%v", c.query, r.Fuzzy, names(r))
		}
		if len(r.Matches) == 0 && r.Fuzzy {
			t.Errorf("%q: fuzzy without matches", c.query)
		}
	}
}

func TestSuggestionThreshold(t *testing.T) {
	f := fleet()
	cases := []struct {
		query string
		want  []string
	}{
		// Too short to suggest anything.
		{"zq", nil},
		// Five letters allow one edit. "tools" exists twice, suggested once.
		{"toxls", []string{"acme-p-shop-tools", "acme-p-tools", "tools"}},
		// Two edits on five letters is too far.
		{"txxls", nil},
		// Eight letters allow two edits.
		{"prxxy-zz", nil},
		{"proxi", []string{"acme-p-proxy"}},
	}
	for _, c := range cases {
		r := Search(f, c.query, nil, noLabel)
		if len(r.Matches) != 0 {
			t.Errorf("%q: unexpected matches %v", c.query, names(r))
			continue
		}
		if strings.Join(r.Suggestions, ",") != strings.Join(c.want, ",") {
			t.Errorf("%q: suggestions %v, want %v", c.query, r.Suggestions, c.want)
		}
	}
}

func TestSuggestionsAreDistinctAndCapped(t *testing.T) {
	var f []inventory.Instance
	for i, n := range []string{"tools", "tools", "tool", "toolz", "tooks", "tolls"} {
		f = append(f, inventory.Instance{ID: "i-" + string(rune('a'+i)), Name: n, Profile: "acme-prod", Region: "eu-west-1"})
	}
	r := Search(f, "toxls", nil, noLabel)
	if len(r.Matches) != 0 {
		t.Fatalf("matches %v", names(r))
	}
	if len(r.Suggestions) > 3 {
		t.Errorf("more than three suggestions: %v", r.Suggestions)
	}
	seen := map[string]bool{}
	for _, s := range r.Suggestions {
		if seen[s] {
			t.Errorf("duplicate suggestion %q: %v", s, r.Suggestions)
		}
		seen[s] = true
	}
}

func TestSubstringDistance(t *testing.T) {
	cases := []struct {
		text, pattern string
		want          int
	}{
		{"acme-p-tools", "tools", 0},
		{"acme-p-tools", "tols", 1},
		{"acme-p-tools", "", 0},
		{"", "abc", 3},
		{"tokyo-東京-app", "東京", 0},
		{"tokyo-東京-app", "東都", 1},
	}
	for _, c := range cases {
		if got := substringDistance(c.text, c.pattern); got != c.want {
			t.Errorf("substringDistance(%q, %q) = %d, want %d", c.text, c.pattern, got, c.want)
		}
	}
}

func TestSubsequenceGap(t *testing.T) {
	cases := []struct {
		s, q string
		gap  int
		ok   bool
	}{
		{"acme-p-tools", "tools", 0, true},
		{"acme-p-tools", "apt", 5, true},
		{"acme-p-tools", "toolsx", 0, false},
		{"abc", "", 0, true},
	}
	for _, c := range cases {
		gap, ok := subsequence(c.s, c.q)
		if ok != c.ok || (ok && gap != c.gap) {
			t.Errorf("subsequence(%q, %q) = %d, %v; want %d, %v", c.s, c.q, gap, ok, c.gap, c.ok)
		}
	}
}

func TestLiteralQueriesAreExactOnly(t *testing.T) {
	f := []inventory.Instance{
		{ID: "i-0123456789abcdef0", Name: "acme-p-tools", Profile: "acme-prod", Region: "eu-west-1", State: "running", PrivateIP: "10.0.1.21"},
		{ID: "i-0123456789abcdef1", Name: "acme-p-app", Profile: "acme-prod", Region: "eu-west-1", State: "running", PrivateIP: "10.0.1.22"},
		{ID: "mi-0aaaabbbbccccdddd", Name: "onprem", Profile: "acme-prod", Region: "eu-west-1", State: "running", PrivateIP: "fd00::1:2"},
	}
	cases := []struct {
		query       string
		literal     string
		want        []string
		suggestions []string
	}{
		// A partial IP is not a prefix search: it must never find 10.0.1.21.
		{"10.0.1.2", "IP address", nil, []string{"10.0.1.21 (acme-p-tools)", "10.0.1.22 (acme-p-app)"}},
		{"10.0.1.21", "IP address", []string{"acme-p-tools"}, nil},
		{"10.9.9.9", "IP address", nil, nil},
		{"FD00::1:2", "IP address", []string{"onprem"}, nil},
		// A truncated instance ID is not a prefix search either.
		{"i-0123456789abcdef", "ID", nil, []string{"i-0123456789abcdef0 (acme-p-tools)", "i-0123456789abcdef1 (acme-p-app)"}},
		{"I-0123456789ABCDEF1", "ID", []string{"acme-p-app"}, nil},
		{"mi-0aaaabbbbccccdddd", "ID", []string{"onprem"}, nil},
		// Not an IP nor an ID: the word search applies, prefix and substring included.
		{"10.0.1", "", []string{"acme-p-tools", "acme-p-app"}, nil},
		{"i-0123", "", []string{"acme-p-tools", "acme-p-app"}, nil},
	}
	for _, c := range cases {
		r := Search(f, c.query, nil, noLabel)
		if r.Literal != c.literal || strings.Join(names(r), ",") != strings.Join(c.want, ",") ||
			strings.Join(r.Suggestions, ",") != strings.Join(c.suggestions, ",") {
			t.Errorf("%q: literal=%q matches=%v suggestions=%v", c.query, r.Literal, names(r), r.Suggestions)
		}
		if len(r.Matches) > 0 && c.literal != "" && (!r.Exact || r.Fuzzy) {
			t.Errorf("%q: an IP or ID match must be exact: %+v", c.query, r)
		}
	}
}

func TestStrong(t *testing.T) {
	in := inventory.Instance{ID: "i-0123456789abcdef0", Name: "acme-p-shop-app", Profile: "acme-shop", Region: "eu-west-1", Env: "prod", PrivateIP: "10.0.1.21"}
	cases := []struct {
		query string
		want  bool
	}{
		{"acme-p-shop-app", true},
		{"ACME-P-SHOP-APP", true},
		{"i-0123456789abcdef0", true},
		{"10.0.1.21", true},
		{"shop app", true},   // every word is a word of the name
		{"app", true},        // a single whole word
		{"shop-app", true},   // a run of whole words
		{"p-shop", true},     // idem
		{"sho", false},       // part of a word
		{"shop ap", false},   // idem
		{"acme-p", true},     // whole words
		{"acme-shop", false}, // the profile, not the name
		{"shop prod", false}, // the environment is not part of the name
		{"eu-west-1", false}, // nor the region
		{"10.0.1", false},    // part of the IP
		{"i-0123", false},    // part of the ID
		{"", false},
		{"-", false},
	}
	for _, c := range cases {
		if got := Strong(in, c.query); got != c.want {
			t.Errorf("Strong(%q) = %v, want %v", c.query, got, c.want)
		}
	}
}

func TestStrongHybridNode(t *testing.T) {
	// The agent of a hybrid node declares its name and IP: anyone who controls the
	// server can claim those of another machine.
	in := inventory.Instance{ID: "mi-0123456789abcdef0", Name: "acme-onprem-db", Profile: "acme-prod", Region: "eu-west-1", PrivateIP: "192.168.1.5"}
	cases := []struct {
		query string
		want  bool
	}{
		{"mi-0123456789abcdef0", true},
		{"MI-0123456789ABCDEF0", true},
		{"acme-onprem-db", false},
		{"192.168.1.5", false},
		{"onprem db", false},
		{"mi-0123", false},
	}
	for _, c := range cases {
		if got := Strong(in, c.query); got != c.want {
			t.Errorf("Strong(%q) = %v, want %v", c.query, got, c.want)
		}
	}
	// Still found, exactly: only acting on it needs a confirmation.
	if r := Search([]inventory.Instance{in}, "acme-onprem-db", nil, noLabel); !r.Exact || len(r.Matches) != 1 {
		t.Errorf("%+v", r)
	}
}
