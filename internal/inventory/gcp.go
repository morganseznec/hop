package inventory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// GCP support is experimental: hop lists the Compute Engine instances of the projects
// gcloud's account can see, with the token gcloud gives, and opens shells through IAP
// with gcloud compute ssh.

const (
	// GCP is the profile of the one pair that covers every GCP project, and GCPScope its
	// region. Its instances are listed under gcp:<project>, in their zone.
	GCP      = "gcp"
	GCPScope = "*"

	gcpPrefix = GCP + ":"
)

// GCPEndpoints are the Google APIs hop calls. Tests point them to a local server.
var GCPEndpoints = struct{ Compute, ResourceManager string }{
	Compute:         "https://compute.googleapis.com",
	ResourceManager: "https://cloudresourcemanager.googleapis.com",
}

// GCPPair is the pair that covers every GCP project.
func GCPPair() Pair { return Pair{Profile: GCP, Region: GCPScope} }

// Key is the key of the pair's Target.
func (p Pair) Key() string { return TargetKey(p.Profile, p.Region) }

// GCPProfile is the profile under which the instances of a project are listed.
func GCPProfile(project string) string { return gcpPrefix + project }

// GCP reports whether the instance is a Compute Engine instance.
func (i Instance) GCP() bool { return strings.HasPrefix(i.Profile, gcpPrefix) }

// Project is the GCP project of a Compute Engine instance.
func (i Instance) Project() string { return strings.TrimPrefix(i.Profile, gcpPrefix) }

// FetchGCP lists the instances of the GCP projects that match the patterns, the
// projects in parallel. A project where Compute Engine is off, or whose instances the
// account may not list, is skipped: in an organization, most projects have no VM. It
// never returns an error: the Target carries it.
func FetchGCP(ctx context.Context, patterns []string) *Target {
	start := time.Now()
	c, err := newGCPClient(ctx)
	if err != nil {
		return failed(GCPPair(), start, err)
	}
	projects, err := c.projects(ctx, patterns)
	if err != nil {
		return failed(GCPPair(), start, err)
	}
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
		out      []Instance
	)
	sem := make(chan struct{}, 16)
	for _, project := range projects {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			ins, err := c.instances(ctx, project)
			mu.Lock()
			defer mu.Unlock()
			if err != nil && firstErr == nil {
				firstErr = fmt.Errorf("project %s: %w", project, err)
			}
			out = append(out, ins...)
		})
	}
	wg.Wait()
	if firstErr != nil {
		return failed(GCPPair(), start, firstErr)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Profile != b.Profile {
			return a.Profile < b.Profile
		}
		if a.Region != b.Region {
			return a.Region < b.Region
		}
		return a.Name < b.Name
	})
	return &Target{Profile: GCP, Region: GCPScope, FetchedAt: start, Instances: out}
}

// LookupGCP reads one Compute Engine instance again, by project, zone and name. found
// is false when it no longer exists.
func LookupGCP(ctx context.Context, cached Instance) (in Instance, found bool, err error) {
	c, err := newGCPClient(ctx)
	if err != nil {
		return cached, false, err
	}
	var g gcpInstance
	u := fmt.Sprintf("%s/compute/v1/projects/%s/zones/%s/instances/%s", GCPEndpoints.Compute,
		url.PathEscape(cached.Project()), url.PathEscape(cached.Region), url.PathEscape(cached.Name))
	if err := c.get(ctx, u, &g); err != nil {
		var api *gcpError
		if errors.As(err, &api) && api.Status == http.StatusNotFound {
			return cached, false, nil
		}
		return cached, false, err
	}
	return g.instance(cached.Project()), true, nil
}

type gcpClient struct{ token string }

var gcloudPrefix = regexp.MustCompile(`^ERROR: \([^)]*\) `)

// newGCPClient takes the access token of gcloud's active account, as gcloud compute
// ssh will use it: no other credentials to set up.
func newGCPClient(ctx context.Context) (*gcpClient, error) {
	gcloud, err := exec.LookPath("gcloud")
	if err != nil {
		return nil, errors.New("gcloud not found: install the Google Cloud CLI, then gcloud auth login")
	}
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, gcloud, "auth", "print-access-token")
	cmd.Stderr = &stderr
	// On Windows, gcloud.cmd runs Python as a child that the context does not kill: stop
	// waiting for its output anyway.
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil {
		// gcloud explains on its first line, after "ERROR: (gcloud.auth...)".
		msg, _, _ := strings.Cut(strings.TrimSpace(stderr.String()), "\n")
		msg = gcloudPrefix.ReplaceAllString(msg, "")
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("gcloud auth print-access-token: %s", msg)
	}
	token := strings.TrimSpace(string(out))
	if token == "" {
		return nil, errors.New("gcloud auth print-access-token gave no token: gcloud auth login")
	}
	return &gcpClient{token: token}, nil
}

// projects returns the projects to query: the patterns as they are when none has a
// wildcard, or else the active projects the account can see that match them.
func (c *gcpClient) projects(ctx context.Context, patterns []string) ([]string, error) {
	if !strings.ContainsAny(strings.Join(patterns, ""), "*?[") {
		var out []string
		for _, p := range patterns {
			if !slices.Contains(out, p) {
				out = append(out, p)
			}
		}
		return out, nil
	}
	var out []string
	q := url.Values{"filter": {"lifecycleState:ACTIVE"}, "pageSize": {"500"}}
	for {
		var page struct {
			Projects []struct {
				ProjectID string `json:"projectId"`
			} `json:"projects"`
			NextPageToken string `json:"nextPageToken"`
		}
		if err := c.get(ctx, GCPEndpoints.ResourceManager+"/v1/projects?"+q.Encode(), &page); err != nil {
			return nil, fmt.Errorf("listing the GCP projects: %w", err)
		}
		for _, p := range page.Projects {
			if id := clean(p.ProjectID); matchPattern(patterns, id) {
				out = append(out, id)
			}
		}
		if page.NextPageToken == "" {
			return out, nil
		}
		q.Set("pageToken", page.NextPageToken)
	}
}

func matchPattern(patterns []string, name string) bool {
	for _, p := range patterns {
		if ok, _ := path.Match(p, name); ok {
			return true
		}
	}
	return false
}

// instances lists the instances of a project in every zone. A project the account
// cannot read (Compute Engine off, or no permission) has none.
func (c *gcpClient) instances(ctx context.Context, project string) ([]Instance, error) {
	var out []Instance
	q := url.Values{"returnPartialSuccess": {"true"}, "maxResults": {"500"}}
	for {
		var page struct {
			Items map[string]struct {
				Instances []gcpInstance `json:"instances"`
			} `json:"items"`
			NextPageToken string   `json:"nextPageToken"`
			Unreachables  []string `json:"unreachables"`
		}
		u := fmt.Sprintf("%s/compute/v1/projects/%s/aggregated/instances?%s", GCPEndpoints.Compute, url.PathEscape(project), q.Encode())
		if err := c.get(ctx, u, &page); err != nil {
			var api *gcpError
			if errors.As(err, &api) && api.Status == http.StatusForbidden && !api.rateLimited() {
				return nil, nil
			}
			return nil, err
		}
		// A zone that did not answer would look empty: keep the instances seen before.
		if len(page.Unreachables) > 0 {
			return nil, fmt.Errorf("not reachable: %s", strings.Join(page.Unreachables, ", "))
		}
		for _, scope := range page.Items {
			for _, g := range scope.Instances {
				out = append(out, g.instance(project))
			}
		}
		if page.NextPageToken == "" {
			return out, nil
		}
		q.Set("pageToken", page.NextPageToken)
	}
}

func (c *gcpClient) get(ctx context.Context, u string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		e := &gcpError{Status: resp.StatusCode}
		var parsed struct {
			Error struct {
				Message string `json:"message"`
				Status  string `json:"status"`
				Errors  []struct {
					Reason string `json:"reason"`
				} `json:"errors"`
			} `json:"error"`
		}
		if json.Unmarshal(body, &parsed) == nil {
			e.Message = clean(parsed.Error.Message)
			e.Reason = parsed.Error.Status
			if len(parsed.Error.Errors) > 0 {
				e.Reason += " " + parsed.Error.Errors[0].Reason
			}
		}
		return e
	}
	return json.Unmarshal(body, v)
}

// gcpError is an error answer of a Google API.
type gcpError struct {
	Status  int
	Message string
	// Reason is the error status and reason, as Google gives them.
	Reason string
}

// rateLimited reports a quota or rate limit, which some Google APIs answer with 403:
// that project is not to be skipped as unreadable.
func (e *gcpError) rateLimited() bool {
	r := strings.ToLower(e.Reason)
	return strings.Contains(r, "ratelimit") || strings.Contains(r, "quota") || strings.Contains(r, "resource_exhausted")
}

func (e *gcpError) Error() string {
	if e.Status == http.StatusUnauthorized {
		return "the gcloud token was refused: gcloud auth login"
	}
	if e.Message == "" {
		return fmt.Sprintf("GCP API: HTTP %d", e.Status)
	}
	return fmt.Sprintf("GCP API: HTTP %d: %s", e.Status, e.Message)
}

type gcpInstance struct {
	ID                string            `json:"id"`
	Name              string            `json:"name"`
	Zone              string            `json:"zone"`
	Status            string            `json:"status"`
	Labels            map[string]string `json:"labels"`
	NetworkInterfaces []struct {
		NetworkIP string `json:"networkIP"`
	} `json:"networkInterfaces"`
}

// instance turns a Compute Engine instance into an inventory entry: its zone as the
// region, the env from its labels (env, environment...) like EC2 tags.
func (g gcpInstance) instance(project string) Instance {
	labels := map[string]string{}
	for k, v := range g.Labels {
		labels[clean(k)] = clean(v)
	}
	name := clean(g.Name)
	in := Instance{
		ID:       clean(g.ID),
		Name:     name,
		Profile:  GCPProfile(project),
		Region:   clean(path.Base(g.Zone)),
		State:    gcpState(g.Status),
		Env:      DeriveEnv(labels, name, project),
		Platform: "Compute Engine",
	}
	if len(g.NetworkInterfaces) > 0 {
		in.PrivateIP = clean(g.NetworkInterfaces[0].NetworkIP)
	}
	return in
}

// gcpState names the Compute Engine states as EC2 does: a stopped instance is
// TERMINATED there.
func gcpState(s string) string {
	switch s {
	case "RUNNING":
		return "running"
	case "TERMINATED":
		return "stopped"
	case "PROVISIONING", "STAGING":
		return "pending"
	}
	return strings.ToLower(clean(s))
}
