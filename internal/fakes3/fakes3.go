// Package fakes3 plays S3 for the tests, on the loopback interface: buckets, listings,
// ranged downloads, conditional writes and multipart uploads, with the checksums S3
// keeps and returns. It checks what it receives the way S3 does, so a test fails when
// hop sends a checksum that does not match the bytes.
package fakes3

import (
	"bufio"
	"bytes"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"hash"
	"hash/crc32"
	"hash/crc64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Object is an object of a fake bucket.
type Object struct {
	Data         []byte
	Modified     time.Time
	StorageClass string
	// Checksum is the algorithm S3 holds a checksum for (CRC32, CRC32C, CRC64NVME,
	// SHA256), or "" for an object stored without one.
	Checksum string
	// Composite marks the checksum of an object uploaded in parts by a tool that kept
	// only a checksum of the parts' checksums.
	Composite bool
}

// Bucket is a fake bucket.
type Bucket struct {
	Region  string
	Created time.Time
	Objects map[string]*Object
	// foreign marks a bucket of another account: usable, but not listed.
	foreign bool
}

type upload struct {
	bucket, key string
	parts       map[int][]byte
}

// Server is a fake S3. Its fields may be set before, or between, the calls of a test.
type Server struct {
	URL string

	mu      sync.Mutex
	buckets map[string]*Bucket
	uploads map[string]*upload
	next    int
	calls   []string

	// PageSize is the number of keys, or of buckets, per page of a listing.
	PageSize int
	// NoBucketRegion leaves the region out of ListBuckets, as S3-compatible stores do.
	NoBucketRegion bool
	// DenyListBuckets refuses ListBuckets.
	DenyListBuckets bool
	// DenyHead refuses HeadObject, as S3 does for a role that may write without
	// reading.
	DenyHead bool
	// DenyHeadBucket refuses HeadBucket: S3 still names the region of the bucket.
	DenyHeadBucket bool
	// Corrupt flips a byte of every object downloaded.
	Corrupt bool
	// FailPart makes UploadPart fail from that part number on (0: never).
	FailPart int
	// Changed makes every ranged GET after the first answer that the object changed.
	Changed bool
	// DenyGet refuses GetObject, DenyPut every write, DenyDelete DeleteObject.
	DenyGet, DenyPut, DenyDelete bool
}

// New starts a fake S3 that stops with the test.
func New(t testing.TB) *Server {
	t.Helper()
	s := &Server{buckets: map[string]*Bucket{}, uploads: map[string]*upload{}, PageSize: 1000}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	s.URL = srv.URL
	return s
}

// AddBucket creates a bucket in a region.
func (s *Server) AddBucket(name, region string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buckets[name] = &Bucket{Region: region, Created: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), Objects: map[string]*Object{}}
}

// AddForeignBucket creates a bucket that ListBuckets leaves out, like the bucket of
// another account that a policy opens to this one.
func (s *Server) AddForeignBucket(name, region string) {
	s.AddBucket(name, region)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buckets[name].foreign = true
}

// Put stores an object, with a checksum of the given algorithm ("" for none).
func (s *Server) Put(bucket, key string, data []byte, checksum string) *Object {
	s.mu.Lock()
	defer s.mu.Unlock()
	o := &Object{Data: data, Modified: time.Date(2026, 11, 7, 14, 2, 0, 0, time.UTC), StorageClass: "STANDARD", Checksum: checksum}
	s.buckets[bucket].Objects[key] = o
	return o
}

// Get returns an object, or nil.
func (s *Server) Get(bucket, key string) *Object {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b := s.buckets[bucket]; b != nil {
		return b.Objects[key]
	}
	return nil
}

// Keys returns the keys of a bucket, sorted.
func (s *Server) Keys(bucket string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	if b := s.buckets[bucket]; b != nil {
		for k := range b.Objects {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// Calls returns the requests received so far, as "METHOD /path operation".
func (s *Server) Calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

// Count returns the number of requests whose description contains what.
func (s *Server) Count(what string) int {
	n := 0
	for _, c := range s.Calls() {
		if strings.Contains(c, what) {
			n++
		}
	}
	return n
}

// OpenUploads returns the number of multipart uploads neither completed nor aborted.
func (s *Server) OpenUploads() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.uploads)
}

var crc64NVME = crc64.MakeTable(0x9a6c9329ac4bc9b5)

func newHash(alg string) hash.Hash {
	switch alg {
	case "CRC32":
		return crc32.NewIEEE()
	case "CRC32C":
		return crc32.New(crc32.MakeTable(crc32.Castagnoli))
	case "CRC64NVME":
		return crc64.New(crc64NVME)
	case "SHA256":
		return sha256.New()
	}
	return nil
}

// Sum returns the checksum of data as S3 writes it: base64 of the digest.
func Sum(alg string, data []byte) string {
	h := newHash(alg)
	h.Write(data)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func etag(data []byte) string {
	sum := md5.Sum(data)
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

func fail(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>%s</Message><RequestId>FAKEREQUEST</RequestId><HostId>FAKEHOST</HostId></Error>`, code, message)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		fail(w, http.StatusBadRequest, "IncompleteBody", err.Error())
		return
	}
	// Path-style addressing: /bucket/key. The key is taken from the escaped path, so
	// that a "%2F" or a "+" in a key stays what it was.
	path := strings.TrimPrefix(r.URL.EscapedPath(), "/")
	name, rawKey, _ := strings.Cut(path, "/")
	key, _ := url.PathUnescape(rawKey)
	q := r.URL.Query()

	s.mu.Lock()
	defer s.mu.Unlock()
	record := func(op string) { s.calls = append(s.calls, r.Method+" /"+path+" "+op) }

	if name == "" {
		record("ListBuckets")
		s.listBuckets(w, q)
		return
	}
	b := s.buckets[name]
	if b == nil {
		record("NoSuchBucket")
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fail(w, http.StatusNotFound, "NoSuchBucket", "The specified bucket does not exist")
		return
	}
	w.Header().Set("x-amz-bucket-region", b.Region)
	switch {
	case rawKey == "" && r.Method == http.MethodHead:
		record("HeadBucket")
		if s.DenyHeadBucket {
			w.WriteHeader(http.StatusForbidden)
		}
	case rawKey == "" && q.Get("list-type") == "2":
		record("ListObjectsV2 prefix=" + q.Get("prefix"))
		s.listObjects(w, name, b, q)
	case r.Method == http.MethodHead:
		record("HeadObject")
		s.headObject(w, r, b, key)
	case r.Method == http.MethodGet:
		record("GetObject " + r.Header.Get("Range"))
		if s.DenyGet {
			fail(w, http.StatusForbidden, "AccessDenied", "Access Denied")
			return
		}
		s.getObject(w, r, b, key)
	case s.DenyPut && (r.Method == http.MethodPut || r.Method == http.MethodPost):
		record("write denied")
		fail(w, http.StatusForbidden, "AccessDenied", "User: arn:aws:iam::111111111111:user/acme is not authorized to perform: s3:PutObject")
	case r.Method == http.MethodPost && q.Has("uploads"):
		record("CreateMultipartUpload")
		s.next++
		id := fmt.Sprintf("upload-%d", s.next)
		s.uploads[id] = &upload{bucket: name, key: key, parts: map[int][]byte{}}
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><InitiateMultipartUploadResult><Bucket>%s</Bucket><Key>%s</Key><UploadId>%s</UploadId></InitiateMultipartUploadResult>`, name, xmlText(key), id)
	case r.Method == http.MethodPut && q.Get("uploadId") != "":
		record("UploadPart " + q.Get("partNumber"))
		s.uploadPart(w, r, q, body)
	case r.Method == http.MethodPost && q.Get("uploadId") != "":
		record("CompleteMultipartUpload")
		s.completeUpload(w, r, q, b, body)
	case r.Method == http.MethodDelete && q.Get("uploadId") != "":
		record("AbortMultipartUpload")
		delete(s.uploads, q.Get("uploadId"))
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPut:
		record("PutObject")
		s.putObject(w, r, b, key, body)
	case r.Method == http.MethodDelete:
		record("DeleteObject")
		if s.DenyDelete {
			fail(w, http.StatusForbidden, "AccessDenied", "User: arn:aws:iam::111111111111:user/acme is not authorized to perform: s3:DeleteObject")
			return
		}
		delete(b.Objects, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		record("unsupported")
		fail(w, http.StatusNotImplemented, "NotImplemented", "the fake does not play this call")
	}
}

// readBody reads the body, out of its aws-chunked envelope when the SDK streams it
// with its checksum as a trailer.
func readBody(r *http.Request) ([]byte, error) {
	if !strings.HasPrefix(r.Header.Get("X-Amz-Content-Sha256"), "STREAMING-") {
		return io.ReadAll(r.Body)
	}
	br := bufio.NewReader(r.Body)
	var out bytes.Buffer
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		size, _, _ := strings.Cut(strings.TrimSpace(line), ";")
		n, err := strconv.ParseInt(size, 16, 64)
		if err != nil {
			return nil, err
		}
		if n == 0 {
			break
		}
		// Copied as it comes, rather than into a buffer of the size the request
		// announces.
		if _, err := io.CopyN(&out, br, n); err != nil {
			return nil, err
		}
		if _, err := br.Discard(2); err != nil {
			return nil, err
		}
	}
	// The trailers: the checksum of the body.
	for {
		line, err := br.ReadString('\n')
		line = strings.TrimSpace(line)
		if k, v, ok := strings.Cut(line, ":"); ok {
			r.Header.Set(k, strings.TrimSpace(v))
		}
		if err != nil || line == "" {
			return out.Bytes(), nil
		}
	}
}

func xmlText(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func (s *Server) listBuckets(w http.ResponseWriter, q url.Values) {
	if s.DenyListBuckets {
		fail(w, http.StatusForbidden, "AccessDenied", "User: arn:aws:iam::111111111111:user/acme is not authorized to perform: s3:ListAllMyBuckets")
		return
	}
	var names []string
	for n, b := range s.buckets {
		if !b.foreign {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	start := 0
	if tok := q.Get("continuation-token"); tok != "" {
		start = sort.SearchStrings(names, tok)
	}
	end := min(start+s.PageSize, len(names))
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><ListAllMyBucketsResult><Buckets>`)
	for _, n := range names[start:end] {
		bk := s.buckets[n]
		region := "<BucketRegion>" + bk.Region + "</BucketRegion>"
		if s.NoBucketRegion {
			region = ""
		}
		fmt.Fprintf(&b, `<Bucket><Name>%s</Name>%s<CreationDate>%s</CreationDate></Bucket>`, n, region, bk.Created.Format("2006-01-02T15:04:05.000Z"))
	}
	b.WriteString(`</Buckets>`)
	if end < len(names) {
		fmt.Fprintf(&b, `<ContinuationToken>%s</ContinuationToken>`, names[end])
	}
	b.WriteString(`</ListAllMyBucketsResult>`)
	w.Header().Set("Content-Type", "application/xml")
	fmt.Fprint(w, b.String())
}

func (s *Server) listObjects(w http.ResponseWriter, name string, b *Bucket, q url.Values) {
	prefix, delimiter := q.Get("prefix"), q.Get("delimiter")
	enc := func(k string) string { return xmlText(k) }
	if q.Get("encoding-type") == "url" {
		enc = url.QueryEscape
	}
	var keys []string
	for k := range b.Objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	// Group the keys at the delimiter, as S3 does: one entry per common prefix.
	type item struct {
		key    string
		prefix bool
	}
	var items []item
	seen := map[string]bool{}
	for _, k := range keys {
		if delimiter != "" {
			if i := strings.Index(k[len(prefix):], delimiter); i >= 0 {
				p := k[:len(prefix)+i+len(delimiter)]
				if !seen[p] {
					seen[p] = true
					items = append(items, item{p, true})
				}
				continue
			}
		}
		items = append(items, item{k, false})
	}
	start := 0
	if tok := q.Get("continuation-token"); tok != "" {
		start, _ = strconv.Atoi(tok)
	}
	end := min(start+s.PageSize, len(items))
	var out strings.Builder
	fmt.Fprintf(&out, `<?xml version="1.0" encoding="UTF-8"?><ListBucketResult><Name>%s</Name><Prefix>%s</Prefix><KeyCount>%d</KeyCount>`, name, enc(prefix), end-start)
	if q.Get("encoding-type") == "url" {
		out.WriteString(`<EncodingType>url</EncodingType>`)
	}
	if end < len(items) {
		fmt.Fprintf(&out, `<IsTruncated>true</IsTruncated><NextContinuationToken>%d</NextContinuationToken>`, end)
	} else {
		out.WriteString(`<IsTruncated>false</IsTruncated>`)
	}
	for _, it := range items[start:end] {
		if it.prefix {
			fmt.Fprintf(&out, `<CommonPrefixes><Prefix>%s</Prefix></CommonPrefixes>`, enc(it.key))
			continue
		}
		o := b.Objects[it.key]
		fmt.Fprintf(&out, `<Contents><Key>%s</Key><LastModified>%s</LastModified><ETag>%s</ETag><Size>%d</Size><StorageClass>%s</StorageClass></Contents>`,
			enc(it.key), o.Modified.Format("2006-01-02T15:04:05.000Z"), xmlText(etag(o.Data)), len(o.Data), o.StorageClass)
	}
	out.WriteString(`</ListBucketResult>`)
	w.Header().Set("Content-Type", "application/xml")
	fmt.Fprint(w, out.String())
}

// describe sets the headers that describe an object, checksums included when asked.
func describe(w http.ResponseWriter, r *http.Request, o *Object) {
	w.Header().Set("ETag", etag(o.Data))
	w.Header().Set("Last-Modified", o.Modified.Format(http.TimeFormat))
	w.Header().Set("x-amz-storage-class", o.StorageClass)
	w.Header().Set("Accept-Ranges", "bytes")
	if o.Checksum == "" || !strings.EqualFold(r.Header.Get("x-amz-checksum-mode"), "ENABLED") {
		return
	}
	header := "x-amz-checksum-" + strings.ToLower(o.Checksum)
	if o.Composite {
		w.Header().Set(header, Sum(o.Checksum, []byte("checksums of the parts"))+"-3")
		w.Header().Set("x-amz-checksum-type", "COMPOSITE")
		return
	}
	w.Header().Set(header, Sum(o.Checksum, o.Data))
	w.Header().Set("x-amz-checksum-type", "FULL_OBJECT")
}

func (s *Server) headObject(w http.ResponseWriter, r *http.Request, b *Bucket, key string) {
	if s.DenyHead {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	o := b.Objects[key]
	if o == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	describe(w, r, o)
	w.Header().Set("Content-Length", strconv.Itoa(len(o.Data)))
}

func (s *Server) getObject(w http.ResponseWriter, r *http.Request, b *Bucket, key string) {
	o := b.Objects[key]
	if o == nil {
		fail(w, http.StatusNotFound, "NoSuchKey", "The specified key does not exist.")
		return
	}
	if o.StorageClass == "GLACIER" {
		fail(w, http.StatusForbidden, "InvalidObjectState", "The operation is not valid for the object's storage class")
		return
	}
	ranged := r.Header.Get("Range")
	first := strings.HasPrefix(ranged, "bytes=0-")
	if m := r.Header.Get("If-Match"); (m != "" && m != etag(o.Data)) || (s.Changed && ranged != "" && !first) {
		fail(w, http.StatusPreconditionFailed, "PreconditionFailed", "At least one of the pre-conditions you specified did not hold")
		return
	}
	data := o.Data
	if s.Corrupt && len(data) > 0 {
		data = append([]byte(nil), data...)
		data[len(data)/2] ^= 0xff
	}
	describe(w, r, o)
	if ranged == "" {
		_, _ = w.Write(data)
		return
	}
	var from, to int
	if _, err := fmt.Sscanf(ranged, "bytes=%d-%d", &from, &to); err != nil || from >= len(data) {
		fail(w, http.StatusRequestedRangeNotSatisfiable, "InvalidRange", "The requested range is not satisfiable")
		return
	}
	to = min(to, len(data)-1)
	// A checksum of the whole object says nothing about a range.
	for k := range w.Header() {
		if strings.HasPrefix(strings.ToLower(k), "x-amz-checksum-") {
			w.Header().Del(k)
		}
	}
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, to, len(data)))
	w.Header().Set("Content-Length", strconv.Itoa(to-from+1))
	w.WriteHeader(http.StatusPartialContent)
	_, _ = w.Write(data[from : to+1])
}

// verify compares the checksum a request announces with the bytes received, like S3.
func verify(w http.ResponseWriter, r *http.Request, body []byte) (alg string, ok bool) {
	for _, a := range []string{"CRC32", "CRC32C", "CRC64NVME", "SHA256"} {
		if want := r.Header.Get("x-amz-checksum-" + strings.ToLower(a)); want != "" {
			if got := Sum(a, body); got != want {
				fail(w, http.StatusBadRequest, "BadDigest", "The "+a+" you specified did not match the calculated checksum.")
				return a, false
			}
			return a, true
		}
	}
	return "", true
}

func (s *Server) putObject(w http.ResponseWriter, r *http.Request, b *Bucket, key string, body []byte) {
	if r.Header.Get("If-None-Match") == "*" && b.Objects[key] != nil {
		fail(w, http.StatusPreconditionFailed, "PreconditionFailed", "At least one of the pre-conditions you specified did not hold")
		return
	}
	alg, ok := verify(w, r, body)
	if !ok {
		return
	}
	o := &Object{Data: body, Modified: time.Now().UTC(), StorageClass: "STANDARD", Checksum: alg}
	b.Objects[key] = o
	w.Header().Set("ETag", etag(body))
	if alg != "" {
		w.Header().Set("x-amz-checksum-"+strings.ToLower(alg), Sum(alg, body))
		w.Header().Set("x-amz-checksum-type", "FULL_OBJECT")
	}
}

func (s *Server) uploadPart(w http.ResponseWriter, r *http.Request, q url.Values, body []byte) {
	up := s.uploads[q.Get("uploadId")]
	n, _ := strconv.Atoi(q.Get("partNumber"))
	if up == nil {
		fail(w, http.StatusNotFound, "NoSuchUpload", "The specified upload does not exist.")
		return
	}
	if s.FailPart > 0 && n >= s.FailPart {
		fail(w, http.StatusForbidden, "AccessDenied", "Access Denied")
		return
	}
	alg, ok := verify(w, r, body)
	if !ok {
		return
	}
	up.parts[n] = body
	w.Header().Set("ETag", etag(body))
	if alg != "" {
		w.Header().Set("x-amz-checksum-"+strings.ToLower(alg), Sum(alg, body))
	}
}

func (s *Server) completeUpload(w http.ResponseWriter, r *http.Request, q url.Values, b *Bucket, body []byte) {
	id := q.Get("uploadId")
	up := s.uploads[id]
	if up == nil {
		fail(w, http.StatusNotFound, "NoSuchUpload", "The specified upload does not exist.")
		return
	}
	if r.Header.Get("If-None-Match") == "*" && b.Objects[up.key] != nil {
		fail(w, http.StatusPreconditionFailed, "PreconditionFailed", "At least one of the pre-conditions you specified did not hold")
		return
	}
	var req struct {
		Parts []struct {
			PartNumber int `xml:"PartNumber"`
		} `xml:"Part"`
	}
	if err := xml.Unmarshal(body, &req); err != nil {
		fail(w, http.StatusBadRequest, "MalformedXML", err.Error())
		return
	}
	var data []byte
	for _, p := range req.Parts {
		part, ok := up.parts[p.PartNumber]
		if !ok {
			fail(w, http.StatusBadRequest, "InvalidPart", "One or more of the specified parts could not be found.")
			return
		}
		data = append(data, part...)
	}
	delete(s.uploads, id)
	b.Objects[up.key] = &Object{Data: data, Modified: time.Now().UTC(), StorageClass: "STANDARD", Checksum: "CRC32"}
	w.Header().Set("Content-Type", "application/xml")
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><CompleteMultipartUploadResult><Bucket>%s</Bucket><Key>%s</Key><ETag>%s</ETag><ChecksumCRC32>%s</ChecksumCRC32><ChecksumType>FULL_OBJECT</ChecksumType></CompleteMultipartUploadResult>`,
		up.bucket, xmlText(up.key), xmlText(etag(data)), Sum("CRC32", data))
}
