package config

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

// tempConfigDir points XDG_CONFIG_HOME to a temporary folder and returns the folder
// where config.yaml lives.
func tempConfigDir(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", base)
	return filepath.Join(base, "hop")
}

func writeConfig(t *testing.T, content string) {
	t.Helper()
	if err := os.MkdirAll(Dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Path(), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDir(t *testing.T) {
	dir := tempConfigDir(t)
	if Dir() != dir || Path() != filepath.Join(dir, "config.yaml") {
		t.Errorf("with XDG_CONFIG_HOME: %s, %s", Dir(), Path())
	}

	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("APPDATA", filepath.Join(home, "AppData", "Roaming"))
	want := filepath.Join(home, ".config", "hop")
	if runtime.GOOS == "windows" {
		want = filepath.Join(home, "AppData", "Roaming", "hop")
	}
	if got := Dir(); got != want {
		t.Errorf("without XDG_CONFIG_HOME: %s, want %s", got, want)
	}
}

func TestLoadMissingFile(t *testing.T) {
	tempConfigDir(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c, &Config{}) {
		t.Errorf("no file must give an empty configuration: %+v", c)
	}
	if c.TranscriptPolicy() != TranscriptWarn || c.TTL() != 15*time.Minute {
		t.Errorf("defaults: %s, %s", c.TranscriptPolicy(), c.TTL())
	}
}

func TestLoad(t *testing.T) {
	tempConfigDir(t)
	writeConfig(t, `# hop configuration
include: [acme-*, globex]
exclude:
  - acme-sandbox
extra_regions:
  acme-prod: [us-east-1, ap-south-1]
default_region: eu-central-1
transcript: require
cache_ttl: 1h
labels:
  acme-prod: ACME
`)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	want := &Config{
		Include:       []string{"acme-*", "globex"},
		Exclude:       []string{"acme-sandbox"},
		ExtraRegions:  map[string][]string{"acme-prod": {"us-east-1", "ap-south-1"}},
		DefaultRegion: "eu-central-1",
		Transcript:    TranscriptRequire,
		CacheTTL:      "1h",
		Labels:        map[string]string{"acme-prod": "ACME"},
	}
	if !reflect.DeepEqual(c, want) {
		t.Errorf("got %+v\nwant %+v", c, want)
	}
	if c.TTL() != time.Hour || c.TranscriptPolicy() != TranscriptRequire || c.Label("acme-prod") != "ACME" || c.Label("globex") != "" {
		t.Errorf("accessors: %s %s %q", c.TTL(), c.TranscriptPolicy(), c.Label("acme-prod"))
	}
}

func TestLoadErrors(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"invalid YAML", "include: [acme-*\n", "config.yaml"},
		{"wrong type", "include: 3\nexclude: {a: b}\n", "config.yaml"},
		{"unknown transcript policy", "transcript: always\n", "transcript must be warn, require or off"},
		{"invalid cache TTL", "cache_ttl: 15 minutes\n", "invalid cache_ttl: 15 minutes"},
		{"misspelt key", "include: [acme-*]\nexlude: [acme-sandbox]\n", "exlude"},
		{"invalid include pattern", "include: [acme-*, \"acme-[\"]\n", `invalid pattern in include: "acme-["`},
		{"invalid exclude pattern", "exclude: ['acme-\\']\n", `invalid pattern in exclude: "acme-\\"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tempConfigDir(t)
			writeConfig(t, c.content)
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %v, want it to mention %q", err, c.want)
			}
			if err != nil && !strings.Contains(err.Error(), Path()) {
				t.Errorf("the error must name the file: %v", err)
			}
		})
	}
}

func TestLoadEmptyFile(t *testing.T) {
	for _, content := range []string{"", "# nothing configured yet\n", "---\n"} {
		tempConfigDir(t)
		writeConfig(t, content)
		c, err := Load()
		if err != nil {
			t.Fatalf("%q: %v", content, err)
		}
		if !reflect.DeepEqual(c, &Config{}) {
			t.Errorf("%q: %+v", content, c)
		}
	}
}

func TestLoadAcceptsEveryPolicy(t *testing.T) {
	for _, p := range []string{TranscriptWarn, TranscriptRequire, TranscriptOff} {
		tempConfigDir(t)
		writeConfig(t, "transcript: "+p+"\n")
		c, err := Load()
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if c.TranscriptPolicy() != p {
			t.Errorf("policy %s read as %s", p, c.TranscriptPolicy())
		}
	}
}

func TestLoadUnreadable(t *testing.T) {
	// A directory in place of the file is an error, not an empty configuration.
	tempConfigDir(t)
	if err := os.MkdirAll(Path(), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil {
		t.Error("want an error")
	}
}

func TestSave(t *testing.T) {
	dir := tempConfigDir(t)
	c := &Config{Include: []string{"acme-*"}, ExtraRegions: map[string][]string{"acme-prod": {"us-east-1"}}, CacheTTL: "30m"}

	// First save: creates the folder, nothing to back up.
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "config.yaml.bak")); !os.IsNotExist(err) {
		t.Errorf("no previous file, yet a backup exists: %v", err)
	}
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, c) {
		t.Errorf("round trip: %+v, want %+v", got, c)
	}

	// Second save over a hand-written file: its comments survive in the backup.
	handWritten := "# my comments\ninclude: [acme-*]\n"
	writeConfig(t, handWritten)
	c.ExtraRegions["acme-prod"] = append(c.ExtraRegions["acme-prod"], "ap-south-1")
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	bak, err := os.ReadFile(Path() + ".bak")
	if err != nil || string(bak) != handWritten {
		t.Errorf("backup %q (%v), want the previous file", bak, err)
	}
	got, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.ExtraRegions["acme-prod"], []string{"us-east-1", "ap-south-1"}) {
		t.Errorf("saved regions: %v", got.ExtraRegions)
	}
	// Empty fields are omitted from the file.
	b, _ := os.ReadFile(Path())
	if strings.Contains(string(b), "labels") || strings.Contains(string(b), "transcript") {
		t.Errorf("empty fields written:\n%s", b)
	}
}

func TestSavePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits do not apply on Windows")
	}
	dir := tempConfigDir(t)
	c := &Config{Include: []string{"acme-*"}}
	// Twice: the second save also writes the backup.
	for range 2 {
		if err := c.Save(); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{dir, Path(), Path() + ".bak"} {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s open to other users: %o", p, st.Mode().Perm())
		}
	}
}

func TestSaveFailsWhenFolderIsAFile(t *testing.T) {
	base := t.TempDir()
	blocker := filepath.Join(base, "not-a-dir")
	os.WriteFile(blocker, nil, 0o644)
	t.Setenv("XDG_CONFIG_HOME", blocker)
	if err := (&Config{}).Save(); err == nil {
		t.Error("want an error when the folder cannot be created")
	}
}

func TestTTL(t *testing.T) {
	cases := []struct {
		ttl  string
		want time.Duration
	}{
		{"", 15 * time.Minute},
		{"5m", 5 * time.Minute},
		{"1h30m", 90 * time.Minute},
		{"bogus", 15 * time.Minute},
	}
	for _, c := range cases {
		if got := (&Config{CacheTTL: c.ttl}).TTL(); got != c.want {
			t.Errorf("TTL(%q) = %s, want %s", c.ttl, got, c.want)
		}
	}
}

func TestWants(t *testing.T) {
	cases := []struct {
		name             string
		include, exclude []string
		profile          string
		want             bool
	}{
		{"no filter", nil, nil, "acme-prod", true},
		{"included by glob", []string{"acme-*"}, nil, "acme-prod", true},
		{"not included", []string{"acme-*"}, nil, "globex", false},
		{"glob does not match a prefix only", []string{"acme"}, nil, "acme-prod", false},
		{"one of several includes", []string{"acme-*", "globex"}, nil, "globex", true},
		{"excluded", nil, []string{"acme-sandbox"}, "acme-sandbox", false},
		{"exclude applies after include", []string{"acme-*"}, []string{"*-sandbox"}, "acme-sandbox", false},
		{"exclude glob leaves others", []string{"acme-*"}, []string{"*-sandbox"}, "acme-prod", true},
		{"character class", []string{"acme-[pt]*"}, nil, "acme-test", true},
		{"single character wildcard", []string{"acme-?"}, nil, "acme-p", true},
		{"single character wildcard, longer name", []string{"acme-?"}, nil, "acme-prod", false},
		{"matching is case-sensitive", []string{"ACME-*"}, nil, "acme-prod", false},
		{"an invalid include pattern matches nothing", []string{"acme-["}, nil, "acme-prod", false},
		{"an invalid exclude pattern excludes nothing", nil, []string{"acme-["}, "acme-prod", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := &Config{Include: c.include, Exclude: c.exclude}
			if got := cfg.Wants(c.profile); got != c.want {
				t.Errorf("Wants(%q) = %v, want %v", c.profile, got, c.want)
			}
		})
	}
}

func TestRegions(t *testing.T) {
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")
	cases := []struct {
		name                         string
		cfg                          Config
		profile, region, fileDefault string
		want                         []string
	}{
		{"profile region", Config{}, "acme-prod", "ap-south-1", "eu-west-3", []string{"ap-south-1"}},
		{"configured default before the file default", Config{DefaultRegion: "eu-central-1"}, "acme-prod", "", "eu-west-3", []string{"eu-central-1"}},
		{"file default", Config{}, "acme-prod", "", "eu-west-3", []string{"eu-west-3"}},
		{"built-in fallback", Config{}, "acme-prod", "", "", []string{"us-east-1"}},
		{"profile region wins over the configured default", Config{DefaultRegion: "eu-central-1"}, "acme-prod", "ap-south-1", "", []string{"ap-south-1"}},
		{
			"extra regions follow, in order",
			Config{ExtraRegions: map[string][]string{"acme-prod": {"us-east-1", "ap-south-1"}}},
			"acme-prod", "eu-west-1", "", []string{"eu-west-1", "us-east-1", "ap-south-1"},
		},
		{
			"extra regions are deduplicated, base included",
			Config{ExtraRegions: map[string][]string{"acme-prod": {"eu-west-1", "us-east-1", "us-east-1"}}},
			"acme-prod", "eu-west-1", "", []string{"eu-west-1", "us-east-1"},
		},
		{
			"extra regions of another profile are ignored",
			Config{ExtraRegions: map[string][]string{"globex": {"us-east-1"}}},
			"acme-prod", "eu-west-1", "", []string{"eu-west-1"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.cfg.Regions(c.profile, c.region, c.fileDefault); !reflect.DeepEqual(got, c.want) {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestRegionsFromEnvironment(t *testing.T) {
	cases := []struct {
		name                       string
		awsRegion, awsDefault      string
		cfg                        Config
		profileRegion, fileDefault string
		want                       string
	}{
		{"AWS_REGION", "ap-southeast-2", "ca-central-1", Config{}, "", "", "ap-southeast-2"},
		{"AWS_DEFAULT_REGION when AWS_REGION is empty", "", "ca-central-1", Config{}, "", "", "ca-central-1"},
		{"neither", "", "", Config{}, "", "", "us-east-1"},
		{"the file default comes first", "ap-southeast-2", "ca-central-1", Config{}, "", "eu-west-3", "eu-west-3"},
		{"the configured default comes first", "ap-southeast-2", "", Config{DefaultRegion: "eu-central-1"}, "", "", "eu-central-1"},
		{"the profile region comes first", "ap-southeast-2", "", Config{}, "sa-east-1", "", "sa-east-1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("AWS_REGION", c.awsRegion)
			t.Setenv("AWS_DEFAULT_REGION", c.awsDefault)
			if got := c.cfg.Regions("acme-prod", c.profileRegion, c.fileDefault); !reflect.DeepEqual(got, []string{c.want}) {
				t.Errorf("got %v, want [%s]", got, c.want)
			}
		})
	}
}

func TestRegionsDoesNotAliasConfig(t *testing.T) {
	// Changing the result must never change the configuration.
	cfg := &Config{ExtraRegions: map[string][]string{"acme-prod": {"us-east-1"}}}
	r := cfg.Regions("acme-prod", "eu-west-1", "")
	r[1] = "changed"
	if cfg.ExtraRegions["acme-prod"][0] != "us-east-1" {
		t.Errorf("configuration modified through the result: %v", cfg.ExtraRegions)
	}
}
