package cli

import (
	"net"
	"strconv"
	"testing"
)

func TestVersionLess(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"1.2.764.0", "1.2.497.0", false},
		{"1.2.463.0", "1.2.497.0", true},
		{"1.2.497.0", "1.2.497.0", false},
		{"1.3", "1.2.999.0", false},
	}
	for _, c := range cases {
		if got := versionLess(c.a, c.b); got != c.want {
			t.Errorf("versionLess(%s, %s) = %v", c.a, c.b, got)
		}
	}
}

func TestPortFallback(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip(err)
	}
	defer l.Close()
	busy := strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	if portFree(busy) {
		t.Fatalf("port %s is in use but reported free", busy)
	}
	next := nextFreePort(busy, portFree)
	if next == "" || next == busy || !portFree(next) {
		t.Errorf("fallback port: %q", next)
	}
}

func TestPlural(t *testing.T) {
	cases := []struct {
		n    int
		word string
		want string
	}{
		{1, "profile", "1 profile"},
		{3, "profile", "3 profiles"},
		{0, "instance", "0 instances"},
		{2, "profile/region pair", "2 profile/region pairs"},
	}
	for _, c := range cases {
		if got := plural(c.n, c.word); got != c.want {
			t.Errorf("plural(%d, %q) = %q, want %q", c.n, c.word, got, c.want)
		}
	}
}
