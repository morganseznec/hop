package inventory

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sync"
	"testing"
	"time"
)

func tempCacheDir(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", base)
	return filepath.Join(base, "hop")
}

func TestCacheDir(t *testing.T) {
	dir := tempCacheDir(t)
	if CacheDir() != dir || Path() != filepath.Join(dir, "inventory.json") {
		t.Errorf("with XDG_CACHE_HOME: %s, %s", CacheDir(), Path())
	}
	home := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("LOCALAPPDATA", filepath.Join(home, "AppData", "Local"))
	want := filepath.Join(home, ".cache", "hop")
	if runtime.GOOS == "windows" {
		want = filepath.Join(home, "AppData", "Local", "hop")
	}
	if got := CacheDir(); got != want {
		t.Errorf("without XDG_CACHE_HOME: %s, want %s", got, want)
	}
}

func sampleInventory() *Inventory {
	at := time.Date(2026, 10, 30, 9, 15, 0, 0, time.UTC)
	return &Inventory{Version: version, Targets: map[string]*Target{
		TargetKey("acme-prod", "eu-west-1"): {
			Profile: "acme-prod", Region: "eu-west-1", FetchedAt: at, AgentsKnown: true,
			Transcript: Transcript{Checked: true, Enabled: true, LogGroup: "/acme/ssm/sessions", Encrypted: true},
			Instances: []Instance{
				{ID: "i-0aaa", Name: "acme-p-tools", Profile: "acme-prod", Region: "eu-west-1", PrivateIP: "10.0.0.1",
					State: "running", Env: "prod", Platform: "Amazon Linux", Agent: "Online", LastPing: at.Add(-time.Minute)},
				{ID: "mi-0bbb", Name: "acme-onprem", Profile: "acme-prod", Region: "eu-west-1", State: "running", Agent: "ConnectionLost"},
			},
		},
		TargetKey("globex", "us-east-1"): {
			Profile: "globex", Region: "us-east-1", Err: "expired", ErrKind: ErrSSOExpired, ErrAt: at,
			Transcript: Transcript{Err: "not checked"},
		},
	}}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := tempCacheDir(t)
	inv := sampleInventory()
	if err := inv.Save(); err != nil {
		t.Fatal(err)
	}
	got := Load()
	if !reflect.DeepEqual(got, inv) {
		t.Errorf("round trip:\n got %+v\nwant %+v", got.Targets, inv.Targets)
	}
	// The temporary file is renamed, never left behind.
	if left, _ := filepath.Glob(filepath.Join(dir, "inventory-*.json")); len(left) != 0 {
		t.Errorf("temporary files left: %v", left)
	}
	// Saving again replaces the file.
	delete(inv.Targets, TargetKey("globex", "us-east-1"))
	if err := inv.Save(); err != nil {
		t.Fatal(err)
	}
	if got := Load(); len(got.Targets) != 1 {
		t.Errorf("second save: %d targets", len(got.Targets))
	}
}

func TestSavePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits do not apply on Windows")
	}
	dir := tempCacheDir(t)
	if err := sampleInventory().Save(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{dir, Path()} {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s open to other users: %o", p, st.Mode().Perm())
		}
	}
}

func TestLoadRebuilds(t *testing.T) {
	cases := []struct {
		name    string
		content string // empty: no file
	}{
		{"no file", ""},
		{"not JSON", "{truncated"},
		{"other version", `{"version":2,"targets":{"acme-prod|eu-west-1":{"profile":"acme-prod","region":"eu-west-1"}}}`},
		{"no version", `{"targets":{"acme-prod|eu-west-1":{"profile":"acme-prod","region":"eu-west-1"}}}`},
		{"null targets", `{"version":1,"targets":null}`},
		{"wrong shape", `[1,2,3]`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := tempCacheDir(t)
			if c.content != "" {
				os.MkdirAll(dir, 0o700)
				os.WriteFile(Path(), []byte(c.content), 0o600)
			}
			inv := Load()
			if inv.Version != version || inv.Targets == nil || len(inv.Targets) != 0 {
				t.Errorf("want an empty inventory, got %+v", inv)
			}
			// Put must work on the rebuilt inventory.
			inv.Put(&Target{Profile: "acme-prod", Region: "eu-west-1"})
		})
	}
}

func TestSaveRemovesTempFileOnFailure(t *testing.T) {
	// A non-empty folder in place of inventory.json: the rename fails.
	dir := tempCacheDir(t)
	if err := os.MkdirAll(filepath.Join(Path(), "blocker"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := sampleInventory().Save(); err == nil {
		t.Fatal("want an error")
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "inventory-*.json")); len(left) != 0 {
		t.Errorf("temporary files left: %v", left)
	}
}

func TestSaveFailsWhenFolderIsAFile(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	os.WriteFile(blocker, nil, 0o600)
	t.Setenv("XDG_CACHE_HOME", blocker)
	if err := sampleInventory().Save(); err == nil {
		t.Error("want an error")
	}
}

func TestConcurrentSaves(t *testing.T) {
	tempCacheDir(t)
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			inv := sampleInventory()
			inv.Targets[TargetKey("acme-prod", "eu-west-1")].Instances[0].Name = "acme-p-tools-" + string(rune('a'+i))
			errs <- inv.Save()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		// Windows can refuse a rename over a file another rename is replacing: the
		// save fails, but must never leave a broken file.
		if err != nil && runtime.GOOS != "windows" {
			t.Error(err)
		}
	}
	if got := Load(); len(got.Targets) != 2 {
		t.Errorf("cache unreadable after concurrent saves: %+v", got)
	}
}

func TestFresh(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	ttl := 15 * time.Minute
	cases := []struct {
		name string
		t    Target
		want bool
	}{
		{"never fetched", Target{}, false},
		{"just fetched", Target{FetchedAt: now}, true},
		{"one second before the TTL", Target{FetchedAt: now.Add(-ttl + time.Second)}, true},
		{"exactly at the TTL", Target{FetchedAt: now.Add(-ttl)}, false},
		{"past the TTL", Target{FetchedAt: now.Add(-time.Hour)}, false},
		{"fetched in the future (clock skew)", Target{FetchedAt: now.Add(time.Minute)}, true},
		{"error, 59 seconds ago", Target{Err: "x", ErrAt: now.Add(-59 * time.Second), FetchedAt: now.Add(-time.Hour)}, true},
		{"error, exactly one minute ago", Target{Err: "x", ErrAt: now.Add(-time.Minute)}, false},
		{"error, recent data does not count", Target{Err: "x", ErrAt: now.Add(-2 * time.Minute), FetchedAt: now}, false},
		{"error after Invalidate", Target{Err: "x"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.t.Fresh(ttl, now); got != c.want {
				t.Errorf("Fresh = %v, want %v", got, c.want)
			}
		})
	}
	if (&Target{FetchedAt: now}).Fresh(0, now) {
		t.Error("a zero TTL is never fresh")
	}
}

func TestPut(t *testing.T) {
	key := TargetKey("acme-prod", "eu-west-1")
	good := func() *Target {
		return &Target{Profile: "acme-prod", Region: "eu-west-1", FetchedAt: time.Now().Add(-time.Hour), AgentsKnown: true,
			Transcript: Transcript{Checked: true, Enabled: true, LogGroup: "/acme/ssm/sessions"},
			Instances:  []Instance{{ID: "i-1", Name: "acme-p-tools"}}}
	}

	t.Run("a success replaces everything", func(t *testing.T) {
		inv := &Inventory{Targets: map[string]*Target{}}
		inv.Put(good())
		fresh := &Target{Profile: "acme-prod", Region: "eu-west-1", FetchedAt: time.Now()}
		inv.Put(fresh)
		if got := inv.Targets[key]; got != fresh || len(got.Instances) != 0 || got.Transcript.Checked {
			t.Errorf("an empty success must replace the old data: %+v", got)
		}
	})

	t.Run("a first failure has nothing to keep", func(t *testing.T) {
		inv := &Inventory{Targets: map[string]*Target{}}
		inv.Put(&Target{Profile: "acme-prod", Region: "eu-west-1", Err: "denied", ErrKind: ErrDenied})
		if got := inv.Targets[key]; got.Err != "denied" || len(got.Instances) != 0 || !got.FetchedAt.IsZero() {
			t.Errorf("%+v", got)
		}
	})

	t.Run("failures in a row keep the last success", func(t *testing.T) {
		inv := &Inventory{Targets: map[string]*Target{}}
		first := good()
		inv.Put(first)
		inv.Put(&Target{Profile: "acme-prod", Region: "eu-west-1", Err: "expired", ErrKind: ErrSSOExpired, ErrAt: time.Now()})
		inv.Put(&Target{Profile: "acme-prod", Region: "eu-west-1", Err: "timeout", ErrKind: ErrNetwork, ErrAt: time.Now()})
		got := inv.Targets[key]
		if len(got.Instances) != 1 || !got.FetchedAt.Equal(first.FetchedAt) || !got.AgentsKnown || !got.Transcript.Enabled {
			t.Errorf("data lost after two failures: %+v", got)
		}
		if got.Err != "timeout" || got.ErrKind != ErrNetwork {
			t.Errorf("the last error must be reported: %+v", got)
		}
	})

	t.Run("a failure with a checked transcript keeps the new transcript", func(t *testing.T) {
		inv := &Inventory{Targets: map[string]*Target{}}
		inv.Put(good())
		inv.Put(&Target{Profile: "acme-prod", Region: "eu-west-1", Err: "x", Transcript: Transcript{Checked: true}})
		if got := inv.Targets[key]; got.Transcript.Enabled || !got.Transcript.Checked {
			t.Errorf("transcript %+v", got.Transcript)
		}
	})

	t.Run("pairs are independent", func(t *testing.T) {
		inv := &Inventory{Targets: map[string]*Target{}}
		inv.Put(good())
		inv.Put(&Target{Profile: "acme-prod", Region: "us-east-1", Err: "x"})
		if len(inv.Targets[key].Instances) != 1 || len(inv.Targets[TargetKey("acme-prod", "us-east-1")].Instances) != 0 {
			t.Error("a failure in one region touched another")
		}
	})
}

func TestInvalidate(t *testing.T) {
	now := time.Now()
	inv := &Inventory{Targets: map[string]*Target{}}
	inv.Put(&Target{Profile: "acme-prod", Region: "eu-west-1", FetchedAt: now})
	inv.Put(&Target{Profile: "acme-prod", Region: "us-east-1", Err: "x", ErrAt: now})
	inv.Put(&Target{Profile: "globex", Region: "eu-west-1", FetchedAt: now})
	inv.Invalidate("acme-prod")
	for _, k := range []string{TargetKey("acme-prod", "eu-west-1"), TargetKey("acme-prod", "us-east-1")} {
		if inv.Targets[k].Fresh(time.Hour, now) {
			t.Errorf("%s still fresh after Invalidate", k)
		}
	}
	if !inv.Targets[TargetKey("globex", "eu-west-1")].Fresh(time.Hour, now) {
		t.Error("Invalidate touched another profile")
	}
	inv.Invalidate("unknown")
}

func TestInstancesEdgeCases(t *testing.T) {
	mk := func(id, name, profile, region string) Instance {
		return Instance{ID: id, Name: name, Profile: profile, Region: region}
	}
	inv := &Inventory{Targets: map[string]*Target{
		TargetKey("acme-shop", "eu-west-1"): {Profile: "acme-shop", Region: "eu-west-1", Instances: []Instance{
			mk("i-1", "acme-p-shop-app", "acme-shop", "eu-west-1"),
		}},
		TargetKey("acme-shop", "us-east-1"): {Profile: "acme-shop", Region: "us-east-1", Instances: []Instance{
			// Same ID in another region: another instance, not a duplicate.
			mk("i-1", "acme-t-shop-app", "acme-shop", "us-east-1"),
		}},
		TargetKey("acme-shop-ro", "eu-west-1"): {Profile: "acme-shop-ro", Region: "eu-west-1", Instances: []Instance{
			{ID: "i-1", Name: "acme-p-shop-app", Profile: "acme-shop-ro", Region: "eu-west-1", AlsoVia: []string{"stale"}},
		}},
		TargetKey("acme-audit", "eu-west-1"): {Profile: "acme-audit", Region: "eu-west-1", Instances: []Instance{
			mk("i-1", "acme-p-shop-app", "acme-audit", "eu-west-1"),
		}},
	}}
	keys := []string{
		TargetKey("acme-shop", "us-east-1"),
		TargetKey("missing", "eu-west-1"),
		TargetKey("acme-shop", "eu-west-1"),
		TargetKey("acme-shop-ro", "eu-west-1"),
		TargetKey("acme-audit", "eu-west-1"),
	}
	for round := range 2 {
		got := inv.Instances(keys)
		if len(got) != 2 {
			t.Fatalf("round %d: %+v", round, got)
		}
		// Within a profile, regions are sorted.
		if got[0].Region != "eu-west-1" || got[1].Region != "us-east-1" {
			t.Errorf("round %d: order %s, %s", round, got[0].Region, got[1].Region)
		}
		// AlsoVia is rebuilt on every call, never accumulated, and the cached value is
		// ignored.
		if !reflect.DeepEqual(got[0].AlsoVia, []string{"acme-shop-ro", "acme-audit"}) || got[1].AlsoVia != nil {
			t.Errorf("round %d: also via %v / %v", round, got[0].AlsoVia, got[1].AlsoVia)
		}
	}
	if got := inv.Targets[TargetKey("acme-shop", "eu-west-1")].Instances[0].AlsoVia; got != nil {
		t.Errorf("the cache was modified: %v", got)
	}
	if got := inv.Instances(nil); len(got) != 0 {
		t.Errorf("no keys: %v", got)
	}
}

func TestInstanceHelpers(t *testing.T) {
	in := Instance{ID: "i-0aaa", Profile: "acme-prod", Region: "eu-west-1", State: "running", Agent: "Online"}
	if in.Key() != "acme-prod|eu-west-1|i-0aaa" || !in.Running() || !in.Online() {
		t.Errorf("%+v", in)
	}
	stopped := Instance{State: "stopped", Agent: "ConnectionLost"}
	if stopped.Running() || stopped.Online() {
		t.Errorf("%+v", stopped)
	}
	tg := Target{Profile: "acme-prod", Region: "eu-west-1"}
	if tg.Key() != TargetKey("acme-prod", "eu-west-1") || tg.Key() != "acme-prod|eu-west-1" {
		t.Errorf("target key %s", tg.Key())
	}
}

func TestTranscriptDestination(t *testing.T) {
	cases := []struct {
		tr   Transcript
		want string
	}{
		{Transcript{LogGroup: "/acme/ssm", Bucket: "acme-logs"}, "/acme/ssm and s3://acme-logs"},
		{Transcript{LogGroup: "/acme/ssm"}, "/acme/ssm"},
		{Transcript{Bucket: "acme-logs"}, "s3://acme-logs"},
		{Transcript{}, ""},
	}
	for _, c := range cases {
		if got := c.tr.Destination(); got != c.want {
			t.Errorf("%+v: %q, want %q", c.tr, got, c.want)
		}
	}
}

func TestParseSessionPrefsCases(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    Transcript
	}{
		{"S3 only, encrypted", `{"inputs":{"s3BucketName":"acme-logs","s3EncryptionEnabled":true}}`,
			Transcript{Checked: true, Enabled: true, Bucket: "acme-logs", Encrypted: true}},
		{"S3 only, not encrypted", `{"inputs":{"s3BucketName":"acme-logs"}}`,
			Transcript{Checked: true, Enabled: true, Bucket: "acme-logs"}},
		{"CloudWatch encryption flag without a log group", `{"inputs":{"cloudWatchEncryptionEnabled":true,"s3EncryptionEnabled":true}}`,
			Transcript{Checked: true}},
		{"both destinations", `{"inputs":{"s3BucketName":"acme-logs","cloudWatchLogGroupName":"/acme/ssm"}}`,
			Transcript{Checked: true, Enabled: true, Bucket: "acme-logs", LogGroup: "/acme/ssm"}},
		{"KMS key only", `{"inputs":{"kmsKeyId":"alias/acme"}}`,
			Transcript{Checked: true, Encrypted: true}},
		{"no inputs", `{"schemaVersion":"1.0"}`, Transcript{Checked: true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ParseSessionPrefs(c.content); got != c.want {
				t.Errorf("got %+v, want %+v", got, c.want)
			}
		})
	}
	for _, bad := range []string{"", "{", "not json", `{"inputs":"x"}`} {
		got := ParseSessionPrefs(bad)
		if got.Checked || got.Enabled || got.Err == "" {
			t.Errorf("%q: an unreadable document must be reported, not trusted: %+v", bad, got)
		}
	}
}

func TestLock(t *testing.T) {
	tempCacheDir(t)
	release, ok := Lock()
	if !ok {
		t.Fatal("first lock refused")
	}
	release2, ok2 := Lock()
	if ok2 {
		t.Fatal("second lock granted while the first is held")
	}
	// Releasing a refused lock must not free the lock of the holder.
	release2()
	if _, ok := Lock(); ok {
		t.Fatal("the release of a refused lock freed the holder's lock")
	}
	release()
	release3, ok3 := Lock()
	if !ok3 {
		t.Fatal("lock refused after release")
	}
	release3()
}

func TestLockStale(t *testing.T) {
	dir := tempCacheDir(t)
	os.MkdirAll(dir, 0o700)
	p := filepath.Join(dir, "refresh.lock")
	cases := []struct {
		age  time.Duration
		want bool
	}{
		{time.Minute, false},
		// A long refresh still holds its lock.
		{9 * time.Minute, false},
		{11 * time.Minute, true},
	}
	for _, c := range cases {
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-c.age)
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
		release, ok := Lock()
		if ok != c.want {
			t.Errorf("lock %s old: granted %v, want %v", c.age, ok, c.want)
		}
		release()
	}
}

func TestLockReleaseKeepsAnotherLock(t *testing.T) {
	dir := tempCacheDir(t)
	p := filepath.Join(dir, "refresh.lock")
	release, ok := Lock()
	if !ok {
		t.Fatal("lock refused")
	}
	b, err := os.ReadFile(p)
	if err != nil || len(b) == 0 {
		t.Fatalf("the lock file holds no token: %q (%v)", b, err)
	}
	// The refresh outlived lockStale: another process removed the lock and took its
	// own. The late release must leave that one alone.
	if err := os.WriteFile(p, []byte("token-of-another-process"), 0o600); err != nil {
		t.Fatal(err)
	}
	release()
	if b, err := os.ReadFile(p); err != nil || string(b) != "token-of-another-process" {
		t.Errorf("the release removed or changed another process's lock: %q (%v)", b, err)
	}
	if _, ok := Lock(); ok {
		t.Error("lock granted while another process holds it")
	}

	// Two locks never share a token.
	os.Remove(p)
	release, _ = Lock()
	first, _ := os.ReadFile(p)
	release()
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("release left the lock behind: %v", err)
	}
	release, _ = Lock()
	second, _ := os.ReadFile(p)
	release()
	if string(first) == string(second) {
		t.Errorf("same token twice: %q", first)
	}
}

func TestLockContention(t *testing.T) {
	tempCacheDir(t)
	var wg sync.WaitGroup
	var mu sync.Mutex
	granted := 0
	var releases []func()
	start := make(chan struct{})
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			release, ok := Lock()
			mu.Lock()
			defer mu.Unlock()
			if ok {
				granted++
				releases = append(releases, release)
			}
		}()
	}
	close(start)
	wg.Wait()
	if granted != 1 {
		t.Errorf("%d goroutines got the lock, want exactly 1", granted)
	}
	for _, r := range releases {
		r()
	}
}

func TestLockWithoutCacheFolder(t *testing.T) {
	// When the lock cannot be created at all, the refresh goes ahead rather than
	// never running.
	blocker := filepath.Join(t.TempDir(), "file")
	os.WriteFile(blocker, nil, 0o600)
	t.Setenv("XDG_CACHE_HOME", blocker)
	release, ok := Lock()
	if !ok {
		t.Error("refresh blocked by an unusable cache folder")
	}
	release()
}
