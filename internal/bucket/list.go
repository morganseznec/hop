package bucket

import (
	"context"
	"net/url"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Object is an entry of a listing: an object, or a group of keys that share what comes
// before the next "/" (Prefix), which S3 calls a common prefix and a shell a directory.
type Object struct {
	Key          string    `json:"key"`
	Size         int64     `json:"size"`
	Modified     time.Time `json:"modified,omitzero"`
	StorageClass string    `json:"storage_class,omitempty"`
	Prefix       bool      `json:"prefix,omitempty"`
}

// List returns what the bucket holds under loc.Key, in the order S3 lists keys. Keys
// are grouped at the next "/" unless recursive is set, which lists every object below.
// cfg must be set to the region of the bucket. progress, when not nil, receives the
// number of entries read after each page: a large bucket takes a call per thousand
// keys.
func List(ctx context.Context, cfg aws.Config, loc Location, recursive bool, progress func(n int)) ([]Object, error) {
	in := &s3.ListObjectsV2Input{
		Bucket: aws.String(loc.Bucket),
		// A key may hold characters that XML cannot carry: S3 then sends the keys
		// URL-encoded, and unescape reads them back.
		EncodingType: types.EncodingTypeUrl,
	}
	if loc.Key != "" {
		in.Prefix = aws.String(loc.Key)
	}
	if !recursive {
		in.Delimiter = aws.String("/")
	}
	out := []Object{}
	pages := s3.NewListObjectsV2Paginator(s3.NewFromConfig(cfg), in)
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, explain(err, "listing", loc)
		}
		for _, p := range page.CommonPrefixes {
			out = append(out, Object{Key: unescape(aws.ToString(p.Prefix)), Prefix: true})
		}
		for _, o := range page.Contents {
			out = append(out, Object{
				Key: unescape(aws.ToString(o.Key)), Size: aws.ToInt64(o.Size),
				Modified: aws.ToTime(o.LastModified), StorageClass: string(o.StorageClass),
			})
		}
		if progress != nil {
			progress(len(out))
		}
	}
	return out, nil
}

// unescape reads a key sent URL-encoded. A key that does not decode is kept as sent.
func unescape(s string) string {
	if u, err := url.QueryUnescape(s); err == nil {
		return u
	}
	return s
}
