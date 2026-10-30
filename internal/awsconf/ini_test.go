package awsconf

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseINI(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		want      []section
		commented int
	}{
		{
			name: "hash and semicolon comments are skipped",
			in: `# a comment
; another comment
[default]
# region = us-east-1
; output = json
region = eu-west-1
`,
			want: []section{{name: "default", keys: map[string]string{"region": "eu-west-1"}}},
		},
		{
			name: "indented comments are skipped too",
			in:   "[default]\n   # region = us-east-1\n\t; region = us-west-2\nregion = eu-west-1\n",
			want: []section{{name: "default", keys: map[string]string{"region": "eu-west-1"}}},
		},
		{
			name: "commented-out credentials are counted, other comments are not",
			in: `# aws_access_key_id = AKIAOLD
;aws_secret_access_key=old
#   aws_session_token = tok
# region = eu-west-1
# my aws_access_key_id = not at the start
[acme-prod]
aws_access_key_id = AKIA1
`,
			want:      []section{{name: "acme-prod", keys: map[string]string{"aws_access_key_id": "AKIA1"}}},
			commented: 3,
		},
		{
			name: "a comment after a section header does not merge the section into the previous one",
			in: `[profile acme-dev]
sso_account_id = 111111111111
region = eu-west-1
[profile acme-prod] # production
sso_account_id = 222222222222
region = eu-west-3
[profile acme-test]; tests
region = us-east-1
`,
			want: []section{
				{name: "profile acme-dev", keys: map[string]string{"sso_account_id": "111111111111", "region": "eu-west-1"}},
				{name: "profile acme-prod", keys: map[string]string{"sso_account_id": "222222222222", "region": "eu-west-3"}},
				{name: "profile acme-test", keys: map[string]string{"region": "us-east-1"}},
			},
		},
		{
			name: "secret values are recorded as present, never kept",
			in:   "[acme]\naws_access_key_id = AKIA1\naws_secret_access_key = s3cr3t\naws_session_token = t0k3n\n[empty]\naws_secret_access_key =\n",
			want: []section{
				{name: "acme", keys: map[string]string{"aws_access_key_id": "AKIA1", "aws_secret_access_key": present, "aws_session_token": present}},
				{name: "empty", keys: map[string]string{"aws_secret_access_key": ""}},
			},
		},
		{
			name: "whitespace inside the section header is normalized",
			in:   "[  profile    acme-prod  ]\nregion = eu-west-1\n",
			want: []section{{name: "profile acme-prod", keys: map[string]string{"region": "eu-west-1"}}},
		},
		{
			name: "keys are lower-cased, values keep their case and inner equal signs",
			in:   "[profile globex]\nRegion = EU-West-1\nCredential_Process = /usr/bin/creds --profile=globex --format=json\n",
			want: []section{{name: "profile globex", keys: map[string]string{
				"region":             "EU-West-1",
				"credential_process": "/usr/bin/creds --profile=globex --format=json",
			}}},
		},
		{
			name: "keys before the first section and lines without an equal sign are ignored",
			in:   "region = us-east-1\n[default]\nnot a key\nregion = eu-west-1\n",
			want: []section{{name: "default", keys: map[string]string{"region": "eu-west-1"}}},
		},
		{
			name: "nested sub-section lines are skipped, the next top-level key is read",
			in: `[profile initech]
s3 =
  max_concurrent_requests = 20
	region = us-west-2
region = af-south-1
`,
			want: []section{{name: "profile initech", keys: map[string]string{"s3": "", "region": "af-south-1"}}},
		},
		{
			name: "a blank line inside a sub-section does not end it",
			in:   "[profile initech]\ns3 =\n  addressing_style = path\n\n  region = us-west-2\nregion = af-south-1\n",
			want: []section{{name: "profile initech", keys: map[string]string{"s3": "", "region": "af-south-1"}}},
		},
		{
			name: "a sub-section ends at the next section header",
			in:   "[profile initech]\ns3 =\n  region = us-west-2\n[profile globex]\n  region = us-east-1\n",
			want: []section{
				{name: "profile initech", keys: map[string]string{"s3": ""}},
				{name: "profile globex", keys: map[string]string{"region": "us-east-1"}},
			},
		},
		{
			name: "indented keys outside a sub-section are read",
			in:   "[profile globex]\n  region = us-east-1\n  sso_role_name = ReadOnly\n",
			want: []section{{name: "profile globex", keys: map[string]string{"region": "us-east-1", "sso_role_name": "ReadOnly"}}},
		},
		{
			name: "Windows line endings",
			in:   "[default]\r\nregion = eu-west-1\r\n\r\n[profile globex]\r\nregion = us-east-1\r\n",
			want: []section{
				{name: "default", keys: map[string]string{"region": "eu-west-1"}},
				{name: "profile globex", keys: map[string]string{"region": "us-east-1"}},
			},
		},
		{
			name: "a later key overrides an earlier one in the same section",
			in:   "[default]\nregion = us-east-1\nregion = eu-west-1\n",
			want: []section{{name: "default", keys: map[string]string{"region": "eu-west-1"}}},
		},
		{
			name: "duplicate sections are kept in file order",
			in:   "[default]\nregion = us-east-1\n[default]\noutput = json\n",
			want: []section{
				{name: "default", keys: map[string]string{"region": "us-east-1"}},
				{name: "default", keys: map[string]string{"output": "json"}},
			},
		},
		{
			name: "empty input",
			in:   "",
			want: nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, commented, err := parseINI(strings.NewReader(c.in))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("sections:\n got %+v\nwant %+v", got, c.want)
			}
			if commented != c.commented {
				t.Errorf("commented keys: %d, want %d", commented, c.commented)
			}
		})
	}
}

func TestParseINILongLine(t *testing.T) {
	// A credential_process with a long inline argument must not stop the scanner at
	// the default 64 KB token size.
	long := strings.Repeat("x", 200*1024)
	secs, _, err := parseINI(strings.NewReader("[profile globex]\ncredential_process = " + long + "\nregion = us-east-1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(secs) != 1 || secs[0].keys["region"] != "us-east-1" || len(secs[0].keys["credential_process"]) != len(long) {
		t.Errorf("long line misread: %d sections", len(secs))
	}
}
