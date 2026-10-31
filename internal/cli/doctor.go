package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/morganseznec/hop/internal/awsconf"
	"github.com/morganseznec/hop/internal/inventory"
	"github.com/morganseznec/hop/internal/session"
)

type level int

const (
	lvlOK level = iota
	lvlInfo
	lvlWarn
	lvlCrit
)

type finding struct {
	lvl    level
	title  string
	detail []string
	fix    []string
}

func newDoctor(f *filters) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check this machine's AWS setup and the cached session logging status",
		Long: `Check this machine's AWS setup and the cached session logging status.

Makes no network calls: reads the AWS config and credentials files, the SSO token
cache, hop's configuration and the last inventory. It checks session-manager-plugin
(1.2.497.0 or later) and the AWS CLI, then flags expired tokens, keys left in plain
text, profiles defined twice or needing MFA, and accounts that do not log sessions,
each with the command that fixes it. Run "hop ls" first for an up-to-date session
logging status.`,
		Example: `  hop doctor
  hop ls --refresh >/dev/null && hop doctor    with an up-to-date logging status`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := newApp(*f)
			if err != nil {
				return err
			}
			fs := a.diagnose()
			sort.SliceStable(fs, func(i, j int) bool { return fs[i].lvl > fs[j].lvl })
			for _, x := range fs {
				printFinding(x)
			}
			return nil
		},
	}
}

func printFinding(x finding) {
	mark, color := "✓", cGreen
	switch x.lvl {
	case lvlInfo:
		mark, color = "·", cCyan
	case lvlWarn:
		mark, color = "!", cYellow
	case lvlCrit:
		mark, color = "✗", cRed
	}
	fmt.Println(paint(stdoutTTY, color, mark) + " " + paint(stdoutTTY, cBold, x.title))
	for _, d := range x.detail {
		fmt.Println("    " + d)
	}
	for _, l := range x.fix {
		fmt.Println(paint(stdoutTTY, cDim, "    → "+l))
	}
}

func (a *app) diagnose() []finding {
	var out []finding
	add := func(x finding) { out = append(out, x) }
	files := a.files

	// --- tools ----------------------------------------------------------------------
	if p, err := session.PluginPath(); err != nil {
		add(finding{lvl: lvlCrit, title: "session-manager-plugin not found: no session can be opened",
			fix: pluginFix(runtime.GOOS, false)})
	} else if v, err := exec.Command(p, "--version").Output(); err == nil {
		ver := strings.TrimSpace(string(v))
		if versionLess(ver, "1.2.497.0") {
			add(finding{lvl: lvlWarn, title: "session-manager-plugin " + ver + " is too old",
				detail: []string{"it cannot read the session response from an environment variable"},
				fix:    pluginFix(runtime.GOOS, true)})
		} else {
			add(finding{lvl: lvlOK, title: "session-manager-plugin " + ver})
		}
	}
	if _, err := exec.LookPath("aws"); err != nil {
		add(finding{lvl: lvlInfo, title: "aws CLI not found: hop cannot open SSO portals (hop login, or when a token expires)",
			fix: []string{"install the AWS CLI v2: https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html"}})
	}
	// The shell runs the first hop on PATH, which may not be this binary.
	if onPath, err := exec.LookPath("hop"); err == nil {
		if self, err := os.Executable(); err == nil {
			first, errFirst := filepath.EvalSymlinks(onPath)
			this, errThis := filepath.EvalSymlinks(self)
			if errFirst == nil && errThis == nil && first != this {
				add(finding{lvl: lvlWarn, title: "another hop earlier on PATH shadows this one: " + first,
					detail: []string{"this binary: " + this},
					fix:    []string{"remove " + first + ", or put " + filepath.Dir(this) + " ahead of it in PATH"}})
			}
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		if b, err := os.ReadFile(filepath.Join(home, ".zshrc")); err == nil && !strings.Contains(string(b), "hop init zsh") {
			add(finding{lvl: lvlInfo, title: "zsh completion not set up",
				fix: []string{`add to ~/.zshrc, after compinit: eval "$(hop init zsh)"`}})
		}
	}

	// --- credentials file -----------------------------------------------------------
	// Windows does not report Unix permission bits: rely on the profile folder ACLs.
	if runtime.GOOS != "windows" && files.CredentialsMode&0o077 != 0 {
		add(finding{lvl: lvlCrit, title: fmt.Sprintf("%s is readable by other users (%o)", files.CredentialsPath, files.CredentialsMode),
			fix: []string{"chmod 600 " + files.CredentialsPath}})
	}
	if files.CommentedKeys > 0 {
		add(finding{lvl: lvlWarn, title: fmt.Sprintf("commented-out keys left in credentials (%s)", plural(files.CommentedKeys, "line")),
			detail: []string{"a commented-out key stays readable on disk and in backups"},
			fix:    []string{"delete these lines, and deactivate the key in IAM if it is still active"}})
	}

	if len(files.Names()) == 0 {
		add(finding{lvl: lvlCrit, title: "no AWS profile with credentials: hop has nothing to query",
			detail: []string{"none in " + files.ConfigPath + " or " + files.CredentialsPath},
			fix:    []string{`create one with "aws configure sso", then run hop ls`}})
	}

	var static, temp, dual, noRegion, mfa []string
	for _, p := range files.Profiles {
		switch {
		case !p.Usable():
			continue
		case p.HasSSO && p.HasKeys:
			dual = append(dual, p.Name)
		case p.Kind == awsconf.KindTemporary:
			temp = append(temp, p.Name)
		case p.Kind == awsconf.KindStatic:
			static = append(static, p.Name)
		}
		if p.Region == "" {
			noRegion = append(noRegion, p.Name)
		}
		if files.MFAProfile(p.Name) != nil {
			mfa = append(mfa, p.Name)
		}
	}
	if len(mfa) > 0 {
		add(finding{lvl: lvlWarn, title: fmt.Sprintf("%s need an MFA code (mfa_serial), which hop does not prompt for yet", plural(len(mfa), "profile")),
			detail: append([]string{"hop skips them:"}, wrap(mfa, 6)...),
			fix: []string{"use a profile without mfa_serial for these accounts",
				"or get the credentials from aws-vault (or another credential_process), which asks for the code"}})
	}
	for _, n := range dual {
		add(finding{lvl: lvlWarn, title: "profile " + n + " defined twice: SSO in config, keys in credentials",
			detail: []string{"hop and the aws CLI use SSO, but tools built on the SDK",
				"(Terraform, Go or Python scripts) pick the keys: two identities under one name"},
			fix: []string{"remove the [" + n + "] section from credentials if SSO is enough"}})
	}
	// Only keys read from the credentials file: deleting a section is the fix for them
	// alone. An SSO profile can get AuthFailure too, in a region its account has not
	// enabled.
	var refused []string
	for _, k := range a.keys() {
		t, ok := a.inv.Targets[k]
		if !ok || t.ErrKind != inventory.ErrBadKeys || slices.Contains(refused, t.Profile) {
			continue
		}
		if p := files.Profile(t.Profile); p != nil && p.KeysInCredentials && (p.Kind == awsconf.KindStatic || p.Kind == awsconf.KindTemporary) {
			refused = append(refused, t.Profile)
		}
	}
	if len(refused) > 0 {
		add(finding{lvl: lvlWarn, title: "keys rejected by AWS during the last inventory (revoked or expired)",
			detail: wrap(refused, 6),
			fix:    []string{"delete these sections from credentials, or list them under exclude: in " + configFile()}})
	}
	if len(temp) > 0 {
		add(finding{lvl: lvlWarn, title: "temporary keys pasted from the SSO portal: " + strings.Join(temp, ", "),
			detail: []string{"they expire within hours and then stay in the file"},
			fix:    []string{"delete these sections: the SSO profile for the same account does the same job"}})
	}
	if len(static) > 0 {
		add(finding{lvl: lvlWarn, title: fmt.Sprintf("long-lived plaintext keys in %s", plural(len(static), "profile")),
			detail: wrap(static, 6),
			fix: []string{"move these accounts to SSO (IAM Identity Center) where possible",
				"otherwise: credential_process backed by the macOS keychain, and key rotation"}})
	}
	if len(noRegion) > 0 {
		def := a.cfg.Regions("", "", files.DefaultRegion)[0]
		add(finding{lvl: lvlInfo, title: fmt.Sprintf("no region set on %s: using %s", plural(len(noRegion), "profile"), def),
			detail: wrap(noRegion, 6),
			fix:    []string{`if an account lives elsewhere, "hop scan" will find it`}})
	}

	// --- SSO portals ----------------------------------------------------------------
	portals := files.Portals(nil)
	byURL := map[string][]awsconf.Portal{}
	var urls []string
	for _, pt := range portals {
		if _, ok := byURL[pt.StartURL]; !ok {
			urls = append(urls, pt.StartURL)
		}
		byURL[pt.StartURL] = append(byURL[pt.StartURL], pt)
	}
	for _, pt := range portals {
		tok := pt.Token()
		lvl := lvlOK
		if tok.State != awsconf.TokenValid {
			lvl = lvlInfo
		}
		x := finding{lvl: lvl, title: fmt.Sprintf("SSO portal %s: token %s (%s)", pt.Label(), tok, plural(len(pt.Profiles), "profile"))}
		if tok.State != awsconf.TokenValid {
			x.fix = []string{"hop login " + pt.Label()}
		}
		add(x)
	}
	for _, url := range urls {
		group := byURL[url]
		if len(group) < 2 {
			continue
		}
		var session, host string
		var legacy []string
		for _, pt := range group {
			if pt.SessionName != "" {
				session = pt.SessionName
			} else {
				// A legacy portal is labeled with the host of its start URL.
				host = pt.Label()
				legacy = append(legacy, pt.Profiles...)
			}
		}
		if session == "" || len(legacy) == 0 {
			continue
		}
		add(finding{lvl: lvlWarn, title: "SSO portal " + host + " is declared in two formats: two logins for one portal",
			detail: []string{"legacy format: " + strings.Join(legacy, ", "), "sso_session format: " + session},
			fix: []string{"in " + files.ConfigPath + ", for each legacy profile, replace",
				"sso_start_url and sso_region with: sso_session = " + session}})
	}

	// --- accounts, from the last inventory ------------------------------------------
	// Pairs never checked, and pairs whose last query failed, grouped by cause: the fix
	// differs (hop login for an expired token, a later refresh when throttled).
	var notLogged, unknown []string
	failed := map[inventory.ErrKind][]string{}
	var causes []inventory.ErrKind
	nFailed := 0
	for _, k := range a.keys() {
		t, ok := a.inv.Targets[k]
		if !ok {
			unknown = append(unknown, strings.ReplaceAll(k, "|", " "))
			continue
		}
		switch {
		case t.Transcript.Checked:
			if !t.Transcript.Enabled && len(t.Instances) > 0 {
				notLogged = append(notLogged, fmt.Sprintf("%s %s (%s)", t.Profile, t.Region, plural(len(t.Instances), "instance")))
			}
		case t.Err != "":
			if _, seen := failed[t.ErrKind]; !seen {
				causes = append(causes, t.ErrKind)
			}
			failed[t.ErrKind] = append(failed[t.ErrKind], t.Profile+" "+t.Region)
			nFailed++
		default:
			unknown = append(unknown, t.Profile+" "+t.Region)
		}
	}
	if len(notLogged) > 0 {
		add(finding{lvl: lvlCrit, title: "sessions NOT logged on these accounts",
			detail: notLogged,
			fix: []string{"configure SSM-SessionManagerRunShell (KMS-encrypted CloudWatch log group)",
				"or set transcript: require in " + configFile() + " to refuse these sessions"}})
	}
	if n := len(unknown) + nFailed; n > 0 {
		detail := wrap(unknown, 4)
		for _, cause := range causes {
			lines := wrap(failed[cause], 4)
			detail = append(detail, cause.Explain()+": "+lines[0])
			for _, l := range lines[1:] {
				detail = append(detail, "  "+l)
			}
		}
		fix := "hop ls --refresh"
		if _, ok := failed[inventory.ErrSSOExpired]; ok {
			fix = "hop login, then hop ls --refresh"
		}
		add(finding{lvl: lvlInfo, title: fmt.Sprintf("session logging not checked for %s", plural(n, "profile/region pair")),
			detail: detail, fix: []string{fix}})
	}

	names := map[string][]inventory.Instance{}
	var noAgent []string
	for _, in := range a.inv.Instances(a.keys()) {
		if !in.Running() {
			continue
		}
		names[in.Profile+"|"+in.Region+"|"+in.Name] = append(names[in.Profile+"|"+in.Region+"|"+in.Name], in)
		if t := a.target(in); t != nil && t.AgentsKnown && in.Agent == "" {
			noAgent = append(noAgent, in.Name+" ("+in.Profile+")")
		}
	}
	var dupes []string
	for _, list := range names {
		if len(list) > 1 {
			dupes = append(dupes, fmt.Sprintf("%s: %d instances in %s", list[0].Name, len(list), list[0].Profile))
		}
	}
	sort.Strings(dupes)
	if len(dupes) > 0 {
		add(finding{lvl: lvlInfo, title: "duplicate instance names (hop will ask which one)", detail: dupes})
	}
	if len(noAgent) > 0 {
		sort.Strings(noAgent)
		add(finding{lvl: lvlInfo, title: "running instances without the SSM agent, unreachable by hop", detail: wrap(noAgent, 3)})
	}
	return out
}

// plural formats a count with an English noun: "1 profile", "3 profiles".
func plural(n int, word string) string {
	if n != 1 {
		return fmt.Sprintf("%d %ss", n, word)
	}
	return fmt.Sprintf("%d %s", n, word)
}

// pluginFix tells how to install (or upgrade) session-manager-plugin: Homebrew on
// macOS, the AWS installation guide elsewhere.
func pluginFix(goos string, upgrade bool) []string {
	if goos == "darwin" {
		if upgrade {
			return []string{"brew upgrade --cask session-manager-plugin"}
		}
		return []string{"brew install --cask session-manager-plugin"}
	}
	verb := "install"
	if upgrade {
		verb = "update"
	}
	return []string{verb + " session-manager-plugin: https://docs.aws.amazon.com/systems-manager/latest/userguide/session-manager-working-with-install-plugin.html"}
}

// wrap groups a list into lines of n items.
func wrap(items []string, n int) []string {
	var out []string
	for i := 0; i < len(items); i += n {
		out = append(out, strings.Join(items[i:min(i+n, len(items))], ", "))
	}
	return out
}

func versionLess(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < max(len(pa), len(pb)); i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			return x < y
		}
	}
	return false
}
