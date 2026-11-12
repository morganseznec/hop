package bucket

import (
	"crypto/sha256"
	"encoding/base64"
	"hash"
	"hash/crc32"
	"hash/crc64"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// crc64NVME is the CRC-64/NVME polynomial, in the reflected form hash/crc64 takes.
// S3 calls the checksum CRC64NVME.
var crc64NVME = crc64.MakeTable(0x9a6c9329ac4bc9b5)

// algorithm is a checksum S3 can keep for an object and hop can compute.
type algorithm struct {
	name string
	new  func() hash.Hash
	// of returns the value S3 holds, base64-encoded, or nil.
	of func(*s3.HeadObjectOutput) *string
}

// algorithms lists them in the order hop prefers when an object has several.
var algorithms = []algorithm{
	{"SHA256", sha256.New, func(h *s3.HeadObjectOutput) *string { return h.ChecksumSHA256 }},
	{"CRC64NVME", func() hash.Hash { return crc64.New(crc64NVME) }, func(h *s3.HeadObjectOutput) *string { return h.ChecksumCRC64NVME }},
	{"CRC32C", func() hash.Hash { return crc32.New(crc32.MakeTable(crc32.Castagnoli)) }, func(h *s3.HeadObjectOutput) *string { return h.ChecksumCRC32C }},
	{"CRC32", func() hash.Hash { return crc32.NewIEEE() }, func(h *s3.HeadObjectOutput) *string { return h.ChecksumCRC32 }},
}

// stored returns the checksum S3 holds for the whole object, if hop can compute it.
// An object uploaded in several parts may only have a checksum of its parts' checksums
// (COMPOSITE, written "value-parts"), which says nothing without the part sizes; and an
// object uploaded without one has none.
func stored(h *s3.HeadObjectOutput) (alg algorithm, value string, ok bool) {
	if h.ChecksumType == types.ChecksumTypeComposite {
		return algorithm{}, "", false
	}
	for _, a := range algorithms {
		if v := aws.ToString(a.of(h)); v != "" && !strings.Contains(v, "-") {
			return a, v, true
		}
	}
	return algorithm{}, "", false
}

// encode writes a digest the way S3 does: base64 of its bytes, most significant first.
func encode(h hash.Hash) string {
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}
