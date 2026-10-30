// Package inventory keeps the list of instances across every account, one
// profile/region pair at a time, with a disk cache.
//
// Each pair has its own refresh time and its own error. An unreachable account
// (expired SSO token, revoked keys) therefore does not empty the others, and its
// last known data stays visible, flagged as stale.
package inventory

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

type Instance struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Profile   string    `json:"profile"`
	Region    string    `json:"region"`
	PrivateIP string    `json:"private_ip,omitempty"`
	State     string    `json:"state"`
	Env       string    `json:"env,omitempty"`
	Platform  string    `json:"platform,omitempty"`
	Agent     string    `json:"agent,omitempty"` // Online, ConnectionLost, Inactive; empty: no SSM agent
	LastPing  time.Time `json:"last_ping,omitzero"`
	// Other profiles that see the same instance (two profiles on one account).
	AlsoVia []string `json:"also_via,omitempty"`
}

func (i Instance) Key() string { return i.Profile + "|" + i.Region + "|" + i.ID }

func (i Instance) Running() bool { return i.State == "running" }

func (i Instance) Online() bool { return i.Agent == "Online" }

// Hybrid reports whether the instance is a hybrid node (mi-): a server registered in
// SSM, outside EC2. Its name and address are reported by the node itself.
func (i Instance) Hybrid() bool { return strings.HasPrefix(i.ID, "mi-") }

// copyAgent sets the SSM agent fields of dst from src: the state, the last contact,
// and the platform when the agent reported one.
func copyAgent(dst *Instance, src Instance) {
	dst.Agent, dst.LastPing = src.Agent, src.LastPing
	if src.Agent != "" && src.Platform != "" {
		dst.Platform = src.Platform
	}
}

// Transcript describes the SSM-SessionManagerRunShell document settings of an account
// in one region: that document decides whether a shell opened through Session Manager
// is recorded.
type Transcript struct {
	Checked   bool   `json:"checked"`
	Enabled   bool   `json:"enabled"`
	LogGroup  string `json:"log_group,omitempty"`
	Bucket    string `json:"bucket,omitempty"`
	Encrypted bool   `json:"encrypted,omitempty"`
	Err       string `json:"err,omitempty"`
}

// Destination tells where the transcript goes, for display in the banner.
func (t Transcript) Destination() string {
	switch {
	case t.LogGroup != "" && t.Bucket != "":
		return t.LogGroup + " and s3://" + t.Bucket
	case t.LogGroup != "":
		return t.LogGroup
	case t.Bucket != "":
		return "s3://" + t.Bucket
	}
	return ""
}

type Target struct {
	Profile   string    `json:"profile"`
	Region    string    `json:"region"`
	FetchedAt time.Time `json:"fetched_at,omitzero"`
	Err       string    `json:"err,omitempty"`
	ErrKind   ErrKind   `json:"err_kind,omitempty"`
	ErrAt     time.Time `json:"err_at,omitzero"`
	// InvalidatedAt is the time of the last Invalidate. It orders the invalidation
	// against a refresh that another process saves later (see Save).
	InvalidatedAt time.Time  `json:"invalidated_at,omitzero"`
	Transcript    Transcript `json:"transcript"`
	// AgentsKnown reports whether the SSM agent states could be read. If not, an empty
	// Agent does not mean "no agent".
	AgentsKnown bool       `json:"agents_known"`
	Instances   []Instance `json:"instances"`
}

func TargetKey(profile, region string) string { return profile + "|" + region }

func (t *Target) Key() string { return TargetKey(t.Profile, t.Region) }

// Fresh reports whether the data is recent and the last attempt succeeded. A pair in
// error is retried after one minute, so an unreachable account is not hammered on
// every keystroke while a `hop login` is still picked up quickly.
func (t *Target) Fresh(ttl time.Duration, now time.Time) bool {
	if t.Err != "" {
		return now.Sub(t.ErrAt) < time.Minute
	}
	return !t.FetchedAt.IsZero() && now.Sub(t.FetchedAt) < ttl
}

// updated is the time of the latest event recorded for the pair: a refresh that
// succeeded or failed, or an Invalidate.
func (t *Target) updated() time.Time {
	u := t.FetchedAt
	for _, at := range []time.Time{t.ErrAt, t.InvalidatedAt} {
		if at.After(u) {
			u = at
		}
	}
	return u
}

type Inventory struct {
	Version int                `json:"version"`
	Targets map[string]*Target `json:"targets"`
}

const version = 1

func CacheDir() string {
	if d := os.Getenv("XDG_CACHE_HOME"); d != "" {
		return filepath.Join(d, "hop")
	}
	if runtime.GOOS == "windows" {
		if d, err := os.UserCacheDir(); err == nil {
			return filepath.Join(d, "hop")
		}
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cache", "hop")
}

func Path() string { return filepath.Join(CacheDir(), "inventory.json") }

func Load() *Inventory {
	if disk, ok := readCache(); ok {
		return disk
	}
	return &Inventory{Version: version, Targets: map[string]*Target{}}
}

// readCache reads the cache file. An unreadable cache, or one from another version,
// counts as no cache: it is only a cache, rebuilt by the next refresh.
func readCache() (*Inventory, bool) {
	b, err := os.ReadFile(Path())
	if err != nil {
		return nil, false
	}
	var disk Inventory
	if json.Unmarshal(b, &disk) == nil && disk.Version == version && disk.Targets != nil {
		return &disk, true
	}
	return nil, false
}

// Save writes the inventory, merged with what another hop saved since this one loaded
// the cache (a background refresh, a login in another terminal): for each pair, the
// most recent information wins (see Target.updated), so a slow refresh never writes
// back what a later refresh or a login replaced. The merged result is kept in memory
// too. The read-merge-write runs under a short-lived lock, separate from the refresh
// lock, and the file is written to a temporary file then renamed, so two concurrent
// calls never leave a truncated file behind.
func (inv *Inventory) Save() error {
	if err := os.MkdirAll(CacheDir(), 0o700); err != nil {
		return err
	}
	release := saveLock()
	defer release()
	if disk, ok := readCache(); ok {
		if inv.Targets == nil {
			inv.Targets = map[string]*Target{}
		}
		for k, t := range disk.Targets {
			if mine := inv.Targets[k]; t != nil && (mine == nil || t.updated().After(mine.updated())) {
				inv.Targets[k] = t
			}
		}
	}
	b, err := json.Marshal(inv)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(CacheDir(), "inventory-*.json")
	if err != nil {
		return err
	}
	_, err = tmp.Write(b)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), Path())
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}

// Put records the result of a refresh. A failed call must never wipe data that is
// still useful: on failure, the instances and the time of the last success are kept,
// and a transcript that could not be read keeps the one read before. When only the
// agent states could not be read (DescribeInstanceInformation is slow on an account
// with thousands of nodes), each instance keeps the agent state it had.
func (inv *Inventory) Put(t *Target) {
	if old, ok := inv.Targets[t.Key()]; ok {
		switch {
		case t.Err != "":
			t.Instances = old.Instances
			t.FetchedAt = old.FetchedAt
			t.AgentsKnown = old.AgentsKnown
		case !t.AgentsKnown:
			keepAgents(t, old)
		}
		if !t.Transcript.Checked && old.Transcript.Checked {
			t.Transcript = old.Transcript
		}
	}
	inv.Targets[t.Key()] = t
}

// keepAgents copies onto the instances of t, by instance ID, the agent fields of the
// previous inventory of the pair. Hybrid nodes, known only from SSM, stay listed.
// AgentsKnown stays false: an instance without agent data may still have an agent.
func keepAgents(t, old *Target) {
	prev := map[string]Instance{}
	for _, in := range old.Instances {
		if in.Agent != "" {
			prev[in.ID] = in
		}
	}
	seen := map[string]bool{}
	for i := range t.Instances {
		seen[t.Instances[i].ID] = true
		if p, ok := prev[t.Instances[i].ID]; ok {
			copyAgent(&t.Instances[i], p)
		}
	}
	for _, in := range old.Instances {
		if in.Hybrid() && !seen[in.ID] {
			t.Instances = append(t.Instances, in)
		}
	}
}

// Invalidate forces the next refresh of every pair of a profile.
func (inv *Inventory) Invalidate(profile string) {
	now := time.Now()
	for _, t := range inv.Targets {
		if t.Profile == profile {
			t.ErrAt = time.Time{}
			t.FetchedAt = time.Time{}
			t.InvalidatedAt = now
		}
	}
}

// Instances flattens the inventory for the selected pairs, in the order of keys. An
// instance seen by several profiles appears only once, under the best copy (see
// better), with the other profiles in AlsoVia.
func (inv *Inventory) Instances(keys []string) []Instance {
	type seenVia struct {
		in Instance
		t  *Target
	}
	var ids []string
	copies := map[string][]seenVia{}
	order := map[string]int{}
	for _, k := range keys {
		t, ok := inv.Targets[k]
		if !ok {
			continue
		}
		if _, ok := order[t.Profile]; !ok {
			order[t.Profile] = len(order)
		}
		for _, in := range t.Instances {
			id := in.Region + "|" + in.ID
			if _, dup := copies[id]; !dup {
				ids = append(ids, id)
			}
			copies[id] = append(copies[id], seenVia{in, t})
		}
	}
	out := make([]Instance, 0, len(ids))
	for _, id := range ids {
		cs := copies[id]
		best := 0
		for i := 1; i < len(cs); i++ {
			if better(cs[i].t, cs[best].t) {
				best = i
			}
		}
		in := cs[best].in
		in.AlsoVia = nil
		for i, c := range cs {
			if i != best {
				in.AlsoVia = append(in.AlsoVia, c.in.Profile)
			}
		}
		out = append(out, in)
	}
	sort.SliceStable(out, func(a, b int) bool {
		if pa, pb := order[out[a].Profile], order[out[b].Profile]; pa != pb {
			return pa < pb
		}
		if out[a].Region != out[b].Region {
			return out[a].Region < out[b].Region
		}
		return out[a].Name < out[b].Name
	})
	return out
}

// dedupMargin is how much more recent the data of a copy must be to replace a copy
// listed before it. The pairs of one refresh are queried over a minute or so: an
// instance must not switch profiles from one refresh to the next.
const dedupMargin = 5 * time.Minute

// better reports whether the copy of an instance seen through a beats the one seen
// through b, listed before it: a pair without error beats a pair in error, then data
// more recent by dedupMargin wins. Otherwise the earlier copy stays: SSO profiles
// come first in the keys.
func better(a, b *Target) bool {
	if (a.Err == "") != (b.Err == "") {
		return a.Err == ""
	}
	return a.FetchedAt.Sub(b.FetchedAt) > dedupMargin
}

// lockStale is the age past which a refresh lock is considered abandoned (a process
// killed before its release). A refresh takes seconds: this leaves room for a slow
// network and many accounts.
const lockStale = 10 * time.Minute

// Lock keeps two background refreshes from running at the same time.
func Lock() (release func(), ok bool) {
	if err := os.MkdirAll(CacheDir(), 0o700); err != nil {
		return func() {}, true
	}
	return tryLock(filepath.Join(CacheDir(), "refresh.lock"), lockStale)
}

// Save waits up to saveLockWait for another Save to finish, and takes over a save lock
// older than saveLockStale. A save takes milliseconds.
const (
	saveLockWait  = 2 * time.Second
	saveLockStale = 10 * time.Second
)

// saveLock serializes the read-merge-write of Save across processes. It never fails:
// past saveLockWait, or when no lock can be created, Save goes ahead without it rather
// than not saving at all.
func saveLock() (release func()) {
	p := filepath.Join(CacheDir(), "save.lock")
	deadline := time.Now().Add(saveLockWait)
	for {
		release, ok := tryLock(p, saveLockStale)
		if ok || time.Now().After(deadline) {
			return release
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// tryLock creates the lock file p, older than stale counting as abandoned. ok is false
// when another process holds it; when the file cannot be created at all, ok is true:
// the caller goes ahead rather than never. The file holds a random token: release
// removes it only if it still holds that token, so a holder that outlived stale never
// removes the lock another process took over.
func tryLock(p string, stale time.Duration) (release func(), ok bool) {
	// Two processes can both find the lock stale and both take it: the cost is one
	// duplicate refresh, or two saves that do not merge, harmless since each write is
	// atomic.
	if st, err := os.Stat(p); err == nil && time.Since(st.ModTime()) > stale {
		os.Remove(p)
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return func() {}, false
		}
		return func() {}, true
	}
	token := rand.Text()
	_, err = f.WriteString(token)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		// A lock without its token could never be released: drop it, and go ahead
		// anyway rather than never.
		os.Remove(p)
		return func() {}, true
	}
	return func() {
		if b, err := os.ReadFile(p); err == nil && string(b) == token {
			os.Remove(p)
		}
	}, true
}
