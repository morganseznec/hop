package awsconf

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// Mirrors a real-world setup: legacy SSO format, sso_session with a trailing space in
// its value, a profile defined in both files, temporary keys pasted from the portal,
// commented-out keys.
const testConfig = `[profile globex]
sso_start_url = https://globex.awsapps.com/start
sso_region    = us-east-1
sso_account_id= 111111111111
sso_role_name = AdministratorAccess
region        = us-east-1

[profile acme-legacy]
sso_start_url = https://acme.awsapps.com/start
sso_region    = eu-west-1
sso_account_id= 222222222222
sso_role_name = AdministratorAccess
region        = eu-west-1

[sso-session acme-sso]
sso_start_url = https://acme.awsapps.com/start
sso_region    = eu-west-1

[profile acme-prod]
sso_session   = acme-sso` + " " /* trailing space, must be trimmed */ + `
sso_account_id= 333333333333
sso_role_name = AdministratorAccess
region        = eu-west-1

[profile acme-data]
sso_session   = acme-sso
sso_account_id= 444444444444
sso_role_name = AdministratorAccess
region        = eu-west-1

[profile initech]
sso_start_url = https://initech.awsapps.com/start
sso_region    = eu-west-1
sso_account_id= 555555555555
sso_role_name = AdministratorAccess
region        = af-south-1
s3 =
  max_concurrent_requests = 20

[default]
region = eu-west-1
`

const testCreds = `# aws_access_key_id = AKIAOLD
#aws_secret_access_key = old
[acme-billing]
aws_access_key_id = AKIA1
aws_secret_access_key = s1

[acme-data]
aws_access_key_id = AKIA2
aws_secret_access_key = s2

[222222222222_AdministratorAccess]
aws_access_key_id = ASIA3
aws_secret_access_key = s3
aws_session_token = t3
`

func load(t *testing.T) *Files {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config")
	creds := filepath.Join(dir, "credentials")
	os.WriteFile(cfg, []byte(testConfig), 0o644)
	os.WriteFile(creds, []byte(testCreds), 0o600)
	f, err := LoadFrom(cfg, creds)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestKinds(t *testing.T) {
	f := load(t)
	want := map[string]Kind{
		"globex":                           KindSSOLegacy,
		"acme-legacy":                      KindSSOLegacy,
		"acme-prod":                        KindSSO,
		"acme-data":                        KindSSO, // hop follows the AWS CLI: SSO wins over keys
		"initech":                          KindSSOLegacy,
		"default":                          KindNone,
		"acme-billing":                     KindStatic,
		"222222222222_AdministratorAccess": KindTemporary,
	}
	for name, k := range want {
		p := f.Profile(name)
		if p == nil {
			t.Fatalf("profile %s missing", name)
		}
		if p.Kind != k {
			t.Errorf("%s: kind %v, want %v", name, p.Kind, k)
		}
	}
	data := f.Profile("acme-data")
	if !data.PreferSSO() || !data.InConfig || !data.InCredentials {
		t.Errorf("double definition not detected: %+v", data)
	}
	if f.Profile("acme-billing").PreferSSO() {
		t.Error("acme-billing only has keys")
	}
	if f.Profile("acme-prod").SSOSession != "acme-sso" {
		t.Error("trailing space of sso_session not trimmed")
	}
	if f.Profile("initech").Region != "af-south-1" {
		t.Error("initech region lost after the s3 sub-section")
	}
	if f.CommentedKeys != 2 {
		t.Errorf("commented keys: %d, want 2", f.CommentedKeys)
	}
	if f.DefaultRegion != "eu-west-1" {
		t.Errorf("default region: %q", f.DefaultRegion)
	}
}

func TestPortals(t *testing.T) {
	f := load(t)
	ps := f.Portals(nil)
	// globex, acme (legacy format), acme-sso, initech.
	if len(ps) != 4 {
		t.Fatalf("%d portals: %+v", len(ps), ps)
	}
	var session *Portal
	for i := range ps {
		if ps[i].SessionName == "acme-sso" {
			session = &ps[i]
		}
	}
	if session == nil || len(session.Profiles) != 2 || session.Profiles[1] != "acme-data" {
		t.Errorf("acme-sso portal: %+v", session)
	}
	if got := session.LoginArgs(); got[2] != "--sso-session" {
		t.Errorf("login: %v", got)
	}
	pt, ok := f.PortalOf("acme-legacy")
	if !ok || pt.Label() != "acme.awsapps.com" || pt.LoginArgs()[3] != "acme-legacy" {
		t.Errorf("legacy portal: %+v", pt)
	}
}

// loadFiles writes config and credentials (either may be empty: no file) to a
// temporary folder and loads them.
func loadFiles(t *testing.T, config, creds string) *Files {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config")
	cred := filepath.Join(dir, "credentials")
	if config != "" {
		if err := os.WriteFile(cfg, []byte(config), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if creds != "" {
		if err := os.WriteFile(cred, []byte(creds), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	f, err := LoadFrom(cfg, cred)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func profileNames(f *Files) []string {
	var out []string
	for _, p := range f.Profiles {
		out = append(out, p.Name)
	}
	return out
}

func TestProfileNameForms(t *testing.T) {
	f := loadFiles(t, `[profile acme-prod]
region = eu-west-1

[default]
region = us-east-1

[profile    globex  ]
region = us-west-2

[acme-unprefixed]
aws_access_key_id = AKIA1
aws_secret_access_key = s1

[sso-session acme-sso]
sso_start_url = https://acme.awsapps.com/start

[services acme-endpoints]
ec2 =
  endpoint_url = http://localhost:4566
`, `[acme-billing]
aws_access_key_id = AKIA2
aws_secret_access_key = s2

[default]
aws_access_key_id = AKIA3
aws_secret_access_key = s3
`)
	got := profileNames(f)
	want := []string{"acme-prod", "default", "globex", "acme-billing"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("profiles %v, want %v", got, want)
	}
	// In the config file, a section without the "profile " prefix is not a profile,
	// as with the AWS CLI and the SDK.
	if f.Profile("acme-unprefixed") != nil {
		t.Error("[acme-unprefixed] in config must not become a profile")
	}
	if f.Profile("acme-endpoints") != nil || f.Profile("services acme-endpoints") != nil {
		t.Error("a services section is not a profile")
	}
	def := f.Profile("default")
	if !def.InConfig || !def.InCredentials || def.Kind != KindStatic || def.Region != "us-east-1" {
		t.Errorf("default defined in both files: %+v", def)
	}
	if f.DefaultRegion != "us-east-1" {
		t.Errorf("default region %q", f.DefaultRegion)
	}
}

func TestDuplicateBetweenFiles(t *testing.T) {
	f := loadFiles(t, `[profile acme-prod]
region = eu-west-1

[profile acme-data]
sso_session = acme-sso
sso_account_id = 111111111111
sso_role_name = ReadOnly
`, `[acme-prod]
region = us-east-1
aws_access_key_id = AKIA1
aws_secret_access_key = s1

[acme-shop]
region = ap-south-1
aws_access_key_id = AKIA2
aws_secret_access_key = s2

[acme-data]
aws_access_key_id = AKIA3
aws_secret_access_key = s3
aws_session_token = t3
`)
	if n := len(f.Profiles); n != 3 {
		t.Fatalf("a profile defined in both files must be listed once: %v", profileNames(f))
	}
	cases := []struct {
		name      string
		region    string
		kind      Kind
		preferSSO bool
	}{
		// The region of the config file wins over the credentials file.
		{"acme-prod", "eu-west-1", KindStatic, false},
		// A region found only in credentials is still used.
		{"acme-shop", "ap-south-1", KindStatic, false},
		// SSO wins over temporary keys, as with the AWS CLI.
		{"acme-data", "", KindSSO, true},
	}
	for _, c := range cases {
		p := f.Profile(c.name)
		if p.Region != c.region || p.Kind != c.kind || p.PreferSSO() != c.preferSSO {
			t.Errorf("%s: region %q kind %v preferSSO %v, want %q %v %v", c.name, p.Region, p.Kind, p.PreferSSO(), c.region, c.kind, c.preferSSO)
		}
	}
	if !f.Profile("acme-prod").InConfig || !f.Profile("acme-prod").InCredentials || f.Profile("acme-shop").InConfig {
		t.Error("InConfig / InCredentials flags")
	}
	if !f.Profile("acme-data").HasSessionToken {
		t.Error("session token in credentials not detected")
	}
	for name, want := range map[string]bool{"acme-prod": true, "acme-shop": true, "acme-data": true} {
		if got := f.Profile(name).KeysInCredentials; got != want {
			t.Errorf("%s: KeysInCredentials %v, want %v", name, got, want)
		}
	}
}

func TestKeysInCredentials(t *testing.T) {
	f := loadFiles(t, `[profile acme-config-keys]
aws_access_key_id = AKIA1
aws_secret_access_key = s1

[profile acme-both]
aws_access_key_id = AKIA2
aws_secret_access_key = s2

[profile acme-region-only-in-creds]
aws_access_key_id = AKIA3
aws_secret_access_key = s3
`, `[acme-both]
aws_access_key_id = AKIA4
aws_secret_access_key = s4

[acme-region-only-in-creds]
region = eu-west-1

[acme-file-keys]
aws_access_key_id = AKIA5
aws_secret_access_key = s5
`)
	cases := map[string]bool{
		"acme-config-keys":          false,
		"acme-both":                 true, // the SDK reads credentials last
		"acme-region-only-in-creds": false,
		"acme-file-keys":            true,
	}
	for name, want := range cases {
		p := f.Profile(name)
		if !p.HasKeys || p.KeysInCredentials != want {
			t.Errorf("%s: HasKeys %v KeysInCredentials %v, want true %v", name, p.HasKeys, p.KeysInCredentials, want)
		}
	}
}

func TestKindOf(t *testing.T) {
	cases := []struct {
		name string
		keys string
		want Kind
	}{
		{"region only", "region = eu-west-1", KindNone},
		{"static keys in config", "aws_access_key_id = AKIA1\naws_secret_access_key = s1", KindStatic},
		{"temporary keys in config", "aws_access_key_id = ASIA1\naws_secret_access_key = s1\naws_session_token = t1", KindTemporary},
		{"access key without secret", "aws_access_key_id = AKIA1", KindNone},
		{"sso_session format", "sso_session = acme-sso\nsso_account_id = 111111111111\nsso_role_name = ReadOnly", KindSSO},
		{"legacy SSO format", "sso_start_url = https://acme.awsapps.com/start\nsso_account_id = 111111111111\nsso_role_name = ReadOnly", KindSSOLegacy},
		{"SSO without an account is not usable", "sso_session = acme-sso\nsso_role_name = ReadOnly", KindNone},
		{"assume role", "role_arn = arn:aws:iam::111111111111:role/Admin\nsource_profile = acme-billing", KindAssumeRole},
		{"assume role wins over SSO", "role_arn = arn:aws:iam::111111111111:role/Admin\nsource_profile = acme-billing\nsso_session = acme-sso\nsso_account_id = 111111111111", KindAssumeRole},
		{"SSO wins over keys", "sso_session = acme-sso\nsso_account_id = 111111111111\naws_access_key_id = AKIA1\naws_secret_access_key = s1", KindSSO},
		{"keys win over credential_process", "credential_process = /usr/bin/creds\naws_access_key_id = AKIA1\naws_secret_access_key = s1", KindStatic},
		{"credential_process", "credential_process = /usr/bin/creds --profile=globex", KindProcess},
		{"role_arn without a source is not usable", "role_arn = arn:aws:iam::111111111111:role/Admin", KindNone},
		{"assume role from the environment", "role_arn = arn:aws:iam::111111111111:role/Deploy\ncredential_source = Environment", KindAssumeRole},
		{"assume role from instance metadata", "role_arn = arn:aws:iam::111111111111:role/Deploy\ncredential_source = Ec2InstanceMetadata", KindAssumeRole},
		{"credential_source without role_arn is not usable", "credential_source = Environment", KindNone},
		{"web identity", "role_arn = arn:aws:iam::111111111111:role/Deploy\nweb_identity_token_file = /var/run/secrets/token", KindWebIdentity},
		// The SDK reports the missing role_arn: better than hiding the profile.
		{"web identity without role_arn", "web_identity_token_file = /var/run/secrets/token", KindWebIdentity},
		{"aws login", "login_session = arn:aws:iam::111111111111:user/alice", KindLogin},
		{"keys win over aws login", "login_session = arn:aws:iam::111111111111:user/alice\naws_access_key_id = AKIA1\naws_secret_access_key = s1", KindStatic},
		{"aws login wins over credential_process", "login_session = arn:aws:iam::111111111111:user/alice\ncredential_process = /usr/bin/creds", KindLogin},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := loadFiles(t, "[profile globex]\n"+c.keys+"\n", "")
			p := f.Profile("globex")
			if p.Kind != c.want {
				t.Errorf("kind %v, want %v", p.Kind, c.want)
			}
			if p.Usable() != (c.want != KindNone) {
				t.Errorf("usable %v for kind %v", p.Usable(), p.Kind)
			}
		})
	}
}

func TestRoleWithoutSourceProfile(t *testing.T) {
	// role_arn + credential_source (Environment, Ec2InstanceMetadata, EcsContainer) or
	// + web_identity_token_file are valid assume-role setups: the SDK assumes the role
	// with the credentials of that source. login_session comes from `aws login`.
	f := loadFiles(t, `[profile acme-ci]
role_arn = arn:aws:iam::111111111111:role/Deploy
credential_source = Environment

[profile acme-oidc]
role_arn = arn:aws:iam::111111111111:role/Deploy
web_identity_token_file = /var/run/secrets/token

[profile acme-console]
login_session = arn:aws:iam::111111111111:user/alice
region = eu-west-1
`, "")
	want := map[string]Kind{"acme-ci": KindAssumeRole, "acme-oidc": KindWebIdentity, "acme-console": KindLogin}
	for name, kind := range want {
		p := f.Profile(name)
		if !p.Usable() || p.Kind != kind {
			t.Errorf("%s: kind %v, want %v", name, p.Kind, kind)
		}
	}
	if p := f.Profile("acme-ci"); p.CredentialSource != "Environment" {
		t.Errorf("credential_source: %q", p.CredentialSource)
	}
	if p := f.Profile("acme-oidc"); p.WebIdentityTokenFile != "/var/run/secrets/token" {
		t.Errorf("web_identity_token_file: %q", p.WebIdentityTokenFile)
	}
	if p := f.Profile("acme-console"); p.LoginSession != "arn:aws:iam::111111111111:user/alice" {
		t.Errorf("login_session: %q", p.LoginSession)
	}
	if got := f.Names(); !reflect.DeepEqual(got, []string{"acme-ci", "acme-oidc", "acme-console"}) {
		t.Errorf("names %v", got)
	}
	if ps := f.Portals(nil); len(ps) != 0 {
		t.Errorf("none of them uses an SSO portal: %+v", ps)
	}
}

func TestMFAProfiles(t *testing.T) {
	f := loadFiles(t, `[profile acme-prod]
region = eu-west-1

[profile acme-admin]
role_arn = arn:aws:iam::111111111111:role/Admin
source_profile = acme-prod
mfa_serial = arn:aws:iam::111111111111:mfa/alice

[profile acme-admin-2]
role_arn = arn:aws:iam::111111111111:role/Admin2
source_profile = acme-admin

[profile acme-ro]
role_arn = arn:aws:iam::111111111111:role/ReadOnly
source_profile = acme-prod

[profile acme-loop]
role_arn = arn:aws:iam::111111111111:role/Loop
source_profile = acme-loop

[profile acme-keys-mfa]
mfa_serial = arn:aws:iam::111111111111:mfa/bob
`, "[acme-prod]\naws_access_key_id = AKIAFAKEFAKEFAKE0001\naws_secret_access_key = fictional-secret\n[acme-keys-mfa]\naws_access_key_id = AKIAFAKEFAKEFAKE0002\naws_secret_access_key = fictional-secret\n")
	if p := f.Profile("acme-admin"); p.MFASerial != "arn:aws:iam::111111111111:mfa/alice" || !p.NeedsMFA() || !p.Usable() {
		t.Errorf("acme-admin: %+v", p)
	}
	cases := map[string]string{
		"acme-admin":   "acme-admin",
		"acme-admin-2": "acme-admin", // through its source_profile
		"acme-ro":      "",
		"acme-prod":    "",
		"acme-loop":    "",
		"unknown":      "",
		// mfa_serial without role_arn: the SDK ignores it.
		"acme-keys-mfa": "",
	}
	for name, want := range cases {
		got := ""
		if p := f.MFAProfile(name); p != nil {
			got = p.Name
		}
		if got != want {
			t.Errorf("MFAProfile(%s) = %q, want %q", name, got, want)
		}
	}
}

func TestSSOSessionLookup(t *testing.T) {
	f := loadFiles(t, `[profile acme-prod]
sso_session = acme-sso
sso_account_id = 111111111111
sso_role_name = ReadOnly

[profile acme-eu]
sso_session = acme-sso
sso_region = eu-central-1
sso_account_id = 222222222222
sso_role_name = ReadOnly

[profile acme-ghost]
sso_session = ghost
sso_account_id = 333333333333
sso_role_name = ReadOnly

[sso-session   acme-sso  ]
sso_start_url = https://acme.awsapps.com/start
sso_region = eu-west-1
`, "")
	if _, ok := f.Sessions["acme-sso"]; !ok {
		t.Fatalf("sessions: %+v", f.Sessions)
	}
	cases := []struct {
		profile, startURL, ssoRegion, portalKey string
	}{
		// The session is declared after the profile: it is resolved once both files
		// are read.
		{"acme-prod", "https://acme.awsapps.com/start", "eu-west-1", "acme-sso"},
		// A region set on the profile wins over the session's.
		{"acme-eu", "https://acme.awsapps.com/start", "eu-central-1", "acme-sso"},
		// An unknown session leaves the start URL empty, the profile stays SSO.
		{"acme-ghost", "", "", "ghost"},
	}
	for _, c := range cases {
		p := f.Profile(c.profile)
		if p.SSOStartURL != c.startURL || p.SSORegion != c.ssoRegion || p.PortalKey() != c.portalKey || p.Kind != KindSSO {
			t.Errorf("%s: %+v", c.profile, p)
		}
	}
}

func TestPortalKey(t *testing.T) {
	cases := []struct {
		p    Profile
		want string
	}{
		{Profile{HasSSO: true, SSOSession: "acme-sso", SSOStartURL: "https://acme.awsapps.com/start"}, "acme-sso"},
		{Profile{HasSSO: true, SSOStartURL: "https://acme.awsapps.com/start"}, "https://acme.awsapps.com/start"},
		{Profile{SSOSession: "acme-sso"}, ""},
	}
	for _, c := range cases {
		if got := c.p.PortalKey(); got != c.want {
			t.Errorf("%+v: %q, want %q", c.p, got, c.want)
		}
	}
}

func TestPortalsGrouping(t *testing.T) {
	f := loadFiles(t, `[sso-session acme-sso]
sso_start_url = https://acme.awsapps.com/start
sso_region = eu-west-1

[profile acme-prod]
sso_session = acme-sso
sso_account_id = 111111111111

[profile acme-data]
sso_session = acme-sso
sso_account_id = 222222222222

[profile acme-legacy-a]
sso_start_url = https://acme.awsapps.com/start
sso_region = eu-west-1
sso_account_id = 333333333333

[profile acme-legacy-b]
sso_start_url = https://acme.awsapps.com/start
sso_region = eu-west-1
sso_account_id = 444444444444

[profile globex]
sso_start_url = http://globex.example.com/start/#/
sso_region = us-east-1
sso_account_id = 555555555555

[profile acme-role]
role_arn = arn:aws:iam::111111111111:role/Admin
source_profile = acme-prod
sso_session = acme-sso
sso_account_id = 111111111111

[profile acme-billing]
aws_access_key_id = AKIA1
aws_secret_access_key = s1
`, "")
	ps := f.Portals(nil)
	var labels []string
	for _, p := range ps {
		labels = append(labels, p.Label()+"="+strings.Join(p.Profiles, ","))
	}
	// Sorted by label. The same start URL in two formats gives two portals: two
	// logins. An assumed role is not an SSO portal user, even with SSO keys.
	want := []string{
		"acme-sso=acme-prod,acme-data",
		"acme.awsapps.com=acme-legacy-a,acme-legacy-b",
		"globex.example.com=globex",
	}
	if !reflect.DeepEqual(labels, want) {
		t.Errorf("portals:\n got %v\nwant %v", labels, want)
	}
	if ps[1].Key != "https://acme.awsapps.com/start" || ps[1].Region != "eu-west-1" {
		t.Errorf("legacy portal: %+v", ps[1])
	}
	if got := ps[1].LoginArgs(); !reflect.DeepEqual(got, []string{"sso", "login", "--profile", "acme-legacy-a"}) {
		t.Errorf("legacy login uses the first profile: %v", got)
	}
	if got := ps[0].LoginArgs(); !reflect.DeepEqual(got, []string{"sso", "login", "--sso-session", "acme-sso"}) {
		t.Errorf("session login: %v", got)
	}

	keep := func(name string) bool { return name != "acme-prod" && name != "globex" }
	labels = nil
	for _, p := range f.Portals(keep) {
		labels = append(labels, p.Label()+"="+strings.Join(p.Profiles, ","))
	}
	want = []string{"acme-sso=acme-data", "acme.awsapps.com=acme-legacy-a,acme-legacy-b"}
	if !reflect.DeepEqual(labels, want) {
		t.Errorf("filtered portals:\n got %v\nwant %v", labels, want)
	}

	if _, ok := f.PortalOf("acme-billing"); ok {
		t.Error("a static-key profile has no portal")
	}
	if _, ok := f.PortalOf("unknown"); ok {
		t.Error("an unknown profile has no portal")
	}
	if pt, ok := f.PortalOf("acme-data"); !ok || pt.SessionName != "acme-sso" {
		t.Errorf("PortalOf(acme-data): %+v", pt)
	}
}

func TestPortalLabel(t *testing.T) {
	cases := []struct {
		p    Portal
		want string
	}{
		{Portal{SessionName: "acme-sso", StartURL: "https://acme.awsapps.com/start"}, "acme-sso"},
		{Portal{StartURL: "https://acme.awsapps.com/start#/"}, "acme.awsapps.com"},
		{Portal{StartURL: "http://globex.example.com"}, "globex.example.com"},
		{Portal{StartURL: "globex.example.com/start"}, "globex.example.com"},
		{Portal{}, ""},
	}
	for _, c := range cases {
		if got := c.p.Label(); got != c.want {
			t.Errorf("%+v: %q, want %q", c.p, got, c.want)
		}
	}
}

func TestNamesKeepsUsableProfilesInOrder(t *testing.T) {
	f := loadFiles(t, "[default]\nregion = eu-west-1\n\n[profile globex]\ncredential_process = /usr/bin/creds\n", "[acme-billing]\naws_access_key_id = AKIA1\naws_secret_access_key = s1\n")
	if got := f.Names(); !reflect.DeepEqual(got, []string{"globex", "acme-billing"}) {
		t.Errorf("names %v", got)
	}
}

func TestKindString(t *testing.T) {
	seen := map[string]bool{}
	for k := KindNone; k <= KindLogin; k++ {
		s := k.String()
		if s == "" || seen[s] {
			t.Errorf("kind %d: label %q empty or duplicated", k, s)
		}
		seen[s] = true
		if k.IsSSO() != (k == KindSSO || k == KindSSOLegacy) {
			t.Errorf("IsSSO(%v)", k)
		}
	}
}

func TestLoadMissingFiles(t *testing.T) {
	f := loadFiles(t, "", "")
	if len(f.Profiles) != 0 || f.DefaultRegion != "" || f.CommentedKeys != 0 || f.CredentialsMode != 0 {
		t.Errorf("missing files: %+v", f)
	}
}

func TestLoadUnreadablePath(t *testing.T) {
	// A directory where the file should be is an error, not an empty configuration.
	dir := t.TempDir()
	if _, err := LoadFrom(dir, filepath.Join(dir, "missing")); err == nil {
		t.Error("config path is a directory: want an error")
	}
	if _, err := LoadFrom(filepath.Join(dir, "missing"), dir); err == nil {
		t.Error("credentials path is a directory: want an error")
	}
}

func TestCredentialsMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits do not apply on Windows")
	}
	dir := t.TempDir()
	cred := filepath.Join(dir, "credentials")
	os.WriteFile(cred, []byte("[acme-billing]\n"), 0o600)
	if err := os.Chmod(cred, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := LoadFrom(filepath.Join(dir, "config"), cred)
	if err != nil {
		t.Fatal(err)
	}
	if f.CredentialsMode != 0o644 {
		t.Errorf("credentials mode %o, want 644", f.CredentialsMode)
	}
}

func TestDefaultPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("AWS_CONFIG_FILE", "")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "")
	if got, want := ConfigPath(), filepath.Join(home, ".aws", "config"); got != want {
		t.Errorf("config path %s, want %s", got, want)
	}
	if got, want := CredentialsPath(), filepath.Join(home, ".aws", "credentials"); got != want {
		t.Errorf("credentials path %s, want %s", got, want)
	}

	cfg := filepath.Join(home, "elsewhere", "config")
	cred := filepath.Join(home, "elsewhere", "credentials")
	t.Setenv("AWS_CONFIG_FILE", cfg)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", cred)
	if ConfigPath() != cfg || CredentialsPath() != cred {
		t.Errorf("environment overrides ignored: %s %s", ConfigPath(), CredentialsPath())
	}
	os.MkdirAll(filepath.Dir(cfg), 0o700)
	os.WriteFile(cfg, []byte("[profile globex]\ncredential_process = /usr/bin/creds\n"), 0o600)
	f, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if f.ConfigPath != cfg || f.Profile("globex") == nil {
		t.Errorf("Load did not read AWS_CONFIG_FILE: %+v", f)
	}
}
