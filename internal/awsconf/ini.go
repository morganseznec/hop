// Package awsconf reads ~/.aws/config and ~/.aws/credentials to list the profiles,
// classify them by credential source and group them by SSO portal.
//
// The SDK can only load a profile whose name is already known: it neither lists the
// profiles nor reports that the same name is defined in both files. Hence this small
// INI reader, deliberately limited to what those two files contain.
package awsconf

import (
	"bufio"
	"io"
	"regexp"
	"strings"
)

type section struct {
	name string
	keys map[string]string
}

// A commented-out key that still holds a credential remains readable on disk: it is
// counted for diagnostics, without ever reading its value.
var commentedKey = regexp.MustCompile(`^[#;]\s*aws_(access_key_id|secret_access_key|session_token)\s*=`)

// parseINI returns the sections in file order. Indented sub-sections (`s3 =` followed
// by indented lines) are skipped: none of them concerns credentials.
func parseINI(r io.Reader) (sections []section, commented int, err error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	var cur *section
	nested := false
	for sc.Scan() {
		raw := sc.Text()
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if line[0] == '#' || line[0] == ';' {
			if commentedKey.MatchString(line) {
				commented++
			}
			continue
		}
		if line[0] == '[' && strings.HasSuffix(line, "]") {
			name := strings.Join(strings.Fields(line[1:len(line)-1]), " ")
			sections = append(sections, section{name: name, keys: map[string]string{}})
			cur = &sections[len(sections)-1]
			nested = false
			continue
		}
		if cur == nil {
			continue
		}
		if nested && (raw[0] == ' ' || raw[0] == '\t') {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.ToLower(strings.TrimSpace(k))
		v = strings.TrimSpace(v)
		nested = v == ""
		cur.keys[k] = v
	}
	return sections, commented, sc.Err()
}
