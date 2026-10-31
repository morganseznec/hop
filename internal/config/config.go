// Package config reads ~/.config/hop/config.yaml. The file is optional: without it,
// each profile is queried in its own region. A profile that declares none is queried
// in the region of [default] in ~/.aws/config, else in AWS_REGION or
// AWS_DEFAULT_REGION, else in us-east-1.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	TranscriptWarn    = "warn"    // warn and ask for confirmation
	TranscriptRequire = "require" // refuse any session that is not logged
	TranscriptOff     = "off"     // do not check
)

type Config struct {
	// Profiles to include. Empty means all. Accepts glob patterns (shop-*).
	Include []string `yaml:"include,omitempty"`
	// Profiles to skip, applied after Include.
	Exclude []string `yaml:"exclude,omitempty"`
	// Regions queried in addition to the profile's own.
	ExtraRegions map[string][]string `yaml:"extra_regions,omitempty"`
	// Region for profiles that declare none, used before the one from [default].
	DefaultRegion string `yaml:"default_region,omitempty"`
	// warn, require or off.
	Transcript string `yaml:"transcript,omitempty"`
	// Lifetime of the cached inventory, as a Go duration (15m, 1h).
	CacheTTL string `yaml:"cache_ttl,omitempty"`
	// Customer label shown next to the profile (acme-prod: ACME).
	Labels map[string]string `yaml:"labels,omitempty"`
	// GCP projects whose Compute Engine instances hop lists, glob patterns allowed ("*":
	// every project gcloud's account can see). Setting it turns on the experimental GCP
	// support.
	GCPProjects []string `yaml:"gcp_projects,omitempty"`
}

// Dir returns the folder of the configuration. Without a home directory (HOME unset)
// and without XDG_CONFIG_HOME, it fails rather than use a folder relative to the
// working directory.
func Dir() (string, error) {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "hop"), nil
	}
	if runtime.GOOS == "windows" {
		if d, err := os.UserConfigDir(); err == nil {
			return filepath.Join(d, "hop"), nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", errors.New("no folder for hop's configuration: the home directory is not set (set HOME, or XDG_CONFIG_HOME)")
	}
	return filepath.Join(home, ".config", "hop"), nil
}

// Path returns the configuration file, or the Dir error.
func Path() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.yaml"), nil
}

func Load() (*Config, error) {
	p, err := Path()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return &Config{}, nil
	}
	if err != nil {
		return nil, err
	}
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(b))
	// A misspelt key ("exlude:") is reported rather than silently ignored.
	dec.KnownFields(true)
	// io.EOF: an empty file, or one with only comments.
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return nil, errors.New(p + ": " + err.Error())
	}
	// An invalid pattern matches nothing: in include, it would hide every profile.
	for _, list := range []struct {
		key      string
		patterns []string
	}{{"include", c.Include}, {"exclude", c.Exclude}, {"gcp_projects", c.GCPProjects}} {
		for _, pat := range list.patterns {
			if _, err := path.Match(pat, ""); err != nil {
				return nil, fmt.Errorf("%s: invalid pattern in %s: %q", p, list.key, pat)
			}
		}
	}
	switch c.Transcript {
	case "", TranscriptWarn, TranscriptRequire, TranscriptOff:
	default:
		return nil, errors.New(p + ": transcript must be warn, require or off")
	}
	if c.CacheTTL != "" {
		if _, err := time.ParseDuration(c.CacheTTL); err != nil {
			return nil, errors.New(p + ": invalid cache_ttl: " + c.CacheTTL)
		}
	}
	return &c, nil
}

// Save rewrites the file. Comments are not preserved: the previous file is kept next
// to it as .bak. The file lists profile names: it is created readable by its owner
// only, like the cache.
func (c *Config) Save() error {
	p, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	if old, err := os.ReadFile(p); err == nil {
		if err := os.WriteFile(p+".bak", old, 0o600); err != nil {
			return err
		}
	}
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o600)
}

func (c *Config) TranscriptPolicy() string {
	if c.Transcript == "" {
		return TranscriptWarn
	}
	return c.Transcript
}

func (c *Config) TTL() time.Duration {
	if d, err := time.ParseDuration(c.CacheTTL); err == nil && c.CacheTTL != "" {
		return d
	}
	return 15 * time.Minute
}

func matchAny(patterns []string, name string) bool {
	for _, p := range patterns {
		if ok, _ := path.Match(p, name); ok {
			return true
		}
	}
	return false
}

// Wants reports whether a profile is in scope.
func (c *Config) Wants(profile string) bool {
	if len(c.Include) > 0 && !matchAny(c.Include, profile) {
		return false
	}
	return !matchAny(c.Exclude, profile)
}

// Regions returns the profile's region, falling back to the configured default, then
// the one from [default], then AWS_REGION, then AWS_DEFAULT_REGION, then us-east-1,
// followed by the declared extra regions.
func (c *Config) Regions(profile, profileRegion, fileDefault string) []string {
	base := profileRegion
	for _, r := range []string{c.DefaultRegion, fileDefault, os.Getenv("AWS_REGION"), os.Getenv("AWS_DEFAULT_REGION"), "us-east-1"} {
		if base != "" {
			break
		}
		base = r
	}
	out := []string{base}
	for _, r := range c.ExtraRegions[profile] {
		if !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	return out
}

func (c *Config) Label(profile string) string { return c.Labels[profile] }
