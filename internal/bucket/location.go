// Package bucket lists, downloads and uploads the objects of S3 buckets.
package bucket

import (
	"fmt"
	"strings"
	"unicode"
)

// Scheme marks an S3 location on the command line.
const Scheme = "s3://"

// Location names a bucket and, inside it, a key or the start of keys.
type Location struct {
	Bucket string
	Key    string
}

func (l Location) String() string { return Scheme + l.Bucket + "/" + l.Key }

// IsLocation reports whether an argument names a place in S3 rather than words to
// search for: it starts with s3://, or holds a "/".
func IsLocation(arg string) bool {
	return HasScheme(arg) || strings.Contains(arg, "/")
}

// HasScheme reports whether an argument starts with s3://, in any case.
func HasScheme(arg string) bool {
	return len(arg) >= len(Scheme) && strings.EqualFold(arg[:len(Scheme)], Scheme)
}

// Parse reads "bucket/key", "bucket/" or "bucket", with or without s3:// in front.
// "s3://" alone is the zero Location: every bucket.
//
// The bucket part may be a word to search the buckets with ("logs/2026/"), so only what
// no bucket name can hold is refused: a space or a control character. The key is kept
// as it is: an S3 key may hold any character, "//" included.
func Parse(s string) (Location, error) {
	rest := s
	if HasScheme(s) {
		rest = s[len(Scheme):]
	}
	name, key, _ := strings.Cut(rest, "/")
	if name == "" {
		if key != "" || !HasScheme(s) {
			return Location{}, fmt.Errorf("no bucket name in %q: expected bucket/key or s3://bucket/key", s)
		}
		return Location{}, nil
	}
	for _, r := range name {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return Location{}, fmt.Errorf("invalid bucket name in %q", s)
		}
	}
	return Location{Bucket: name, Key: key}, nil
}
