package inventory

import (
	"maps"
	"slices"
	"strings"
)

// Words that mark an environment. "recette" is the French term for acceptance testing.
var (
	prodWords = map[string]bool{"p": true, "prod": true, "prd": true, "production": true}
	testWords = map[string]bool{"t": true, "test": true, "tst": true, "dev": true, "staging": true, "stg": true, "preprod": true, "qa": true, "uat": true, "recette": true}
)

// Tag keys that hold the environment, in order of precedence. Keys are compared
// without case.
var envTagKeys = []string{"environment", "env", "stage", "environnement"}

// DeriveEnv reads the environment from a tag if there is one, otherwise from the
// instance name (acme-p-tools, ec2-ew1-a-p-shop-app), otherwise from the profile
// name (acme-shop-test). It returns prod, test or "".
//
// Tags are read in a fixed order, so that two tags that disagree (env=prod,
// Stage=dev) always give the same answer.
func DeriveEnv(tags map[string]string, name, profile string) string {
	keys := slices.Sorted(maps.Keys(tags))
	for _, want := range envTagKeys {
		for _, k := range keys {
			if !strings.EqualFold(k, want) {
				continue
			}
			if e := word(strings.ToLower(tags[k])); e != "" {
				return e
			}
		}
	}
	for _, s := range []string{name, profile} {
		for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return r == '-' || r == '_' || r == '.' }) {
			if e := word(w); e != "" {
				return e
			}
		}
	}
	return ""
}

func word(w string) string {
	switch {
	case prodWords[w]:
		return "prod"
	case testWords[w]:
		return "test"
	}
	return ""
}
