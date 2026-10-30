package cli

import (
	"bytes"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/mattn/go-runewidth"
)

var ansi = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func TestPaint(t *testing.T) {
	cases := []struct {
		name     string
		tty      bool
		colorOff bool
		s        string
		want     string
	}{
		{"terminal", true, false, "ok", cGreen + "ok" + cReset},
		{"not a terminal", false, false, "ok", "ok"},
		{"NO_COLOR", true, true, "ok", "ok"},
		{"empty text stays empty", true, false, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			terminal(t, false, c.colorOff)
			if got := paint(c.tty, cGreen, c.s); got != c.want {
				t.Errorf("paint = %q, want %q", got, c.want)
			}
		})
	}
}

func TestTable(t *testing.T) {
	build := func() *table {
		tb := &table{head: []string{"NAME", "PROFILE", "ENV"}}
		tb.add(cell{"東京-app", cGreen}, cell{"acme-prod (ACME)", ""}, cell{"prod", cRed})
		tb.add(cell{"a", cDim}, cell{"", ""}, cell{"", ""})
		tb.add(cell{"acme-p-shop-tools", ""}, cell{"globex", cYellow}, cell{"test", ""})
		return tb
	}
	want := strings.Join([]string{
		"  NAME               PROFILE           ENV",
		"  東京-app           acme-prod (ACME)  prod",
		"  a",
		"  acme-p-shop-tools  globex            test",
		"",
	}, "\n")

	terminal(t, false, false)
	var plain bytes.Buffer
	build().print(&plain, false)
	if plain.String() != want {
		t.Errorf("plain table:\n%s\nwant:\n%s", plain.String(), want)
	}

	// Colors must not shift the columns: once the escape codes are removed, the
	// output is the same.
	var colored bytes.Buffer
	build().print(&colored, true)
	if !strings.Contains(colored.String(), cGreen+"東京-app"+cReset) || !strings.Contains(colored.String(), cBold+"NAME"+cReset) {
		t.Errorf("colors missing:\n%q", colored.String())
	}
	if got := ansi.ReplaceAllString(colored.String(), ""); got != want {
		t.Errorf("colored table misaligned:\n%s\nwant:\n%s", got, want)
	}

	// Every cell of the second column starts at the same display column, whatever
	// the width of the characters before it.
	for _, line := range strings.Split(want, "\n")[1:] {
		if i := strings.Index(line, "acme-prod"); i >= 0 {
			if w := runewidth.StringWidth(line[:i]); w != 2+len("acme-p-shop-tools")+2 {
				t.Errorf("second column at %d in %q", w, line)
			}
		}
	}

	terminal(t, false, true)
	var noColorOut bytes.Buffer
	build().print(&noColorOut, true)
	if strings.Contains(noColorOut.String(), "\x1b[") {
		t.Errorf("NO_COLOR ignored:\n%q", noColorOut.String())
	}
}

func TestTableHeaderOnly(t *testing.T) {
	var b bytes.Buffer
	(&table{head: []string{"PORTAL", "TOKEN"}}).print(&b, false)
	if b.String() != "  PORTAL  TOKEN\n" {
		t.Errorf("%q", b.String())
	}
}

func TestTableMoreCellsThanHeader(t *testing.T) {
	tb := &table{head: []string{"NAME"}}
	tb.add(cell{"tools", ""}, cell{"extra", ""}, cell{"x", ""})
	tb.add(cell{"a", ""})
	var b bytes.Buffer
	tb.print(&b, false)
	if want := "  NAME\n  tools  extra  x\n  a\n"; b.String() != want {
		t.Errorf("got %q, want %q", b.String(), want)
	}
}

func TestAgo(t *testing.T) {
	now := time.Now()
	cases := []struct {
		t    time.Time
		want string
	}{
		{time.Time{}, "never"},
		{now.Add(time.Hour), "just now"}, // clock skew
		{now, "just now"},
		{now.Add(-59 * time.Second), "just now"},
		{now.Add(-61 * time.Second), "1m ago"},
		{now.Add(-59*time.Minute - 50*time.Second), "59m ago"},
		{now.Add(-61 * time.Minute), "1h ago"},
		{now.Add(-47*time.Hour - 59*time.Minute), "47h ago"},
		{now.Add(-48*time.Hour - time.Minute), "2d ago"},
		{now.Add(-30 * 24 * time.Hour), "30d ago"},
	}
	for _, c := range cases {
		if got := ago(c.t); got != c.want {
			t.Errorf("ago(%s ago) = %q, want %q", time.Since(c.t).Round(time.Second), got, c.want)
		}
	}
}

func TestPluralMore(t *testing.T) {
	if got := plural(-1, "line"); got != "-1 lines" {
		t.Errorf("%q", got)
	}
	if got := plural(1, "profile/region pair"); got != "1 profile/region pair" {
		t.Errorf("%q", got)
	}
}

func TestVersionLessMore(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"1.9", "1.10", true}, // numeric, not lexical
		{"1.10", "1.9", false},
		{"1.2.497", "1.2.497.0", false},
		{"1.2.497.0", "1.2.497", false},
		{"1.2.497.0", "1.2.497.1", true},
		{"", "1.2.497.0", true},
		{"garbage", "1.2.497.0", true},
		{"2", "1.99.999.999", false},
	}
	for _, c := range cases {
		if got := versionLess(c.a, c.b); got != c.want {
			t.Errorf("versionLess(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestConfirm(t *testing.T) {
	t.Run("without a terminal, the answer is no, whatever the default", func(t *testing.T) {
		terminal(t, false, false)
		stdin(t, "y\n")
		var yes, no bool
		out := capture(t, &os.Stderr, func() { yes, no = confirm("Sign in again now?", true), confirm("Open?", false) })
		if yes || no {
			t.Error("a question nobody can answer must give no")
		}
		if out != "" {
			t.Errorf("nothing to print without a terminal: %q", out)
		}
	})
	cases := []struct {
		input string
		def   bool
		want  bool
	}{
		{"y\n", false, true},
		{"yes\n", false, true},
		{"  YES  \n", false, true},
		{"Y\n", false, true},
		{"n\n", true, false},
		{"no\n", true, false},
		{"maybe\n", true, false},
		{"\n", true, true},
		{"\n", false, false},
		{"", true, true}, // end of input
		{"y", false, true},
	}
	for _, c := range cases {
		t.Run(strings.TrimSpace(c.input)+"/default "+map[bool]string{true: "yes", false: "no"}[c.def], func(t *testing.T) {
			terminal(t, true, true)
			stdin(t, c.input)
			var got bool
			prompt := capture(t, &os.Stderr, func() { got = confirm("Open anyway?", c.def) })
			if got != c.want {
				t.Errorf("confirm(%q, %v) = %v, want %v", c.input, c.def, got, c.want)
			}
			suffix := "[y/N]"
			if c.def {
				suffix = "[Y/n]"
			}
			if !strings.Contains(prompt, "hop: Open anyway? "+suffix) {
				t.Errorf("prompt %q", prompt)
			}
		})
	}
}

func TestMessages(t *testing.T) {
	terminal(t, false, false)
	out := capture(t, &os.Stderr, func() {
		info("%d instances", 3)
		warn("account %s does NOT log sessions", "acme-prod")
		hint("hop login %s", "acme-sso")
	})
	want := "hop: 3 instances\nhop: account acme-prod does NOT log sessions\n     hop login acme-sso\n"
	if out != want {
		t.Errorf("got %q, want %q", out, want)
	}
}
