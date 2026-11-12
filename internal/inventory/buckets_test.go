package inventory

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/morganseznec/hop/internal/fakes3"
)

// fakeS3Env isolates the SDK like fakeEnv, with a fake S3 as every endpoint.
func fakeS3Env(t *testing.T) *fakes3.Server {
	t.Helper()
	srv := fakes3.New(t)
	fakeEnv(t, srv.URL)
	t.Setenv("AWS_ENDPOINT_URL_S3", "")
	return srv
}

func TestFetchBuckets(t *testing.T) {
	srv := fakeS3Env(t)
	srv.PageSize = 2 // several pages
	srv.AddBucket("acme-prod-logs", "eu-west-1")
	srv.AddBucket("acme-backups", "us-east-1")
	srv.AddBucket("acme-t-scratch", "eu-west-3")

	var steps []int
	lists := FetchBuckets(context.Background(), []Pair{{Profile: "acme-prod", Region: "eu-west-1"}}, 10*time.Second,
		func(done, total int) { steps = append(steps, done, total) })
	if len(lists) != 1 || lists[0].Err != "" {
		t.Fatalf("lists %+v", lists)
	}
	l := lists[0]
	if l.Profile != "acme-prod" || l.FetchedAt.IsZero() || len(l.Buckets) != 3 {
		t.Fatalf("list %+v", l)
	}
	// Each bucket comes with its own region, whatever the region the call went to, and
	// with the environment its name or its profile tells.
	want := []Bucket{
		{Name: "acme-backups", Profile: "acme-prod", Region: "us-east-1", Env: "prod"},
		{Name: "acme-prod-logs", Profile: "acme-prod", Region: "eu-west-1", Env: "prod"},
		{Name: "acme-t-scratch", Profile: "acme-prod", Region: "eu-west-3", Env: "test"},
	}
	for i, w := range want {
		got := l.Buckets[i]
		if got.Name != w.Name || got.Profile != w.Profile || got.Region != w.Region || got.Env != w.Env || got.Created.IsZero() {
			t.Errorf("bucket %d: %+v, want %+v", i, got, w)
		}
	}
	if srv.Count("ListBuckets") != 2 {
		t.Errorf("3 buckets by pages of 2 take 2 calls: %v", srv.Calls())
	}
	if len(steps) != 2 || steps[0] != 1 || steps[1] != 1 {
		t.Errorf("progress %v", steps)
	}
}

func TestFetchBucketsFailures(t *testing.T) {
	srv := fakeS3Env(t)
	srv.AddBucket("acme-prod-logs", "eu-west-1")
	srv.DenyListBuckets = true
	lists := FetchBuckets(context.Background(), []Pair{
		{Profile: "acme-prod", Region: "eu-west-1"},
		// SSO, without a token in the temporary home.
		{Profile: "acme-dual", Region: "eu-west-1", PreferSSO: true},
	}, 10*time.Second, nil)
	denied, sso := lists[0], lists[1]
	if denied.ErrKind != ErrDenied || denied.ErrAt.IsZero() || !denied.FetchedAt.IsZero() || !strings.Contains(denied.Err, "s3:ListAllMyBuckets") {
		t.Errorf("denied: %+v", denied)
	}
	if sso.ErrKind != ErrSSOExpired || sso.Profile != "acme-dual" {
		t.Errorf("SSO profile without a token: %+v", sso)
	}
}

func TestBucketRegion(t *testing.T) {
	srv := fakeS3Env(t)
	srv.AddBucket("acme-backups", "us-east-1")
	cfg, err := AWSConfig(context.Background(), "acme-prod", "eu-west-1", false)
	if err != nil {
		t.Fatal(err)
	}
	if r, err := BucketRegion(context.Background(), cfg, "acme-backups"); err != nil || r != "us-east-1" {
		t.Errorf("region %q, %v", r, err)
	}
	if r, err := BucketRegion(context.Background(), cfg, "acme-gone"); err == nil {
		t.Errorf("a missing bucket has no region, got %q", r)
	}
	// A role that may not ask about the bucket still learns where it lives.
	srv.DenyHeadBucket = true
	if r, err := BucketRegion(context.Background(), cfg, "acme-backups"); err != nil || r != "us-east-1" {
		t.Errorf("HeadBucket denied: region %q, %v", r, err)
	}
}

func bucketList(profile string, at time.Time, names ...string) *BucketList {
	l := &BucketList{Profile: profile, FetchedAt: at, Buckets: []Bucket{}}
	for _, n := range names {
		l.Buckets = append(l.Buckets, Bucket{Name: n, Profile: profile, Region: "eu-west-1"})
	}
	return l
}

func TestPutBucketsKeepsTheLastListOnFailure(t *testing.T) {
	inv := &Inventory{Version: version, Targets: map[string]*Target{}}
	at := time.Now().Add(-time.Hour)
	inv.PutBuckets(bucketList("acme-prod", at, "acme-logs", "acme-backups"))
	failedAt := time.Now()
	inv.PutBuckets(&BucketList{Profile: "acme-prod", Err: "token expired", ErrKind: ErrSSOExpired, ErrAt: failedAt})
	l := inv.Buckets["acme-prod"]
	if len(l.Buckets) != 2 || !l.FetchedAt.Equal(at) || l.ErrKind != ErrSSOExpired {
		t.Errorf("after a failed refresh: %+v", l)
	}
	// A success replaces everything, an emptied account included.
	inv.PutBuckets(bucketList("acme-prod", time.Now()))
	if l := inv.Buckets["acme-prod"]; len(l.Buckets) != 0 || l.Err != "" {
		t.Errorf("after a successful refresh: %+v", l)
	}
}

func TestBucketListFresh(t *testing.T) {
	now := time.Now()
	for _, c := range []struct {
		name string
		l    BucketList
		want bool
	}{
		{"just read", BucketList{FetchedAt: now.Add(-time.Minute)}, true},
		{"past the TTL", BucketList{FetchedAt: now.Add(-16 * time.Minute)}, false},
		{"never read", BucketList{}, false},
		{"failed seconds ago", BucketList{Err: "x", ErrAt: now.Add(-10 * time.Second), FetchedAt: now.Add(-time.Hour)}, true},
		{"failed two minutes ago", BucketList{Err: "x", ErrAt: now.Add(-2 * time.Minute), FetchedAt: now.Add(-time.Minute)}, false},
	} {
		if got := c.l.Fresh(15*time.Minute, now); got != c.want {
			t.Errorf("%s: Fresh = %v", c.name, got)
		}
	}
}

func TestBucketsOf(t *testing.T) {
	now := time.Now()
	inv := &Inventory{Version: version, Targets: map[string]*Target{}}
	// acme-sso and acme-keys are two profiles on the same account.
	inv.PutBuckets(bucketList("acme-sso", now, "acme-logs", "acme-backups"))
	inv.PutBuckets(bucketList("acme-keys", now, "acme-backups", "acme-logs"))
	inv.PutBuckets(bucketList("globex", now, "globex-data"))

	got := inv.BucketsOf([]string{"acme-sso", "acme-keys", "globex", "unknown"})
	var lines []string
	for _, b := range got {
		lines = append(lines, b.Name+" "+b.Profile+" "+strings.Join(b.AlsoVia, ","))
	}
	want := "acme-backups acme-sso acme-keys|acme-logs acme-sso acme-keys|globex-data globex "
	if strings.Join(lines, "|") != want {
		t.Errorf("got %s\nwant %s", strings.Join(lines, "|"), want)
	}

	// The profile listed first is in error: its copy gives way to the one that answers.
	inv.PutBuckets(&BucketList{Profile: "acme-sso", Err: "token expired", ErrAt: now})
	got = inv.BucketsOf([]string{"acme-sso", "acme-keys"})
	if len(got) != 2 || got[0].Profile != "acme-keys" || len(got[0].AlsoVia) != 1 || got[0].AlsoVia[0] != "acme-sso" {
		t.Errorf("with the first profile in error: %+v", got)
	}

	// Out of scope: nothing of the other profiles.
	if got := inv.BucketsOf([]string{"globex"}); len(got) != 1 || got[0].Name != "globex-data" {
		t.Errorf("one profile: %+v", got)
	}
	if got := inv.BucketsOf(nil); len(got) != 0 {
		t.Errorf("no profile: %+v", got)
	}
}

func TestSaveKeepsAndMergesBuckets(t *testing.T) {
	tempCacheDir(t)
	old := time.Now().Add(-time.Hour)
	a := Load()
	a.PutBuckets(bucketList("acme-prod", old, "acme-logs"))
	a.PutBuckets(bucketList("globex", old, "globex-data"))
	if err := a.Save(); err != nil {
		t.Fatal(err)
	}

	// Another hop reads the cache, then this one saves a newer list of acme-prod.
	b := Load()
	a.PutBuckets(bucketList("acme-prod", time.Now(), "acme-logs", "acme-new"))
	if err := a.Save(); err != nil {
		t.Fatal(err)
	}
	// The other one saves what it read an hour-old copy of: the newer list wins.
	b.PutBuckets(bucketList("globex", time.Now(), "globex-data", "globex-new"))
	if err := b.Save(); err != nil {
		t.Fatal(err)
	}
	got := Load()
	if n := len(got.Buckets["acme-prod"].Buckets); n != 2 {
		t.Errorf("acme-prod: the older list was written back (%d buckets)", n)
	}
	if n := len(got.Buckets["globex"].Buckets); n != 2 {
		t.Errorf("globex: %d buckets", n)
	}

	// A profile removed from the AWS files leaves the cache with its buckets.
	c := Load()
	c.Retain([]string{"acme-prod"})
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	if got := Load(); got.Buckets["globex"] != nil || got.Buckets["acme-prod"] == nil {
		t.Errorf("after Retain: %v", got.Buckets)
	}
}

func TestInvalidateCoversBuckets(t *testing.T) {
	inv := &Inventory{Version: version, Targets: map[string]*Target{}}
	inv.PutBuckets(bucketList("acme-prod", time.Now(), "acme-logs"))
	inv.PutBuckets(bucketList("globex", time.Now(), "globex-data"))
	inv.Invalidate("acme-prod")
	if l := inv.Buckets["acme-prod"]; l.Fresh(time.Hour, time.Now()) || l.InvalidatedAt.IsZero() || len(l.Buckets) != 1 {
		t.Errorf("acme-prod after Invalidate: %+v", l)
	}
	if !inv.Buckets["globex"].Fresh(time.Hour, time.Now()) {
		t.Error("another profile was invalidated")
	}
}

func TestClassifyS3Keys(t *testing.T) {
	if k := classifyCode("InvalidAccessKeyId", "S3"); k != ErrBadKeys {
		t.Errorf("InvalidAccessKeyId: %q", k)
	}
	if k := classifyCode("AccessDenied", "S3"); k != ErrDenied {
		t.Errorf("AccessDenied: %q", k)
	}
}
