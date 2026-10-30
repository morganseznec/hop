package awsconf

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Kind is the credential source the Go SDK will pick for a profile.
type Kind int

const (
	KindNone        Kind = iota // no credentials: a [default] that only sets a region
	KindStatic                  // long-lived access key in credentials
	KindTemporary               // key + session token pasted from the SSO portal
	KindSSO                     // sso_session, refreshable token
	KindSSOLegacy               // sso_start_url on the profile, legacy format
	KindAssumeRole              // role_arn + source_profile or credential_source
	KindProcess                 // credential_process
	KindWebIdentity             // web_identity_token_file (OIDC token, CI runners, EKS)
	KindLogin                   // login_session, written by `aws login`
)

func (k Kind) String() string {
	switch k {
	case KindStatic:
		return "static keys"
	case KindTemporary:
		return "temporary keys"
	case KindSSO:
		return "SSO"
	case KindSSOLegacy:
		return "SSO (legacy)"
	case KindAssumeRole:
		return "assume role"
	case KindProcess:
		return "credential_process"
	case KindWebIdentity:
		return "web identity"
	case KindLogin:
		return "aws login"
	}
	return "none"
}

func (k Kind) IsSSO() bool { return k == KindSSO || k == KindSSOLegacy }

type Profile struct {
	Name   string
	Region string
	Kind   Kind

	SSOSession   string
	SSOStartURL  string
	SSORegion    string
	SSOAccountID string
	SSORoleName  string

	RoleARN              string
	SourceProfile        string
	CredentialSource     string // Environment, Ec2InstanceMetadata, EcsContainer
	WebIdentityTokenFile string
	CredentialProcess    string
	LoginSession         string

	InConfig      bool
	InCredentials bool
	// HasSSO and HasKeys can both be true. The AWS CLI then uses SSO and the Go SDK
	// uses the keys: hop follows the CLI, see PreferSSO.
	HasSSO          bool
	HasKeys         bool
	HasSessionToken bool
	// KeysInCredentials reports that the keys come from the credentials file (the
	// SDK reads that file last, so its keys win over those of config).
	KeysInCredentials bool
}

// PreferSSO reports whether the profile is defined both with SSO and with keys. hop
// then ignores the credentials file for that profile, as the AWS CLI does. Otherwise
// the same profile name would yield two identities depending on the tool (for example
// revoked keys while SSO works).
func (p *Profile) PreferSSO() bool { return p.HasSSO && p.HasKeys }

// Usable reports whether the profile carries credentials. Others are silently skipped.
func (p *Profile) Usable() bool { return p.Kind != KindNone }

// PortalKey identifies the SSO token the profile depends on: the session name for the
// sso_session format, the start URL for the legacy format. It is also the key of the
// token cache file.
func (p *Profile) PortalKey() string {
	if !p.HasSSO {
		return ""
	}
	if p.SSOSession != "" {
		return p.SSOSession
	}
	return p.SSOStartURL
}

type SSOSession struct {
	Name     string
	StartURL string
	Region   string
}

type Files struct {
	ConfigPath      string
	CredentialsPath string
	CredentialsMode fs.FileMode
	Profiles        []*Profile
	Sessions        map[string]SSOSession
	// Commented-out lines in credentials that still hold a key.
	CommentedKeys int
	DefaultRegion string
}

func ConfigPath() string {
	if p := os.Getenv("AWS_CONFIG_FILE"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".aws", "config")
}

func CredentialsPath() string {
	if p := os.Getenv("AWS_SHARED_CREDENTIALS_FILE"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".aws", "credentials")
}

func Load() (*Files, error) {
	return LoadFrom(ConfigPath(), CredentialsPath())
}

func LoadFrom(configPath, credsPath string) (*Files, error) {
	f := &Files{ConfigPath: configPath, CredentialsPath: credsPath, Sessions: map[string]SSOSession{}}
	byName := map[string]*Profile{}
	get := func(name string) *Profile {
		if p, ok := byName[name]; ok {
			return p
		}
		p := &Profile{Name: name}
		byName[name] = p
		f.Profiles = append(f.Profiles, p)
		return p
	}

	if cf, err := os.Open(configPath); err == nil {
		secs, _, err := parseINI(cf)
		cf.Close()
		if err != nil {
			return nil, err
		}
		for _, s := range secs {
			switch {
			case s.name == "default" || strings.HasPrefix(s.name, "profile "):
				p := get(strings.TrimSpace(strings.TrimPrefix(s.name, "profile ")))
				p.InConfig = true
				applyConfig(p, s.keys)
			case strings.HasPrefix(s.name, "sso-session "):
				name := strings.TrimSpace(strings.TrimPrefix(s.name, "sso-session "))
				f.Sessions[name] = SSOSession{Name: name, StartURL: s.keys["sso_start_url"], Region: s.keys["sso_region"]}
			}
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}

	if cf, err := os.Open(credsPath); err == nil {
		if st, err := cf.Stat(); err == nil {
			f.CredentialsMode = st.Mode().Perm()
		}
		secs, commented, err := parseINI(cf)
		cf.Close()
		if err != nil {
			return nil, err
		}
		f.CommentedKeys = commented
		for _, s := range secs {
			p := get(s.name)
			p.InCredentials = true
			if s.keys["aws_access_key_id"] != "" && s.keys["aws_secret_access_key"] != "" {
				p.HasKeys = true
				p.KeysInCredentials = true
				p.HasSessionToken = s.keys["aws_session_token"] != ""
			}
			if r := s.keys["region"]; r != "" && p.Region == "" {
				p.Region = r
			}
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}

	for _, p := range f.Profiles {
		if p.SSOSession != "" {
			if s, ok := f.Sessions[p.SSOSession]; ok {
				p.SSOStartURL = s.StartURL
				if p.SSORegion == "" {
					p.SSORegion = s.Region
				}
			}
		}
		p.Kind = kindOf(p)
	}
	if d, ok := byName["default"]; ok {
		f.DefaultRegion = d.Region
	}
	return f, nil
}

func applyConfig(p *Profile, k map[string]string) {
	p.Region = k["region"]
	p.SSOSession = k["sso_session"]
	p.SSOStartURL = k["sso_start_url"]
	p.SSORegion = k["sso_region"]
	p.SSOAccountID = k["sso_account_id"]
	p.SSORoleName = k["sso_role_name"]
	p.RoleARN = k["role_arn"]
	p.SourceProfile = k["source_profile"]
	p.CredentialSource = k["credential_source"]
	p.WebIdentityTokenFile = k["web_identity_token_file"]
	p.CredentialProcess = k["credential_process"]
	p.LoginSession = k["login_session"]
	p.HasSSO = p.SSOAccountID != "" && (p.SSOSession != "" || p.SSOStartURL != "")
	if k["aws_access_key_id"] != "" && k["aws_secret_access_key"] != "" {
		p.HasKeys = true
		p.HasSessionToken = k["aws_session_token"] != ""
	}
}

// kindOf returns the source hop will use: an assumed role comes first, then a web
// identity token, then SSO (like the AWS CLI, see PreferSSO), then keys, then an `aws
// login` session, then credential_process.
//
// A web identity profile without role_arn is kept: the SDK then reports the missing
// role, which beats a profile that silently disappears.
func kindOf(p *Profile) Kind {
	switch {
	case p.RoleARN != "" && (p.SourceProfile != "" || p.CredentialSource != ""):
		return KindAssumeRole
	case p.WebIdentityTokenFile != "":
		return KindWebIdentity
	case p.HasSSO && p.SSOSession != "":
		return KindSSO
	case p.HasSSO:
		return KindSSOLegacy
	case p.HasKeys && p.HasSessionToken:
		return KindTemporary
	case p.HasKeys:
		return KindStatic
	case p.LoginSession != "":
		return KindLogin
	case p.CredentialProcess != "":
		return KindProcess
	}
	return KindNone
}

func (f *Files) Profile(name string) *Profile {
	for _, p := range f.Profiles {
		if p.Name == name {
			return p
		}
	}
	return nil
}

func (f *Files) Names() []string {
	var out []string
	for _, p := range f.Profiles {
		if p.Usable() {
			out = append(out, p.Name)
		}
	}
	return out
}

// Portal groups the profiles that share the same SSO token: a single login opens them
// all.
type Portal struct {
	Key         string // session name, or start URL for the legacy format
	SessionName string
	StartURL    string
	Region      string
	Profiles    []string
}

// Label is what is shown on screen: the session name, or the host of the start URL.
func (p Portal) Label() string {
	if p.SessionName != "" {
		return p.SessionName
	}
	u := strings.TrimPrefix(strings.TrimPrefix(p.StartURL, "https://"), "http://")
	u, _, _ = strings.Cut(u, "/")
	return u
}

// LoginArgs returns the `aws sso login` arguments that open this portal.
func (p Portal) LoginArgs() []string {
	if p.SessionName != "" {
		return []string{"sso", "login", "--sso-session", p.SessionName}
	}
	return []string{"sso", "login", "--profile", p.Profiles[0]}
}

// Portals only keeps the profiles whose effective source is SSO, filtered by keep.
func (f *Files) Portals(keep func(string) bool) []Portal {
	idx := map[string]int{}
	var out []Portal
	for _, p := range f.Profiles {
		if !p.Kind.IsSSO() || (keep != nil && !keep(p.Name)) {
			continue
		}
		key := p.PortalKey()
		i, ok := idx[key]
		if !ok {
			out = append(out, Portal{Key: key, SessionName: p.SSOSession, StartURL: p.SSOStartURL, Region: p.SSORegion})
			i = len(out) - 1
			idx[key] = i
		}
		out[i].Profiles = append(out[i].Profiles, p.Name)
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].Label() < out[b].Label() })
	return out
}

// PortalOf returns the portal of an SSO profile.
func (f *Files) PortalOf(profile string) (Portal, bool) {
	for _, pt := range f.Portals(nil) {
		for _, n := range pt.Profiles {
			if n == profile {
				return pt, true
			}
		}
	}
	return Portal{}, false
}
