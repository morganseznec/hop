package inventory

import (
	"testing"
	"unicode"
)

// FuzzClean checks that no control or bidi character survives: tag values end up in
// the terminal, where they could rewrite what hop prints.
func FuzzClean(f *testing.F) {
	for _, seed := range []string{"", "acme-p-tools", "\x1b[2Jsession LOGGED", "a\u202eb", "\r\n\t", "é"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		for _, r := range clean(s) {
			if unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) {
				t.Fatalf("clean(%q) kept %U", s, r)
			}
		}
	})
}

// FuzzParseSessionPrefs feeds arbitrary documents: never a panic, and a session is only
// reported as logged when there is a destination to show.
func FuzzParseSessionPrefs(f *testing.F) {
	for _, seed := range []string{
		"",
		"{}",
		`{"inputs":{"cloudWatchLogGroupName":"/acme/ssm/sessions","cloudWatchEncryptionEnabled":true}}`,
		`{"inputs":{"s3BucketName":"acme-logs","kmsKeyId":"alias/x"}}`,
		`{"inputs":{"cloudWatchLogGroupName":"\u001b[31m"}}`,
		`{"inputs":null}`,
		`[`,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, doc string) {
		tr := ParseSessionPrefs(doc)
		if tr.Enabled && tr.Destination() == "" {
			t.Fatalf("%q: logging enabled without a destination", doc)
		}
		if !tr.Enabled && tr.Destination() != "" {
			t.Fatalf("%q: destination %q but logging disabled", doc, tr.Destination())
		}
		if tr.Err != "" && tr.Checked {
			t.Fatalf("%q: unreadable document reported as checked", doc)
		}
	})
}
