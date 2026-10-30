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
	Profile    string     `json:"profile"`
	Region     string     `json:"region"`
	FetchedAt  time.Time  `json:"fetched_at,omitzero"`
	Err        string     `json:"err,omitempty"`
	ErrKind    ErrKind    `json:"err_kind,omitempty"`
	ErrAt      time.Time  `json:"err_at,omitzero"`
	Transcript Transcript `json:"transcript"`
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
	inv := &Inventory{Version: version, Targets: map[string]*Target{}}
	b, err := os.ReadFile(Path())
	if err != nil {
		return inv
	}
	var disk Inventory
	// An unreadable cache, or one from another version, is rebuilt: it is only a cache.
	if json.Unmarshal(b, &disk) == nil && disk.Version == version && disk.Targets != nil {
		return &disk
	}
	return inv
}

// Save writes to a temporary file and then renames it, so two concurrent calls never
// leave a truncated file behind.
func (inv *Inventory) Save() error {
	if err := os.MkdirAll(CacheDir(), 0o700); err != nil {
		return err
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

// Put records the result of a refresh. On failure, the instances and the time of the
// last success are kept: a failed call must never wipe data that is still useful.
func (inv *Inventory) Put(t *Target) {
	if old, ok := inv.Targets[t.Key()]; ok && t.Err != "" {
		t.Instances = old.Instances
		t.FetchedAt = old.FetchedAt
		t.AgentsKnown = old.AgentsKnown
		if !t.Transcript.Checked {
			t.Transcript = old.Transcript
		}
	}
	inv.Targets[t.Key()] = t
}

// Invalidate forces the next refresh of every pair of a profile.
func (inv *Inventory) Invalidate(profile string) {
	for _, t := range inv.Targets {
		if t.Profile == profile {
			t.ErrAt = time.Time{}
			t.FetchedAt = time.Time{}
		}
	}
}

// Instances flattens the inventory for the selected pairs, in the order of keys.
// An instance seen by several profiles appears only once, under the first one.
func (inv *Inventory) Instances(keys []string) []Instance {
	var out []Instance
	seen := map[string]int{}
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
			if i, dup := seen[id]; dup {
				out[i].AlsoVia = append(out[i].AlsoVia, in.Profile)
				continue
			}
			in.AlsoVia = nil
			seen[id] = len(out)
			out = append(out, in)
		}
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

// lockStale is the age past which a refresh lock is considered abandoned (a process
// killed before its release). A refresh takes seconds: this leaves room for a slow
// network and many accounts.
const lockStale = 10 * time.Minute

// Lock keeps two background refreshes from running at the same time. The lock file
// holds a random token: release removes it only if it still holds that token, so a
// refresh that outlived lockStale never removes the lock another process took over.
func Lock() (release func(), ok bool) {
	p := filepath.Join(CacheDir(), "refresh.lock")
	if err := os.MkdirAll(CacheDir(), 0o700); err != nil {
		return func() {}, true
	}
	// Two processes can both find the lock stale and both take it: the cost is one
	// duplicate refresh, harmless since Save is atomic.
	if st, err := os.Stat(p); err == nil && time.Since(st.ModTime()) > lockStale {
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
		// A lock without its token could never be released: drop it, and refresh
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
