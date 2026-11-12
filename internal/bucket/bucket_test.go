package bucket

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"hash/crc64"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/morganseznec/hop/internal/fakes3"
)

// newFake starts a fake S3 with the bucket acme-logs, and returns it with a
// configuration that reaches nothing else: fictional keys, no file of the machine.
func newFake(t *testing.T) (*fakes3.Server, aws.Config) {
	t.Helper()
	dir := t.TempDir()
	for k, v := range map[string]string{
		"AWS_CONFIG_FILE": filepath.Join(dir, "config"), "AWS_SHARED_CREDENTIALS_FILE": filepath.Join(dir, "credentials"),
		"AWS_EC2_METADATA_DISABLED": "true", "AWS_PROFILE": "", "AWS_ENDPOINT_URL": "", "AWS_ENDPOINT_URL_S3": "",
	} {
		t.Setenv(k, v)
	}
	srv := fakes3.New(t)
	srv.AddBucket("acme-logs", "eu-west-1")
	cfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion("eu-west-1"),
		awsconfig.WithBaseEndpoint(srv.URL),
		awsconfig.WithRetryMaxAttempts(1),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("AKIAFAKEFAKEFAKE0001", "fictional-secret", "")))
	if err != nil {
		t.Fatal(err)
	}
	return srv, cfg
}

// random returns n bytes that no two calls share.
func random(n int, seed uint64) []byte {
	data := make([]byte, n)
	r := rand.New(rand.NewPCG(seed, 7))
	for i := range data {
		data[i] = byte(r.IntN(256))
	}
	return data
}

func leftovers(t *testing.T, dir string) []string {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(dir, ".hop-s3-*"))
	return m
}

func TestParse(t *testing.T) {
	cases := []struct {
		in          string
		bucket, key string
		wantErr     bool
	}{
		{"acme-logs/2026/app.log", "acme-logs", "2026/app.log", false},
		{"s3://acme-logs/2026/app.log", "acme-logs", "2026/app.log", false},
		{"S3://acme-logs/2026/", "acme-logs", "2026/", false},
		{"acme-logs/", "acme-logs", "", false},
		{"acme-logs", "acme-logs", "", false},
		{"s3://acme-logs", "acme-logs", "", false},
		// Every bucket.
		{"s3://", "", "", false},
		// A key is kept as it is, double slashes and spaces included.
		{"acme-logs//a b/ c", "acme-logs", "/a b/ c", false},
		{"logs/2026", "logs", "2026", false},
		{"", "", "", true},
		{"/2026/app.log", "", "", true},
		{"s3:///app.log", "", "", true},
		{"acme logs/app.log", "", "", true},
		{"acme\x1b[2Jlogs/app.log", "", "", true},
	}
	for _, c := range cases {
		got, err := Parse(c.in)
		if (err != nil) != c.wantErr || got.Bucket != c.bucket || got.Key != c.key {
			t.Errorf("Parse(%q) = %+v, %v; want bucket %q, key %q, error %v", c.in, got, err, c.bucket, c.key, c.wantErr)
		}
	}
	if s := (Location{Bucket: "acme-logs", Key: "2026/app.log"}).String(); s != "s3://acme-logs/2026/app.log" {
		t.Errorf("String() = %q", s)
	}
}

func TestIsLocation(t *testing.T) {
	for arg, want := range map[string]bool{
		"s3://": true, "S3://acme-logs": true, "acme-logs/": true, "acme-logs/2026": true,
		"acme-logs": false, "logs": false, "s3": false, "": false,
	} {
		if got := IsLocation(arg); got != want {
			t.Errorf("IsLocation(%q) = %v", arg, got)
		}
	}
}

func FuzzParse(f *testing.F) {
	for _, s := range []string{"acme-logs/2026/app.log", "s3://acme-logs/", "s3://", "", "/", "a/b//c", "S3://x", "s3:/x/y", "a b/c"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		loc, err := Parse(s)
		if err != nil {
			if loc != (Location{}) {
				t.Errorf("Parse(%q) failed with a location: %+v", s, loc)
			}
			return
		}
		if loc.Bucket == "" {
			if loc.Key != "" || !HasScheme(s) {
				t.Errorf("Parse(%q) = %+v: a key without a bucket", s, loc)
			}
			return
		}
		if strings.ContainsAny(loc.Bucket, "/ \t\n") {
			t.Errorf("Parse(%q): bucket %q", s, loc.Bucket)
		}
		// The key is the input after the first "/", untouched.
		if !strings.HasSuffix(s, loc.Key) {
			t.Errorf("Parse(%q): key %q is not the end of the input", s, loc.Key)
		}
	})
}

func TestCRC64NVME(t *testing.T) {
	// The check value of CRC-64/NVME: the checksum of "123456789".
	if got := crc64.Checksum([]byte("123456789"), crc64NVME); got != 0xae8b14860a799888 {
		t.Errorf("CRC64NVME of the check string = %#x", got)
	}
}

func TestStored(t *testing.T) {
	sum := fakes3.Sum("SHA256", []byte("x"))
	cases := []struct {
		name string
		head s3.HeadObjectOutput
		alg  string
	}{
		{"none", s3.HeadObjectOutput{}, ""},
		{"sha256", s3.HeadObjectOutput{ChecksumSHA256: aws.String(sum), ChecksumType: types.ChecksumTypeFullObject}, "SHA256"},
		{"crc64 without a type", s3.HeadObjectOutput{ChecksumCRC64NVME: aws.String("AAAAAAAAAAA=")}, "CRC64NVME"},
		{"sha256 before crc32", s3.HeadObjectOutput{ChecksumCRC32: aws.String("AAAAAA=="), ChecksumSHA256: aws.String(sum)}, "SHA256"},
		{"composite", s3.HeadObjectOutput{ChecksumSHA256: aws.String(sum + "-3"), ChecksumType: types.ChecksumTypeComposite}, ""},
		// An older multipart object: no type, but the "-parts" suffix.
		{"composite by its suffix", s3.HeadObjectOutput{ChecksumCRC32: aws.String("AAAAAA==-12")}, ""},
		// An algorithm hop does not compute.
		{"sha1 only", s3.HeadObjectOutput{ChecksumSHA1: aws.String("AAAAAAAAAAAAAAAAAAAAAAAAAAA=")}, ""},
	}
	for _, c := range cases {
		alg, _, ok := stored(&c.head)
		if alg.name != c.alg || ok != (c.alg != "") {
			t.Errorf("%s: stored = %q, %v", c.name, alg.name, ok)
		}
	}
}

func TestList(t *testing.T) {
	srv, cfg := newFake(t)
	srv.PageSize = 2 // several pages
	for _, k := range []string{"2026/11/app.log", "2026/11/db.log", "2026/12/app.log", "2026/readme.txt", "2026-old.txt", "top.txt", "a b/c+d.txt", "odd/\x01key"} {
		srv.Put("acme-logs", k, []byte(k), "")
	}
	keys := func(objs []Object) string {
		var out []string
		for _, o := range objs {
			k := o.Key
			if o.Prefix {
				k += " (prefix)"
			}
			out = append(out, k)
		}
		return strings.Join(out, "|")
	}

	var seen []int
	top, err := List(context.Background(), cfg, Location{Bucket: "acme-logs"}, false, func(n int) { seen = append(seen, n) })
	if err != nil {
		t.Fatal(err)
	}
	if got, want := keys(top), "2026/ (prefix)|2026-old.txt|a b/ (prefix)|odd/ (prefix)|top.txt"; got != want {
		t.Errorf("top of the bucket:\n got %s\nwant %s", got, want)
	}
	if len(seen) != 3 || seen[len(seen)-1] != 5 {
		t.Errorf("progress after each page: %v", seen)
	}

	under, err := List(context.Background(), cfg, Location{Bucket: "acme-logs", Key: "2026/"}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := keys(under), "2026/11/ (prefix)|2026/12/ (prefix)|2026/readme.txt"; got != want {
		t.Errorf("under 2026/:\n got %s\nwant %s", got, want)
	}
	if o := under[2]; o.Size != int64(len("2026/readme.txt")) || o.StorageClass != "STANDARD" || o.Modified.IsZero() {
		t.Errorf("object details: %+v", o)
	}

	// A prefix that stops in the middle of a name, as "aws s3 ls" accepts.
	partial, err := List(context.Background(), cfg, Location{Bucket: "acme-logs", Key: "2026"}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := keys(partial), "2026/ (prefix)|2026-old.txt"; got != want {
		t.Errorf("keys that start with 2026:\n got %s\nwant %s", got, want)
	}

	all, err := List(context.Background(), cfg, Location{Bucket: "acme-logs", Key: "2026/"}, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := keys(all), "2026/11/app.log|2026/11/db.log|2026/12/app.log|2026/readme.txt"; got != want {
		t.Errorf("recursive:\n got %s\nwant %s", got, want)
	}

	// Keys that XML or a URL would mangle come back as they are.
	odd, err := List(context.Background(), cfg, Location{Bucket: "acme-logs", Key: "a b/"}, true, nil)
	if err != nil || keys(odd) != "a b/c+d.txt" {
		t.Errorf("key with a space and a plus: %s, %v", keys(odd), err)
	}
	ctl, err := List(context.Background(), cfg, Location{Bucket: "acme-logs", Key: "odd/"}, true, nil)
	if err != nil || keys(ctl) != "odd/\x01key" {
		t.Errorf("key with a control character: %q, %v", keys(ctl), err)
	}

	empty, err := List(context.Background(), cfg, Location{Bucket: "acme-logs", Key: "nothing/"}, false, nil)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Errorf("empty prefix: %v, %v", empty, err)
	}
}

func TestListErrors(t *testing.T) {
	_, cfg := newFake(t)
	_, err := List(context.Background(), cfg, Location{Bucket: "acme-gone"}, false, nil)
	if err == nil || err.Error() != "no bucket named acme-gone" {
		t.Errorf("missing bucket: %v", err)
	}
}

func TestDownload(t *testing.T) {
	for _, c := range []struct {
		name, checksum string
		size           int
		composite      bool
		want           string
	}{
		{"small with SHA-256", "SHA256", 300_000, false, "SHA256"},
		{"several ranges with CRC64NVME", "CRC64NVME", 2*PartSize + 12_345, false, "CRC64NVME"},
		{"exactly one range", "CRC32C", PartSize, false, "CRC32C"},
		{"CRC32", "CRC32", 1000, false, "CRC32"},
		{"no checksum", "", 300_000, false, ""},
		{"checksum of the parts only", "SHA256", 300_000, true, ""},
		{"empty", "CRC32", 0, false, "CRC32"},
		{"empty without a checksum", "", 0, false, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv, cfg := newFake(t)
			data := random(c.size, 1)
			srv.Put("acme-logs", "2026/app.bin", data, c.checksum).Composite = c.composite
			dir := t.TempDir()
			local := filepath.Join(dir, "app.bin")

			var mu sync.Mutex
			var last, total int64
			res, err := Download(context.Background(), cfg, Location{Bucket: "acme-logs", Key: "2026/app.bin"}, local, func(done, all int64) {
				mu.Lock()
				defer mu.Unlock()
				if done > all {
					t.Errorf("progress past the size: %d of %d", done, all)
				}
				last, total = max(last, done), all
			})
			if err != nil {
				t.Fatal(err)
			}
			if res.Size != int64(c.size) || res.Checksum != c.want {
				t.Errorf("result %+v, want size %d and checksum %q", res, c.size, c.want)
			}
			got, err := os.ReadFile(local)
			if err != nil || !bytes.Equal(got, data) {
				t.Fatalf("content differs (%d bytes, %v)", len(got), err)
			}
			if c.size > 0 && (last != int64(c.size) || total != int64(c.size)) {
				t.Errorf("progress ended at %d of %d", last, total)
			}
			if l := leftovers(t, dir); len(l) != 0 {
				t.Errorf("temporary files left: %v", l)
			}
			if runtime.GOOS != "windows" {
				if st, _ := os.Stat(local); st.Mode().Perm() != 0o600 {
					t.Errorf("a new file must be readable by its owner only, got %v", st.Mode().Perm())
				}
			}
			if want := (c.size + PartSize - 1) / PartSize; srv.Count("GetObject") != want {
				t.Errorf("%d GetObject calls, want %d ranges: %v", srv.Count("GetObject"), want, srv.Calls())
			}
		})
	}
}

func TestDownloadReplacesAndKeepsTheMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no permission bits to keep")
	}
	srv, cfg := newFake(t)
	srv.Put("acme-logs", "app.conf", []byte("new"), "SHA256")
	local := filepath.Join(t.TempDir(), "app.conf")
	os.WriteFile(local, []byte("old content"), 0o644)
	if _, err := Download(context.Background(), cfg, Location{Bucket: "acme-logs", Key: "app.conf"}, local, nil); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(local)
	if b, _ := os.ReadFile(local); string(b) != "new" || st.Mode().Perm() != 0o644 {
		t.Errorf("content %q, mode %v", b, st.Mode().Perm())
	}
}

func TestDownloadFails(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*fakes3.Server)
		key   string
		want  string
		is    error
	}{
		{"missing object", func(*fakes3.Server) {}, "nope.log", "no such object: s3://acme-logs/nope.log", ErrNotFound},
		{"corrupted on the way", func(s *fakes3.Server) { s.Corrupt = true }, "app.bin", "checksum mismatch after download (SHA256)", nil},
		{"replaced meanwhile", func(s *fakes3.Server) { s.Changed = true }, "big.bin", "s3://acme-logs/big.bin changed while it was downloaded", nil},
		{"archived", func(s *fakes3.Server) { s.Get("acme-logs", "app.bin").StorageClass = "GLACIER" }, "app.bin", "s3://acme-logs/app.bin is archived", nil},
		{"not allowed to read", func(s *fakes3.Server) { s.DenyHead = true }, "app.bin", "IAM denied reading s3://acme-logs/app.bin", ErrDenied},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, cfg := newFake(t)
			srv.Put("acme-logs", "app.bin", random(300_000, 2), "SHA256")
			srv.Put("acme-logs", "big.bin", random(PartSize+10, 3), "SHA256")
			c.setup(srv)
			dir := t.TempDir()
			local := filepath.Join(dir, "out.bin")
			os.WriteFile(local, []byte("kept"), 0o600)
			_, err := Download(context.Background(), cfg, Location{Bucket: "acme-logs", Key: c.key}, local, nil)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %v, want %q", err, c.want)
			}
			if c.is != nil && !errors.Is(err, c.is) {
				t.Errorf("error %v is not %v", err, c.is)
			}
			// Neither a partial file, nor a destination changed.
			if b, _ := os.ReadFile(local); string(b) != "kept" {
				t.Errorf("the destination changed: %d bytes", len(b))
			}
			if l := leftovers(t, dir); len(l) != 0 {
				t.Errorf("temporary files left: %v", l)
			}
		})
	}
}

func TestDownloadInterrupted(t *testing.T) {
	srv, cfg := newFake(t)
	srv.Put("acme-logs", "big.bin", random(3*PartSize, 4), "SHA256")
	dir := t.TempDir()
	local := filepath.Join(dir, "big.bin")
	ctx, cancel := context.WithCancel(context.Background())
	_, err := Download(ctx, cfg, Location{Bucket: "acme-logs", Key: "big.bin"}, local, func(done, total int64) { cancel() })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error %v", err)
	}
	if _, err := os.Stat(local); err == nil {
		t.Error("an interrupted download left a file")
	}
	if l := leftovers(t, dir); len(l) != 0 {
		t.Errorf("temporary files left: %v", l)
	}
}

func writeFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestUpload(t *testing.T) {
	for _, c := range []struct {
		name      string
		size      int
		multipart bool
	}{
		{"small", 300_000, false},
		{"empty", 0, false},
		{"just under the threshold", MultipartThreshold - 1, false},
		{"multipart", MultipartThreshold + 2*PartSize + 123, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv, cfg := newFake(t)
			data := random(c.size, 5)
			local := writeFile(t, "dump.html", data)
			loc := Location{Bucket: "acme-logs", Key: "db/dump.html"}

			var mu sync.Mutex
			var last int64
			digest := sha256.New()
			res, err := Upload(context.Background(), cfg, local, loc, UploadOptions{Digest: digest, Progress: func(done, total int64) {
				mu.Lock()
				defer mu.Unlock()
				if total != int64(c.size) || done > total {
					t.Errorf("progress %d of %d, size %d", done, total, c.size)
				}
				last = max(last, done)
			}})
			if err != nil {
				t.Fatal(err)
			}
			if res.Size != int64(c.size) || res.Checksum != "CRC32" {
				t.Errorf("result %+v", res)
			}
			o := srv.Get("acme-logs", "db/dump.html")
			if o == nil || !bytes.Equal(o.Data, data) {
				t.Fatalf("the object is not the file sent")
			}
			if last != int64(c.size) {
				t.Errorf("progress ended at %d of %d", last, c.size)
			}
			// The digest saw the whole file, in order, also when parts go in parallel.
			if want := sha256.Sum256(data); !bytes.Equal(digest.Sum(nil), want[:]) {
				t.Error("the digest is not the one of the file")
			}
			if got := srv.Count("CreateMultipartUpload") == 1; got != c.multipart {
				t.Errorf("multipart %v, want %v: %v", got, c.multipart, srv.Calls())
			}
			if c.multipart {
				if want := (c.size + PartSize - 1) / PartSize; srv.Count("UploadPart") != want || srv.Count("CompleteMultipartUpload") != 1 {
					t.Errorf("want %d parts and one completion: %v", want, srv.Calls())
				}
			}
			if srv.OpenUploads() != 0 {
				t.Error("a multipart upload stayed open")
			}
		})
	}
}

func TestUploadNeverReplacesUnasked(t *testing.T) {
	for _, size := range []int{1000, MultipartThreshold + 10} {
		srv, cfg := newFake(t)
		srv.Put("acme-logs", "app.conf", []byte("the one in place"), "")
		local := writeFile(t, "app.conf", random(size, 6))
		loc := Location{Bucket: "acme-logs", Key: "app.conf"}

		_, err := Upload(context.Background(), cfg, local, loc, UploadOptions{})
		if !errors.Is(err, ErrExists) || err.Error() != "s3://acme-logs/app.conf already exists" {
			t.Fatalf("size %d: error %v", size, err)
		}
		if o := srv.Get("acme-logs", "app.conf"); string(o.Data) != "the one in place" {
			t.Fatalf("size %d: the object was replaced", size)
		}
		// A multipart upload refused at its end is aborted: its parts do not stay.
		if srv.OpenUploads() != 0 {
			t.Errorf("size %d: a multipart upload stayed open: %v", size, srv.Calls())
		}

		if _, err := Upload(context.Background(), cfg, local, loc, UploadOptions{Replace: true}); err != nil {
			t.Fatalf("size %d, replace: %v", size, err)
		}
		if o := srv.Get("acme-logs", "app.conf"); len(o.Data) != size {
			t.Errorf("size %d: the object was not replaced", size)
		}
	}
}

func TestUploadFailureAbortsTheMultipartUpload(t *testing.T) {
	srv, cfg := newFake(t)
	srv.FailPart = 2
	local := writeFile(t, "dump.bin", random(MultipartThreshold+PartSize, 7))
	_, err := Upload(context.Background(), cfg, local, Location{Bucket: "acme-logs", Key: "dump.bin"}, UploadOptions{})
	if !errors.Is(err, ErrDenied) || !strings.Contains(err.Error(), "IAM denied writing s3://acme-logs/dump.bin") {
		t.Fatalf("error %v", err)
	}
	if srv.Count("AbortMultipartUpload") != 1 || srv.OpenUploads() != 0 {
		t.Errorf("the upload was not aborted: %v", srv.Calls())
	}
	if srv.Get("acme-logs", "dump.bin") != nil {
		t.Error("an object was written")
	}
}

func TestUploadInterruptedAbortsTheMultipartUpload(t *testing.T) {
	srv, cfg := newFake(t)
	local := writeFile(t, "dump.bin", random(MultipartThreshold+3*PartSize, 8))
	ctx, cancel := context.WithCancel(context.Background())
	_, err := Upload(ctx, cfg, local, Location{Bucket: "acme-logs", Key: "dump.bin"}, UploadOptions{Progress: func(done, total int64) { cancel() }})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error %v", err)
	}
	// The abort goes out with a context of its own: Ctrl-C must not leave parts behind.
	if srv.Count("AbortMultipartUpload") != 1 || srv.OpenUploads() != 0 {
		t.Errorf("the upload was not aborted: %v", srv.Calls())
	}
}

func TestUploadRefusesWhatIsNotAFile(t *testing.T) {
	_, cfg := newFake(t)
	if _, err := Upload(context.Background(), cfg, t.TempDir(), Location{Bucket: "acme-logs", Key: "x"}, UploadOptions{}); err == nil {
		t.Error("a directory was accepted")
	}
	if _, err := Upload(context.Background(), cfg, filepath.Join(t.TempDir(), "missing"), Location{Bucket: "acme-logs", Key: "x"}, UploadOptions{}); err == nil {
		t.Error("a missing file was accepted")
	}
}

func TestHead(t *testing.T) {
	srv, cfg := newFake(t)
	srv.Put("acme-logs", "app.log", []byte("hello"), "SHA256")
	info, err := Head(context.Background(), cfg, Location{Bucket: "acme-logs", Key: "app.log"})
	if err != nil || info.Size != 5 || info.Modified.IsZero() || info.StorageClass != "STANDARD" {
		t.Errorf("info %+v, %v", info, err)
	}
	if _, err := Head(context.Background(), cfg, Location{Bucket: "acme-logs", Key: "nope"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing object: %v", err)
	}
	srv.DenyHead = true
	if _, err := Head(context.Background(), cfg, Location{Bucket: "acme-logs", Key: "app.log"}); !errors.Is(err, ErrDenied) {
		t.Errorf("denied: %v", err)
	}
}

func TestPresignAndDelete(t *testing.T) {
	srv, cfg := newFake(t)
	srv.Put("acme-logs", "tmp/hop-relay-0abc", []byte("the file"), "")
	loc := Location{Bucket: "acme-logs", Key: "tmp/hop-relay-0abc"}

	url, err := Presign(context.Background(), cfg, loc, 7*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	// Signed for seven minutes, and enough on its own to download the object: no call
	// was made to produce it.
	if !strings.Contains(url, "X-Amz-Expires=420") || !strings.Contains(url, "X-Amz-Signature=") || !strings.Contains(url, "/acme-logs/tmp/hop-relay-0abc") {
		t.Errorf("URL %s", url)
	}
	if len(srv.Calls()) != 0 {
		t.Errorf("signing called S3: %v", srv.Calls())
	}
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "the file" {
		t.Errorf("GET of the presigned URL: %d %q", resp.StatusCode, body)
	}

	srv.DenyDelete = true
	if err := Delete(context.Background(), cfg, loc); !errors.Is(err, ErrDenied) || !strings.Contains(err.Error(), "IAM denied deleting s3://acme-logs/tmp/hop-relay-0abc") {
		t.Errorf("denied: %v", err)
	}
	srv.DenyDelete = false
	if err := Delete(context.Background(), cfg, loc); err != nil || srv.Get("acme-logs", "tmp/hop-relay-0abc") != nil {
		t.Errorf("delete: %v", err)
	}
}
