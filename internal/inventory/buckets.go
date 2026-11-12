package inventory

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// Bucket is an S3 bucket of the account behind a profile. Region is where the bucket
// lives, whatever the region of the profile: one ListBuckets call returns the buckets
// of every region.
type Bucket struct {
	Name    string    `json:"name"`
	Profile string    `json:"profile"`
	Region  string    `json:"region,omitempty"`
	Env     string    `json:"env,omitempty"`
	Created time.Time `json:"created,omitzero"`
	// Other profiles that list the same bucket (two profiles on one account).
	AlsoVia []string `json:"also_via,omitempty"`
}

// BucketList holds the buckets of the account behind one profile, with its own refresh
// time and its own error, like a Target does for the instances of a profile/region
// pair. It is filled by the S3 commands only: listing instances never calls S3.
type BucketList struct {
	Profile   string    `json:"profile"`
	FetchedAt time.Time `json:"fetched_at,omitzero"`
	Err       string    `json:"err,omitempty"`
	ErrKind   ErrKind   `json:"err_kind,omitempty"`
	ErrAt     time.Time `json:"err_at,omitzero"`
	// InvalidatedAt is the time of the last Invalidate (see Target.InvalidatedAt).
	InvalidatedAt time.Time `json:"invalidated_at,omitzero"`
	Buckets       []Bucket  `json:"buckets"`
}

// Fresh reports whether the list is recent and the last attempt succeeded. A list in
// error is retried after one minute, as a Target is.
func (l *BucketList) Fresh(ttl time.Duration, now time.Time) bool {
	if l.Err != "" {
		return now.Sub(l.ErrAt) < time.Minute
	}
	return !l.FetchedAt.IsZero() && now.Sub(l.FetchedAt) < ttl
}

// updated is the time of the latest event recorded for the list (see Target.updated).
func (l *BucketList) updated() time.Time {
	u := l.FetchedAt
	for _, at := range []time.Time{l.ErrAt, l.InvalidatedAt} {
		if at.After(u) {
			u = at
		}
	}
	return u
}

// PutBuckets records the result of a refresh. A failed call keeps the buckets and the
// time of the last success: an expired token must not empty the list.
func (inv *Inventory) PutBuckets(l *BucketList) {
	if inv.Buckets == nil {
		inv.Buckets = map[string]*BucketList{}
	}
	if old, ok := inv.Buckets[l.Profile]; ok && l.Err != "" {
		l.Buckets = old.Buckets
		l.FetchedAt = old.FetchedAt
	}
	inv.Buckets[l.Profile] = l
}

// BucketsOf flattens the bucket lists of the given profiles, in that order. A bucket
// listed by several profiles appears once, under the first profile whose list is not in
// error, with the other profiles in AlsoVia.
func (inv *Inventory) BucketsOf(profiles []string) []Bucket {
	type seenVia struct {
		b  Bucket
		ok bool
	}
	var names []string
	copies := map[string][]seenVia{}
	order := map[string]int{}
	for _, p := range profiles {
		l, ok := inv.Buckets[p]
		if !ok {
			continue
		}
		if _, ok := order[p]; !ok {
			order[p] = len(order)
		}
		for _, b := range l.Buckets {
			if _, dup := copies[b.Name]; !dup {
				names = append(names, b.Name)
			}
			copies[b.Name] = append(copies[b.Name], seenVia{b, l.Err == ""})
		}
	}
	out := make([]Bucket, 0, len(names))
	for _, name := range names {
		cs := copies[name]
		best := 0
		for i := 1; i < len(cs); i++ {
			if cs[i].ok && !cs[best].ok {
				best = i
			}
		}
		b := cs[best].b
		b.AlsoVia = nil
		for i, c := range cs {
			if i != best {
				b.AlsoVia = append(b.AlsoVia, c.b.Profile)
			}
		}
		out = append(out, b)
	}
	sort.SliceStable(out, func(a, b int) bool {
		if pa, pb := order[out[a].Profile], order[out[b].Profile]; pa != pb {
			return pa < pb
		}
		return out[a].Name < out[b].Name
	})
	return out
}

// bucketPage is the number of buckets asked for per ListBuckets call.
const bucketPage = 1000

// FetchBuckets lists the buckets of the account behind each profile, sixteen profiles at
// a time. The region of a pair is only the endpoint the call goes to. It never returns
// an error: each BucketList carries its own. As in FetchAll, profiles that share an SSO
// token wait for the first of them to resolve its credentials. progress is called each
// time a profile completes.
func FetchBuckets(ctx context.Context, pairs []Pair, timeout time.Duration, progress func(done, total int)) []*BucketList {
	out := make([]*BucketList, len(pairs))
	var gate TokenGate
	sem := make(chan struct{}, 16)
	var wg sync.WaitGroup
	var mu sync.Mutex
	done := 0
	for i, p := range pairs {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			cctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			out[i] = fetchBuckets(cctx, p, &gate)
			mu.Lock()
			done++
			if progress != nil {
				progress(done, len(pairs))
			}
			mu.Unlock()
		})
	}
	wg.Wait()
	return out
}

func fetchBuckets(ctx context.Context, p Pair, gate *TokenGate) *BucketList {
	start := time.Now()
	fail := func(err error) *BucketList {
		return &BucketList{Profile: p.Profile, Err: Redact(err.Error()), ErrKind: Classify(err), ErrAt: start}
	}
	cfg, err := AWSConfig(ctx, p.Profile, p.Region, p.PreferSSO)
	if err != nil {
		return fail(err)
	}
	// As for instances: an expired SSO token fails here, quickly and with a typed error.
	if err := gate.Retrieve(ctx, p.Token, cfg); err != nil {
		return fail(err)
	}
	l := &BucketList{Profile: p.Profile, FetchedAt: start, Buckets: []Bucket{}}
	pages := s3.NewListBucketsPaginator(s3.NewFromConfig(cfg), &s3.ListBucketsInput{MaxBuckets: aws.Int32(bucketPage)})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return fail(err)
		}
		for _, b := range page.Buckets {
			name := clean(aws.ToString(b.Name))
			if name == "" {
				continue
			}
			l.Buckets = append(l.Buckets, Bucket{
				Name: name, Profile: p.Profile, Region: clean(aws.ToString(b.BucketRegion)),
				Env: DeriveEnv(nil, name, p.Profile), Created: aws.ToTime(b.CreationDate),
			})
		}
	}
	return l
}

// BucketRegion asks S3 where a bucket lives, with cfg set to any region. S3 names the
// region in a header even when it refuses the call (another region, no permission on the
// bucket): only a bucket that does not exist, or an answer without that header, is an
// error.
func BucketRegion(ctx context.Context, cfg aws.Config, bucket string) (string, error) {
	out, err := s3.NewFromConfig(cfg).HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)})
	if err == nil {
		return aws.ToString(out.BucketRegion), nil
	}
	var re *smithyhttp.ResponseError
	if errors.As(err, &re) && re.Response != nil {
		if r := re.Response.Header.Get("x-amz-bucket-region"); r != "" && re.HTTPStatusCode() != 404 {
			return r, nil
		}
	}
	return "", err
}
