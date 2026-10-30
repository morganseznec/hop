package awsconf

import (
	"strings"
	"testing"
)

// FuzzParseINI feeds arbitrary files to the parser. It must never panic, and the keys it
// returns are always trimmed and lower-cased, as the lookups expect.
func FuzzParseINI(f *testing.F) {
	for _, seed := range []string{
		"",
		"[default]\nregion = eu-west-1\n",
		"[profile acme-prod]\nsso_session = acme-sso \n[sso-session acme-sso]\nsso_region=us-east-1\n",
		"# aws_access_key_id = AKIA1\n; aws_secret_access_key = s1\n[acme]\naws_access_key_id=AKIA2\n",
		"[globex]\ns3 =\n  max_concurrent_requests = 20\nregion = af-south-1\n",
		"\r\n[ initech ]\r\nKey=v=w\r\n",
		"orphan = before any section\n[\n]\n=\n",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, in string) {
		sections, commented, err := parseINI(strings.NewReader(in))
		if err != nil {
			return
		}
		if commented < 0 {
			t.Fatalf("negative count of commented keys: %d", commented)
		}
		for _, s := range sections {
			for k := range s.keys {
				if k != strings.ToLower(strings.TrimSpace(k)) {
					t.Fatalf("section %q: key %q is not trimmed and lower-cased", s.name, k)
				}
			}
		}
	})
}
