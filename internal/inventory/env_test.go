package inventory

import (
	"slices"
	"strings"
	"testing"
)

func TestDeriveEnvCases(t *testing.T) {
	cases := []struct {
		name     string
		tags     map[string]string
		instance string
		profile  string
		want     string
	}{
		{"tag key is case-insensitive", map[string]string{"ENV": "prd"}, "bastion", "", "prod"},
		{"French tag key", map[string]string{"Environnement": "recette"}, "bastion", "", "test"},
		{"stage tag", map[string]string{"stage": "staging"}, "bastion", "", "test"},
		{"tag value is case-insensitive", map[string]string{"Environment": "QA"}, "bastion", "", "test"},
		{"tag wins over the name", map[string]string{"env": "dev"}, "acme-p-tools", "acme-prod", "test"},
		{"unknown tag value falls back to the name", map[string]string{"env": "sandbox"}, "acme-p-tools", "", "prod"},
		{"unrelated tags are ignored", map[string]string{"Name": "prod", "team": "test"}, "bastion", "", ""},
		{"name wins over the profile", nil, "acme-t-tools", "acme-prod", "test"},
		{"profile when the name says nothing", nil, "bastion", "acme-prod", "prod"},
		{"underscore separator", nil, "acme_uat_app", "", "test"},
		{"dot separator", nil, "app.preprod.acme", "", "test"},
		{"full word", nil, "acme-production-db", "", "prod"},
		{"words inside a word do not count", nil, "acme-product-api", "acme-testing", ""},
		{"single letters must stand alone", nil, "acme-tp-app", "", ""},
		{"first matching word wins", nil, "acme-t-to-p", "", "test"},
		{"empty", nil, "", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DeriveEnv(c.tags, c.instance, c.profile); got != c.want {
				t.Errorf("DeriveEnv(%v, %q, %q) = %q, want %q", c.tags, c.instance, c.profile, got, c.want)
			}
		})
	}
}

func TestDeriveEnvDeterministic(t *testing.T) {
	tags := map[string]string{"env": "prod", "Stage": "dev", "Owner": "acme"}
	first := DeriveEnv(tags, "bastion", "")
	for range 200 {
		if got := DeriveEnv(tags, "bastion", ""); got != first {
			t.Fatalf("DeriveEnv returned %q then %q for the same input", first, got)
		}
	}
}

func TestDeriveEnvTagPrecedence(t *testing.T) {
	cases := []struct {
		name string
		tags map[string]string
		want string
	}{
		{"environment before env", map[string]string{"env": "prod", "Environment": "dev"}, "test"},
		{"env before stage", map[string]string{"Stage": "dev", "env": "prod"}, "prod"},
		{"stage before environnement", map[string]string{"environnement": "prod", "stage": "qa"}, "test"},
		{"an unknown value gives way to the next key", map[string]string{"Environment": "sandbox", "env": "prd"}, "prod"},
		{"same key in two cases", map[string]string{"ENV": "prod", "env": "dev"}, "prod"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for range 50 {
				if got := DeriveEnv(c.tags, "bastion", ""); got != c.want {
					t.Fatalf("DeriveEnv(%v) = %q, want %q", c.tags, got, c.want)
				}
			}
		})
	}
}

func TestNormalizeEnv(t *testing.T) {
	cases := map[string]string{
		"prod": "prod", "PROD": "prod", "production": "prod", "prd": "prod", " p ": "prod",
		"test": "test", "staging": "test", "dev": "test", "qa": "test", "uat": "test",
		"preprod": "test", "Recette": "test", "stg": "test",
		"": "", "sandbox": "", "prod-eu": "", "testing": "",
	}
	for in, want := range cases {
		got, ok := NormalizeEnv(in)
		if got != want || ok != (want != "") {
			t.Errorf("NormalizeEnv(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
}

func TestEnvWords(t *testing.T) {
	if got := strings.Join(EnvWords("prod"), ","); got != "p,prd,production" {
		t.Errorf("prod: %s", got)
	}
	got := EnvWords("test")
	if !slices.IsSorted(got) || slices.Contains(got, "test") || !slices.Contains(got, "staging") || !slices.Contains(got, "dev") {
		t.Errorf("test: %v", got)
	}
	// Every word listed is accepted, and means its environment.
	for _, env := range []string{"prod", "test"} {
		for _, w := range EnvWords(env) {
			if got, _ := NormalizeEnv(w); got != env {
				t.Errorf("%s listed for %s, normalized to %q", w, env, got)
			}
		}
	}
}
